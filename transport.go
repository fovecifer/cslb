package cslb

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const defaultMaxBodyBuffer = 32 << 20 // 32MB

var (
	ErrNilRequest            = errors.New("cslb: nil request")
	ErrNilRequestURL         = errors.New("cslb: nil request URL")
	ErrNilRoundTripper       = errors.New("cslb: nil underlying transport")
	ErrInvalidRoundTrip      = errors.New("cslb: underlying transport returned nil response and nil error")
	ErrNilResponseBody       = errors.New("cslb: underlying transport returned a response with a nil body")
	ErrInvalidUpstreamConfig = errors.New("cslb: invalid upstream configuration")
)

// ---------- Transport: http.RoundTripper with client-side load balancing ----------

// Transport implements http.RoundTripper. It selects a backend using the
// configured load balancing algorithm and rewrites the request URL's Scheme
// and Host before forwarding.
type Transport struct {
	transport          http.RoundTripper
	upstreams          map[upstreamKey]*upstream
	timeout            time.Duration
	connectTimeout     time.Duration
	maxBodyBuffer      int64
	nextUpstream       NextUpstreamCondition
	retryNonIdempotent bool
	initErr            error
}

// NextUpstreamCondition determines whether a response should be considered
// a failure and the request should be retried on the next upstream server.
// It is called after each backend attempt. If it returns true, the Transport
// will retry on the next available peer when the request-method safety gate
// allows it. Sent POST, LOCK, and PATCH requests require
// WithNonIdempotentRetries.
//
// The resp parameter is nil when err is non-nil (connection error / timeout).
type NextUpstreamCondition func(resp *http.Response, err error) bool

// upstreamKey identifies an upstream by scheme+host.
type upstreamKey struct {
	scheme string
	host   string
}

// newUpstreamKey builds the lookup key for an origin. Scheme and host compare
// case-insensitively and an explicit default port is dropped, so
// http://API.example.com:80 matches the upstream http://api.example.com
// instead of silently bypassing it.
func newUpstreamKey(scheme, host string) upstreamKey {
	scheme = strings.ToLower(scheme)
	host = strings.ToLower(host)
	switch scheme {
	case "http":
		host = strings.TrimSuffix(host, ":80")
	case "https":
		host = strings.TrimSuffix(host, ":443")
	}
	return upstreamKey{scheme: scheme, host: host}
}

// upstream holds the balancer and peer-to-address mapping for one upstream group.
type upstream struct {
	balancer            Balancer
	backends            map[*Peer]*backendAddr
	hashFunc            func(*http.Request) string
	algo                AlgorithmType
	multipleUsablePeers bool
	sslName             string            // SNI override for TLS connections (proxy_ssl_name)
	transport           http.RoundTripper // per-upstream transport with custom TLS ServerName
}

// backendAddr stores the rewritten scheme and host for a backend peer.
type backendAddr struct {
	scheme string
	host   string
}

// AlgorithmType identifies a load balancing algorithm.
type AlgorithmType int

const (
	AlgoRoundRobin AlgorithmType = iota
	AlgoLeastConn
	AlgoIPHash
	AlgoHash
	AlgoRandom
	AlgoRandomTwo
	AlgoHashConsistent
)

// ---------- Option types ----------

// TransportOption configures a Transport.
type TransportOption func(*Transport)

// ServerOption configures a backend server inside an Upstream.
type ServerOption func(*Peer)

// ServerConfig defines one backend server within an upstream group.
// It maps closely to nginx's `server ...` entries inside an `upstream` block.
type ServerConfig struct {
	Address     string
	Weight      int
	Down        bool
	MaxConns    int
	MaxFails    int
	FailTimeout time.Duration
	Backup      bool

	explicit  serverFieldSet
	nilOption bool
}

// UpstreamConfig defines one upstream group and the servers behind it.
// It maps closely to nginx's `upstream ... { server ...; }` structure.
type UpstreamConfig struct {
	Pattern   string
	Servers   []ServerConfig
	Algorithm AlgorithmType
	HashFunc  func(*http.Request) string
	SSLName   string
}

// upstreamConfig collects configuration before building an upstream.
type upstreamConfig struct {
	defaultScheme string
	peers         []*Peer
	addrs         map[*Peer]*backendAddr
	algo          AlgorithmType
	hashFunc      func(*http.Request) string
	sslName       string
	err           error
}

// Upstream creates an explicit upstream configuration block.
func Upstream(pattern string, servers ...ServerConfig) UpstreamConfig {
	return UpstreamConfig{
		Pattern: pattern,
		Servers: append([]ServerConfig(nil), servers...),
	}
}

// Server creates an explicit backend server entry for use inside Upstream.
func Server(addr string, opts ...ServerOption) ServerConfig {
	peer := &Peer{Addr: addr}
	nilOption := false
	for _, opt := range opts {
		if opt == nil {
			nilOption = true
			continue
		}
		opt(peer)
	}

	return ServerConfig{
		Address:     addr,
		Weight:      peer.Weight,
		Down:        peer.Down,
		MaxConns:    peer.MaxConns,
		MaxFails:    peer.MaxFails,
		FailTimeout: peer.FailTimeout,
		Backup:      peer.Backup,
		explicit:    peer.explicit,
		nilOption:   nilOption,
	}
}

// RoundRobin selects smooth weighted round-robin for the upstream.
func (u UpstreamConfig) RoundRobin() UpstreamConfig {
	u.Algorithm = AlgoRoundRobin
	u.HashFunc = nil
	return u
}

// LeastConn selects least-connections for the upstream.
func (u UpstreamConfig) LeastConn() UpstreamConfig {
	u.Algorithm = AlgoLeastConn
	u.HashFunc = nil
	return u
}

// IPHash selects IP-hash for the upstream. In client-side use, this is only
// meaningful when the caller sets X-Real-IP, X-Forwarded-For, or RemoteAddr on
// outgoing requests; otherwise no client IP is available and selection falls
// back to round-robin. For ordinary client affinity, prefer Hash with an
// explicit request key.
func (u UpstreamConfig) IPHash() UpstreamConfig {
	u.Algorithm = AlgoIPHash
	u.HashFunc = nil
	return u
}

// Hash selects key-based hash for the upstream. keyFunc runs before the
// request body is prepared for sending, so it must not read req.Body.
func (u UpstreamConfig) Hash(keyFunc func(*http.Request) string) UpstreamConfig {
	u.Algorithm = AlgoHash
	u.HashFunc = keyFunc
	return u
}

// HashConsistent selects ketama-style consistent hashing for the upstream.
// Mirrors nginx's "hash <key> consistent". Compared to plain Hash, scaling
// or restarting the upstream pool only redistributes ~1/N of keys instead
// of nearly all of them. As with Hash, keyFunc must not read req.Body.
func (u UpstreamConfig) HashConsistent(keyFunc func(*http.Request) string) UpstreamConfig {
	u.Algorithm = AlgoHashConsistent
	u.HashFunc = keyFunc
	return u
}

// Random selects weighted random for the upstream.
func (u UpstreamConfig) Random() UpstreamConfig {
	u.Algorithm = AlgoRandom
	u.HashFunc = nil
	return u
}

// RandomTwo selects the Power of Two Choices algorithm for the upstream.
func (u UpstreamConfig) RandomTwo() UpstreamConfig {
	u.Algorithm = AlgoRandomTwo
	u.HashFunc = nil
	return u
}

// ProxySSLName sets the TLS server name used for both SNI and certificate
// verification on this upstream. It requires the underlying RoundTripper to
// be an *http.Transport.
func (u UpstreamConfig) ProxySSLName(name string) UpstreamConfig {
	u.SSLName = name
	return u
}

// ---------- Constructor ----------

// NewTransport creates a new load-balancing transport with the given options.
// Invalid configuration is retained on the returned transport and surfaced
// through Err() / RoundTrip(). Use NewTransportE
// to fail fast with an immediate error.
//
// Usage:
//
//	transport := cslb.NewTransport(
//	    cslb.WithUpstreams(
//	        cslb.Upstream("http://api.example.com",
//	            cslb.Server("http://10.0.0.1:8080", cslb.Weight(5)),
//	            cslb.Server("http://10.0.0.2:8080", cslb.Weight(3)),
//	            cslb.Server("http://10.0.0.3:8080", cslb.Backup()),
//	        ).LeastConn(),
//	    ),
//	    cslb.WithTimeout(5*time.Second),
//	    cslb.WithConnectTimeout(3*time.Second),
//	)
//	client := &http.Client{Transport: transport}
func NewTransport(opts ...TransportOption) *Transport {
	t, _ := NewTransportE(opts...)
	return t
}

// NewTransportE creates a new load-balancing transport and returns any
// configuration error immediately instead of panicking.
func NewTransportE(opts ...TransportOption) (*Transport, error) {
	t := &Transport{
		upstreams:     make(map[upstreamKey]*upstream),
		maxBodyBuffer: defaultMaxBodyBuffer,
	}
	for _, opt := range opts {
		if opt == nil {
			t.addInitError(errors.New("cslb: nil transport option"))
			continue
		}
		opt(t)
	}
	if t.transport == nil {
		dialer := &net.Dialer{
			Timeout:   t.connectTimeout,
			KeepAlive: 30 * time.Second,
		}
		if dialer.Timeout == 0 {
			dialer.Timeout = 30 * time.Second
		}
		t.transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	// Create per-upstream transports for upstreams with custom SSL names.
	// Each upstream with ProxySSLName gets a cloned transport whose
	// TLSClientConfig.ServerName is set, so TLS handshakes use the
	// specified hostname for SNI instead of the backend address.
	for _, ups := range t.upstreams {
		if ups.sslName == "" {
			continue
		}
		ht, ok := t.transport.(*http.Transport)
		if !ok {
			t.addInitError(errors.New("cslb: ProxySSLName requires the underlying transport to be *http.Transport"))
			continue
		}
		cloned := ht.Clone()
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.ServerName = ups.sslName
		ups.transport = cloned
	}

	return t, t.initErr
}

// CloseIdleConnections closes idle connections held by the underlying
// RoundTripper and by the per-upstream transports created for ProxySSLName.
// Connections currently in use are not interrupted. http.Client forwards its
// own CloseIdleConnections here.
func (t *Transport) CloseIdleConnections() {
	if t == nil {
		return
	}
	type closeIdler interface{ CloseIdleConnections() }
	if c, ok := t.transport.(closeIdler); ok {
		c.CloseIdleConnections()
	}
	for _, ups := range t.upstreams {
		if c, ok := ups.transport.(closeIdler); ok {
			c.CloseIdleConnections()
		}
	}
}

// Err returns the configuration error captured during construction, if any.
func (t *Transport) Err() error {
	if t == nil {
		return ErrNilRoundTripper
	}
	return t.initErr
}

func (t *Transport) addInitError(err error) {
	if err == nil {
		return
	}
	if t.initErr == nil {
		t.initErr = err
		return
	}
	t.initErr = errors.Join(t.initErr, err)
}

func (cfg *upstreamConfig) addError(err error) {
	if err == nil {
		return
	}
	if cfg.err == nil {
		cfg.err = err
		return
	}
	cfg.err = errors.Join(cfg.err, err)
}

// ---------- Transport options ----------

// WithRoundTripper sets the underlying http.RoundTripper used to send requests
// to backend servers. Defaults to a standard http.Transport if not set.
func WithRoundTripper(rt http.RoundTripper) TransportOption {
	return func(t *Transport) {
		if rt == nil {
			t.addInitError(ErrNilRoundTripper)
			return
		}
		t.transport = rt
	}
}

// WithTimeout sets the per-attempt request timeout. Each backend attempt gets
// its own timeout derived from the original request context.
func WithTimeout(d time.Duration) TransportOption {
	return func(t *Transport) {
		if d < 0 {
			t.addInitError(errors.New("cslb: attempt timeout must be non-negative"))
			return
		}
		t.timeout = d
	}
}

// WithConnectTimeout sets the TCP connect timeout for the default transport.
// Ignored if WithRoundTripper is used.
func WithConnectTimeout(d time.Duration) TransportOption {
	return func(t *Transport) {
		if d < 0 {
			t.addInitError(errors.New("cslb: connect timeout must be non-negative"))
			return
		}
		t.connectTimeout = d
	}
}

// WithMaxBodyBuffer sets the maximum request body size to buffer in memory.
// Bodies larger than this are spilled to a temporary file. Zero always spills
// non-empty bodies; the default is 32MB.
// This is not a total body-size limit; callers should enforce a separate limit
// for untrusted or very large request bodies.
func WithMaxBodyBuffer(size int64) TransportOption {
	return func(t *Transport) {
		if size < 0 {
			t.addInitError(errors.New("cslb: max body buffer must be non-negative"))
			return
		}
		t.maxBodyBuffer = size
	}
}

// WithNextUpstream sets a custom condition to determine whether an attempt is
// a candidate for retrying on the next backend. The non-idempotent request
// gate is applied after this condition.
// This corresponds to nginx's proxy_next_upstream directive.
//
// Example — only retry on 502/503/504:
//
//	cslb.WithNextUpstream(func(resp *http.Response, err error) bool {
//	    if err != nil {
//	        return true // connection error or timeout
//	    }
//	    switch resp.StatusCode {
//	    case 502, 503, 504:
//	        return true
//	    }
//	    return false
//	})
func WithNextUpstream(cond NextUpstreamCondition) TransportOption {
	return func(t *Transport) {
		if cond == nil {
			t.addInitError(errors.New("cslb: next upstream condition must not be nil"))
			return
		}
		t.nextUpstream = cond
	}
}

// WithNextUpstreamCodes configures which HTTP status codes are considered
// failures that trigger retrying on the next backend. Connection errors and
// timeouts always trigger a retry regardless of this setting.
//
// This corresponds to nginx's proxy_next_upstream http_xxx directives.
// If this option is not called, HTTP status codes do not trigger retry.
// Configured 403 and 404 responses can be retried but do not count as peer
// failures, matching nginx.
//
// Example — match nginx's "proxy_next_upstream error timeout http_502 http_503 http_504":
//
//	cslb.WithNextUpstreamCodes(502, 503, 504)
func WithNextUpstreamCodes(codes ...int) TransportOption {
	codeSet := make(map[int]bool, len(codes))
	for _, c := range codes {
		codeSet[c] = true
	}
	return WithNextUpstream(func(resp *http.Response, err error) bool {
		if err != nil {
			return true // connection error / timeout always retries
		}
		return codeSet[resp.StatusCode]
	})
}

// WithNonIdempotentRetries allows requests using POST, LOCK, or PATCH to be
// retried after they have been sent to an upstream server. By default these
// methods are not retried after sending, matching nginx's non_idempotent gate.
//
// Enabling this option can duplicate side effects. Use it only when the
// operation is safe to replay, for example when the application uses an
// idempotency key.
func WithNonIdempotentRetries() TransportOption {
	return func(t *Transport) { t.retryNonIdempotent = true }
}

// WithUpstreams registers one or more explicit upstream blocks.
// This is the recommended API when you want nginx-style readability:
//
//	cslb.NewTransport(
//	    cslb.WithUpstreams(
//	        cslb.Upstream("http://api.example.com",
//	            cslb.Server("http://10.0.0.1:8080", cslb.Weight(5)),
//	            cslb.Server("http://10.0.0.2:8080", cslb.Weight(3)),
//	            cslb.Server("http://10.0.0.3:8080", cslb.Backup()),
//	        ).LeastConn(),
//	    ),
//	)
func WithUpstreams(upstreams ...UpstreamConfig) TransportOption {
	return func(t *Transport) {
		for _, upstream := range upstreams {
			t.registerExplicitUpstream(upstream)
		}
	}
}

// ---------- Server options ----------

// Weight sets the server's weight. Higher weight means more traffic. It must
// be between 1 and 65536; see the README for per-upstream totals.
func Weight(w int) ServerOption {
	return func(p *Peer) {
		p.Weight = w
		p.explicit |= serverFieldWeight
	}
}

// MaxFails sets the number of failures before a server is temporarily disabled.
// An explicit zero disables temporary failure suppression, matching nginx.
func MaxFails(n int) ServerOption {
	return func(p *Peer) {
		p.MaxFails = n
		p.explicit |= serverFieldMaxFails
	}
}

// FailTimeout sets the duration a failed server stays disabled. An explicit
// zero is preserved; an omitted value defaults to 10 seconds.
func FailTimeout(d time.Duration) ServerOption {
	return func(p *Peer) {
		p.FailTimeout = d
		p.explicit |= serverFieldFailTimeout
	}
}

// MaxConns sets the maximum concurrent connections to a server.
func MaxConns(n int) ServerOption {
	return func(p *Peer) { p.MaxConns = n }
}

// Backup marks a server as backup, only used when all primary servers fail.
func Backup() ServerOption {
	return func(p *Peer) { p.Backup = true }
}

// Down marks a server as initially down.
func Down() ServerOption {
	return func(p *Peer) { p.Down = true }
}

func newUpstreamConfig(defaultScheme string) *upstreamConfig {
	return &upstreamConfig{
		defaultScheme: defaultScheme,
		addrs:         make(map[*Peer]*backendAddr),
	}
}

func (server ServerConfig) validate() error {
	var err error
	if server.nilOption {
		err = errors.Join(err, errors.New("cslb: nil server option"))
	}
	if server.Weight < 0 || (server.explicit.has(serverFieldWeight) && server.Weight == 0) {
		err = errors.Join(err, errors.New("weight must be greater than zero"))
	}
	if server.Weight > maxServerWeight {
		err = errors.Join(err, fmt.Errorf("weight must not exceed %d", maxServerWeight))
	}
	if server.MaxConns < 0 {
		err = errors.Join(err, errors.New("max_conns must be non-negative"))
	}
	if server.MaxFails < 0 {
		err = errors.Join(err, errors.New("max_fails must be non-negative"))
	}
	if server.FailTimeout < 0 {
		err = errors.Join(err, errors.New("fail_timeout must be non-negative"))
	}
	return err
}

func (cfg *upstreamConfig) addPeer(peer *Peer, addr string) {
	if addr == "" {
		cfg.addError(errors.New("cslb: backend address must not be empty"))
		return
	}

	scheme := cfg.defaultScheme
	raw := addr
	hasScheme := strings.Contains(addr, "://")
	if !hasScheme {
		// A bare host[:port] inherits the upstream scheme.
		raw = "//" + addr
	}
	u, err := parseOrigin(raw)
	if err != nil {
		cfg.addError(fmt.Errorf("cslb: invalid backend address: %q: %w", addr, err))
		return
	}
	if hasScheme {
		scheme = u.Scheme
	}

	cfg.peers = append(cfg.peers, peer)
	cfg.addrs[peer] = &backendAddr{scheme: scheme, host: u.Host}
}

// parseOrigin parses an upstream pattern or backend address, which must name
// an origin and nothing else. Anything more would be silently dropped when
// requests are rewritten, so a misconfiguration would only surface per request
// as a failing peer. A URL without a scheme ("//host:port") is accepted for
// backends that inherit the upstream scheme.
func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("missing host")
	}
	if u.User != nil {
		return nil, errors.New("user information is not supported")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("path is not supported")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("query is not supported")
	}
	if u.Fragment != "" {
		return nil, errors.New("fragment is not supported")
	}
	return u, nil
}

func (cfg *upstreamConfig) normalize() {
	if cfg.hashFunc != nil && cfg.algo == AlgoRoundRobin {
		cfg.algo = AlgoHash
	}
	if !supportedAlgorithm(cfg.algo) {
		cfg.addError(fmt.Errorf("unsupported load-balancing algorithm: %d", cfg.algo))
		return
	}
	if cfg.hashFunc != nil && cfg.algo != AlgoHash && cfg.algo != AlgoHashConsistent {
		cfg.addError(errors.New("cslb: hash key function can only be used with hash algorithm"))
	}
	if (cfg.algo == AlgoHash || cfg.algo == AlgoHashConsistent) && cfg.hashFunc == nil {
		cfg.addError(errors.New("cslb: hash key function must not be nil"))
	}
}

func supportedAlgorithm(algo AlgorithmType) bool {
	switch algo {
	case AlgoRoundRobin, AlgoLeastConn, AlgoIPHash, AlgoHash, AlgoRandom,
		AlgoRandomTwo, AlgoHashConsistent:
		return true
	default:
		return false
	}
}

func (t *Transport) registerExplicitUpstream(upstream UpstreamConfig) {
	cfg := newUpstreamConfig("")
	cfg.algo = upstream.Algorithm
	cfg.hashFunc = upstream.HashFunc
	cfg.sslName = upstream.SSLName

	for _, server := range upstream.Servers {
		if err := server.validate(); err != nil {
			cfg.addError(fmt.Errorf("server %q: %w", server.Address, err))
			continue
		}
		peer := &Peer{
			Addr:        server.Address,
			Weight:      server.Weight,
			Down:        server.Down,
			MaxConns:    server.MaxConns,
			MaxFails:    server.MaxFails,
			FailTimeout: server.FailTimeout,
			Backup:      server.Backup,
			explicit:    server.explicit,
		}
		cfg.addPeer(peer, server.Address)
	}

	t.registerUpstream(upstream.Pattern, cfg)
}

func (t *Transport) registerUpstream(pattern string, cfg *upstreamConfig) {
	u, err := parseOrigin(pattern)
	if err == nil && u.Scheme == "" {
		err = errors.New("missing scheme")
	}
	if err != nil {
		t.addInitError(fmt.Errorf("cslb: invalid upstream pattern: %q: %w", pattern, err))
		return
	}

	if cfg.defaultScheme == "" {
		cfg.defaultScheme = u.Scheme
		// Rebuild inherited backend schemes when explicit config was created before
		// the upstream pattern was known.
		for peer, addr := range cfg.addrs {
			if strings.Contains(peer.Addr, "://") {
				continue
			}
			addr.scheme = u.Scheme
		}
	}

	cfg.normalize()
	if cfg.err != nil {
		t.addInitError(fmt.Errorf("cslb: upstream %q: %w", pattern, cfg.err))
		return
	}
	if len(cfg.peers) == 0 {
		t.addInitError(fmt.Errorf("cslb: upstream %q has no backends", pattern))
		return
	}
	usablePeers := 0
	totalWeight := 0
	for _, peer := range cfg.peers {
		if !peer.Down {
			usablePeers++
		}
		// Each weight is at most maxServerWeight, so the sum cannot overflow
		// before the limit check below.
		totalWeight += max(peer.Weight, 1)
	}
	if usablePeers == 0 {
		t.addInitError(fmt.Errorf("cslb: upstream %q has no usable backends: all servers are down", pattern))
		return
	}
	weightLimit := maxUpstreamWeight
	if cfg.algo == AlgoHashConsistent {
		weightLimit = maxConsistentHashWeight
	}
	if totalWeight > weightLimit {
		t.addInitError(fmt.Errorf("cslb: upstream %q: total server weight %d exceeds %d", pattern, totalWeight, weightLimit))
		return
	}

	key := newUpstreamKey(u.Scheme, u.Host)
	if _, exists := t.upstreams[key]; exists {
		t.addInitError(fmt.Errorf("cslb: duplicate upstream for %s://%s", u.Scheme, u.Host))
		return
	}

	t.upstreams[key] = &upstream{
		balancer:            newBalancer(cfg.algo, cfg.peers),
		backends:            cfg.addrs,
		hashFunc:            cfg.hashFunc,
		algo:                cfg.algo,
		multipleUsablePeers: usablePeers > 1,
		sslName:             cfg.sslName,
	}
}

func newBalancer(algo AlgorithmType, peers []*Peer) Balancer {
	switch algo {
	case AlgoLeastConn:
		return NewLeastConn(peers)
	case AlgoIPHash:
		return NewIPHash(peers)
	case AlgoHash:
		return NewHash(peers)
	case AlgoHashConsistent:
		return NewHashConsistent(peers)
	case AlgoRandom:
		return NewRandom(peers)
	case AlgoRandomTwo:
		return NewRandomTwo(peers)
	default:
		return NewRoundRobin(peers)
	}
}

// ---------- RoundTrip ----------

// cancelCloser wraps a ReadCloser to cancel a context when the body is closed.
type cancelCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelCloser) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

type attemptDecision struct {
	retry  bool
	failed bool
	// neutral releases the peer without any health signal. It is distinct from
	// failed == false, which reports success and would run nginx's recovery
	// path, clearing the failure counter of a still-recovering peer.
	neutral bool
}

// RoundTrip implements http.RoundTripper. It matches the request against
// configured upstreams, selects a backend, rewrites the URL, and forwards the
// request. Transport errors are retried by default; HTTP status codes require
// WithNextUpstream or WithNextUpstreamCodes. Sent POST, LOCK, and PATCH
// requests require WithNonIdempotentRetries before they can be replayed. When
// all peers are exhausted the last attempt's (resp, err) is returned; when no
// peer is ever available, ErrNoPeerAvailable is returned.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil {
		return failRoundTrip(req, ErrNilRoundTripper)
	}
	if t.initErr != nil {
		return failRoundTrip(req, t.initErr)
	}
	if req == nil {
		return failRoundTrip(nil, ErrNilRequest)
	}
	if req.URL == nil {
		return failRoundTrip(req, ErrNilRequestURL)
	}
	if t.transport == nil {
		return failRoundTrip(req, ErrNilRoundTripper)
	}
	if err := req.Context().Err(); err != nil {
		return failRoundTrip(req, err)
	}

	ups, ok := t.upstreams[newUpstreamKey(req.URL.Scheme, req.URL.Host)]
	if !ok {
		return t.roundTrip(t.transport, req)
	}

	baseReq := req.Clone(req.Context())

	// Create picker based on algorithm
	pk := t.newPicker(ups, baseReq)
	if pk == nil {
		return failRoundTrip(req, ErrInvalidUpstreamConfig)
	}

	// Prepare the body for replay only when a later attempt may need it.
	getBody, cleanup, err := t.prepareBody(baseReq, t.requestBodyMayNeedReplay(ups, baseReq))
	if err != nil {
		return nil, err
	}
	var stream io.Reader
	if getBody == nil && baseReq.Body != http.NoBody {
		stream = baseReq.Body
	}
	body := newRequestBody(getBody, stream, cleanup)

	resp, err := t.forward(baseReq, ups, pk, body)
	if resp == nil {
		body.releaseNow()
	} else {
		// The body may still be streaming to the backend that produced resp; it
		// is released once the underlying RoundTripper closes it.
		body.release()
	}
	return resp, err
}

// forward runs the attempt loop for a request matched to ups.
func (t *Transport) forward(baseReq *http.Request, ups *upstream, pk Picker, body *requestBody) (*http.Response, error) {
	reqCtx := baseReq.Context()

	// Carries the most recent attempt across iterations so that, when peers
	// are exhausted, the caller sees the same (resp, err) the last upstream
	// produced — mirroring nginx's ngx_http_upstream_next, which overwrites
	// u->state->status on every retry and finalizes with whatever was last.
	var lastResp *http.Response
	var lastErr error
	closeLast := func() {
		if lastResp != nil {
			lastResp.Body.Close()
			lastResp = nil
		}
	}

	for {
		if err := reqCtx.Err(); err != nil {
			closeLast()
			return nil, err
		}

		peer := pk.Pick()
		if peer == nil {
			if lastResp != nil || lastErr != nil {
				return lastResp, lastErr
			}
			return nil, ErrNoPeerAvailable
		}

		addr := ups.backends[peer]
		if addr == nil {
			doneNeutral(pk, peer)
			closeLast()
			return nil, fmt.Errorf("cslb: selected peer %q has no backend address", peer.Addr)
		}

		// Committing to a new attempt — drop the previous failed attempt's body.
		closeLast()
		lastErr = nil

		attemptReq := baseReq.Clone(reqCtx)

		// Preserve original Host header
		if attemptReq.Host == "" {
			attemptReq.Host = baseReq.URL.Host
		}

		// Rewrite to selected backend
		attemptReq.URL.Scheme = addr.scheme
		attemptReq.URL.Host = addr.host

		// Give this attempt its own body
		if !body.empty() {
			attemptBody, err := body.next()
			if err != nil {
				doneNeutral(pk, peer)
				return nil, err
			}
			attemptReq.Body = attemptBody
			attemptReq.GetBody = body.getBody()
		}

		// Apply per-attempt timeout
		var cancel context.CancelFunc
		if t.timeout > 0 {
			var ctx context.Context
			ctx, cancel = context.WithTimeout(reqCtx, t.timeout)
			attemptReq = attemptReq.WithContext(ctx)
		}

		// Use per-upstream transport (with custom SNI) if available
		rt := t.transport
		if ups.transport != nil {
			rt = ups.transport
		}

		attemptReq, requestSent := traceRequestSent(attemptReq)
		resp, err := t.roundTrip(rt, attemptReq)

		// Handle context cancellation
		if err != nil {
			if cancel != nil {
				cancel()
			}
		} else if cancel != nil {
			resp.Body = &cancelCloser{ReadCloser: resp.Body, cancel: cancel}
		}

		// A caller cancellation is not an upstream failure. Do not fan the
		// canceled request out across the remaining peers or penalize them.
		if err != nil && reqCtx.Err() != nil {
			doneNeutral(pk, peer)
			// roundTrip guarantees a non-nil Body on any non-nil response.
			if resp != nil {
				resp.Body.Close()
			}
			return nil, reqCtx.Err()
		}

		decision := t.decideAttempt(
			attemptReq,
			resp,
			err,
			requestWasSent(rt, requestSent, resp, err),
		)
		if decision.neutral {
			doneNeutral(pk, peer)
		} else {
			pk.Done(peer, decision.failed)
		}

		if !decision.retry {
			return resp, err
		}

		// Hold onto this attempt — it becomes the return value if the next
		// Pick() turns up empty. The body is closed at the start of the next
		// iteration, or by the caller if this is the last attempt.
		lastResp = resp
		lastErr = err
	}
}

// isRoundTripperContractError reports whether err describes the configured
// RoundTripper breaking its own contract, rather than an upstream server
// misbehaving. These errors say nothing about peer health and are not
// retryable: the next peer would go through the same RoundTripper.
func isRoundTripperContractError(err error) bool {
	return errors.Is(err, ErrNilResponseBody) ||
		errors.Is(err, ErrInvalidRoundTrip) ||
		errors.Is(err, ErrNilRoundTripper)
}

func failRoundTrip(req *http.Request, err error) (*http.Response, error) {
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, err
}

func doneNeutral(picker Picker, peer *Peer) {
	if neutral, ok := picker.(interface{ doneNeutral(*Peer) }); ok {
		neutral.doneNeutral(peer)
		return
	}
	// All built-in pickers implement doneNeutral. Keep a safe fallback for a
	// future external Picker implementation so its connection count is released.
	picker.Done(peer, false)
}

// shouldRetry determines whether the request should be retried on the next peer.
func (t *Transport) shouldRetry(resp *http.Response, err error) bool {
	if t.nextUpstream != nil {
		return t.nextUpstream(resp, err)
	}
	// nginx defaults proxy_next_upstream to error and timeout. RoundTripper
	// exposes both as errors; HTTP status codes are opt-in.
	return err != nil
}

func (t *Transport) decideAttempt(req *http.Request, resp *http.Response, err error, sent bool) attemptDecision {
	// A contract violation by the configured RoundTripper is a local defect. It
	// says nothing about upstream health, so the peer is released neutrally:
	// neither as a failure, nor as the success that would clear a recovering
	// peer's counter. Whether to try the next peer is left to the retry
	// condition below, so a custom WithNextUpstream is still honored.
	neutral := isRoundTripperContractError(err)

	if !t.shouldRetry(resp, err) {
		// nginx reports every error and timeout to the peer as a failure,
		// whether or not proxy_next_upstream lets the request move on.
		return attemptDecision{failed: err != nil && !neutral, neutral: neutral}
	}

	blockedNonIdempotent := sent && isNonIdempotentMethod(req.Method) && !t.retryNonIdempotent
	decision := attemptDecision{
		retry:   !blockedNonIdempotent,
		failed:  !neutral,
		neutral: neutral,
	}

	if err == nil && resp != nil {
		// nginx can pass 403/404 to the next peer when configured, but these
		// statuses never count as an unsuccessful attempt for peer health.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			decision.failed = false
		}

		// For response-status retries nginx applies the non-idempotent gate
		// before calling the peer failure path. Transport errors reach that
		// path first and therefore still count as failures.
		if blockedNonIdempotent {
			decision.failed = false
		}
	}

	return decision
}

func isNonIdempotentMethod(method string) bool {
	switch method {
	case http.MethodPost, "LOCK", http.MethodPatch:
		return true
	default:
		return false
	}
}

// requestBodyMayNeedReplay reports whether the request can reach another peer
// after its body has started being sent. A custom next-upstream condition is
// arbitrary and cannot be predicted before an attempt, so only the peer count
// and the non-idempotent method gate are safe static exclusions.
func (t *Transport) requestBodyMayNeedReplay(ups *upstream, req *http.Request) bool {
	if ups == nil || !ups.multipleUsablePeers {
		return false
	}
	return !isNonIdempotentMethod(req.Method) || t.retryNonIdempotent
}

func traceRequestSent(req *http.Request) (*http.Request, *atomic.Bool) {
	sent := &atomic.Bool{}
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() {
			sent.Store(true)
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			sent.Store(true)
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), sent
}

func requestWasSent(rt http.RoundTripper, sent *atomic.Bool, resp *http.Response, err error) bool {
	if resp != nil || sent.Load() {
		return true
	}
	if err == nil {
		return false
	}

	// A standard http.Transport reports writes through httptrace, so an error
	// before either write hook means the request was not sent. Arbitrary
	// RoundTrippers have no equivalent contract; treat their errors as sent to
	// avoid duplicating side effects.
	_, standardTransport := rt.(*http.Transport)
	return !standardTransport
}

func (t *Transport) roundTrip(rt http.RoundTripper, req *http.Request) (*http.Response, error) {
	if rt == nil {
		return failRoundTrip(req, ErrNilRoundTripper)
	}

	resp, err := rt.RoundTrip(req)
	if resp == nil && err == nil {
		return failRoundTrip(req, ErrInvalidRoundTrip)
	}
	if resp != nil && resp.Body == nil {
		// http.RoundTripper requires a non-nil Body, but a custom or test
		// implementation may still return one. Reject it here, the single point
		// every response passes through, so retry bookkeeping, the per-attempt
		// cancelCloser, and the caller can all Close unconditionally.
		//
		// Substituting http.NoBody instead would let a broken RoundTripper read
		// as a legitimate empty response, so the defect is surfaced rather than
		// papered over.
		return failRoundTrip(req, ErrNilResponseBody)
	}
	return resp, err
}

// newPicker creates a per-request picker, using algorithm-specific constructors
// for IPHash and Hash that require request context.
func (t *Transport) newPicker(ups *upstream, req *http.Request) Picker {
	if ups == nil || ups.balancer == nil {
		return nil
	}

	switch ups.algo {
	case AlgoIPHash:
		if h, ok := ups.balancer.(*IPHash); ok {
			return h.NewPickerForIP(clientIP(req))
		}
	case AlgoHash:
		if h, ok := ups.balancer.(*Hash); ok && ups.hashFunc != nil {
			return h.NewPickerForKey(ups.hashFunc(req))
		}
	case AlgoHashConsistent:
		if h, ok := ups.balancer.(*HashConsistent); ok && ups.hashFunc != nil {
			return h.NewPickerForKey(ups.hashFunc(req))
		}
	}
	return ups.balancer.NewPicker()
}

// prepareBody makes the request body available to each permitted attempt.
// Returns a getBody function, an optional cleanup function, and any error.
func (t *Transport) prepareBody(req *http.Request, replayPossible bool) (getBody func() (io.ReadCloser, error), cleanup func(), err error) {
	if req.GetBody != nil {
		if req.Body == nil {
			return req.GetBody, nil, nil
		}
		initialBody := req.Body
		return req.GetBody, func() { initialBody.Close() }, nil
	}

	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil, nil
	}

	// Optimization: seekable body (e.g., *os.File)
	if seeker, ok := req.Body.(io.ReadSeeker); ok {
		initialBody := req.Body
		startPos, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			initialBody.Close()
			return nil, nil, err
		}
		// Best-effort: when SeekEnd works we know how many bytes will actually
		// be read on each retry, and can both clamp a stale ContentLength and
		// detect an at/past-EOF seeker. Leave -1 (chunked) or any value
		// already <= remaining alone, so we do not change request framing or
		// forward more bytes than the caller declared.
		end := int64(-1)
		if endPos, seekErr := seeker.Seek(0, io.SeekEnd); seekErr == nil {
			if _, err := seeker.Seek(startPos, io.SeekStart); err != nil {
				initialBody.Close()
				return nil, nil, err
			}
			end = endPos
			remaining := endPos - startPos
			if remaining < 0 {
				remaining = 0
			}
			switch {
			case remaining == 0 && req.ContentLength > 0:
				// Stale positive ContentLength but body has nothing left.
				// Hand back http.NoBody so net/http emits an explicit
				// Content-Length: 0 instead of switching to chunked once the
				// caller's positive value gets clamped to 0.
				req.ContentLength = 0
				return func() (io.ReadCloser, error) { return http.NoBody, nil },
					func() { initialBody.Close() }, nil
			case remaining > 0 && req.ContentLength > remaining:
				// Caller's ContentLength was assuming the seeker started at 0;
				// clamp down so we do not declare more bytes than we send.
				req.ContentLength = remaining
			}
			// Otherwise (ContentLength == -1 chunked, or 0/positive already
			// <= remaining) leave framing untouched.
		}
		src := &replaySource{rs: seeker}
		return func() (io.ReadCloser, error) {
			return src.reader(startPos, end), nil
		}, func() { initialBody.Close() }, nil
	}

	if !replayPossible {
		// Do not drain a genuinely streaming body before the only attempt that
		// can consume it. Returning no getBody makes RoundTrip share the unread
		// stream between attempts without letting an attempt close it, so it
		// remains available after a standard *http.Transport connection failure
		// that happened before any request bytes were written. Such a failure is
		// the only path that can reach another peer without replayPossible. A
		// sent attempt is terminal either because there is no other usable peer
		// or because of the non-idempotent gate; errors from custom
		// RoundTrippers are conservatively treated as sent.
		initialBody := req.Body
		return nil, func() { initialBody.Close() }, nil
	}

	// Buffer body: memory or spill to temp file
	b, f, err := t.readBody(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, nil, err
	}

	if f != nil {
		return prepareSpilledBody(req, f)
	}

	// Fits in memory
	req.ContentLength = int64(len(b))
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}, nil, nil
}

// bodyTempFile is the subset of os.File used by body replay.
type bodyTempFile interface {
	io.ReadWriteSeeker
	io.Closer
	Stat() (os.FileInfo, error)
	Name() string
}

type bodyTempFileFactory func() (bodyTempFile, error)

func prepareSpilledBody(req *http.Request, f bodyTempFile) (getBody func() (io.ReadCloser, error), cleanup func(), err error) {
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	size := stat.Size()
	req.ContentLength = size
	src := &replaySource{rs: f}
	return func() (io.ReadCloser, error) {
			return src.reader(0, size), nil
		}, func() {
			name := f.Name()
			f.Close()
			os.Remove(name)
		}, nil
}

func createBodyTempFile() (bodyTempFile, error) {
	return os.CreateTemp("", "cslb-body-*")
}

// readBody reads a request body, buffering small bodies in memory and
// spilling large ones to a temporary file.
func (t *Transport) readBody(r io.Reader) ([]byte, bodyTempFile, error) {
	return t.readBodyWithFactory(r, createBodyTempFile)
}

func (t *Transport) readBodyWithFactory(r io.Reader, createFile bodyTempFileFactory) ([]byte, bodyTempFile, error) {
	limit := t.maxBodyBuffer

	lr := io.LimitReader(r, limit+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, nil, err
	}

	if int64(len(b)) <= limit {
		return b, nil, nil
	}

	// Spill to temp file
	f, err := createFile()
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	return nil, f, nil
}

// clientIP extracts the client IP from a request, checking X-Real-IP,
// X-Forwarded-For, and RemoteAddr in order. Client-side outgoing requests
// usually do not have these values unless the caller sets them explicitly; nil
// means IPHash will fall back to round-robin selection.
func clientIP(req *http.Request) net.IP {
	if value := req.Header.Get("X-Real-IP"); value != "" {
		if ip := net.ParseIP(strings.TrimSpace(value)); ip != nil {
			return ip
		}
	}
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := net.ParseIP(strings.TrimSpace(xff)); ip != nil {
			return ip
		}
	}
	if req.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(req.RemoteAddr)
		if err != nil {
			host = req.RemoteAddr
		}
		if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
			return ip
		}
	}
	return nil
}
