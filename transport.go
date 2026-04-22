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
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultMaxBodyBuffer = 32 << 20 // 32MB

var (
	ErrNilRequest            = errors.New("cslb: nil request")
	ErrNilRequestURL         = errors.New("cslb: nil request URL")
	ErrNilRoundTripper       = errors.New("cslb: nil underlying transport")
	ErrInvalidRoundTrip      = errors.New("cslb: underlying transport returned nil response and nil error")
	ErrInvalidUpstreamConfig = errors.New("cslb: invalid upstream configuration")
)

// ---------- Transport: http.RoundTripper with client-side load balancing ----------

// Transport implements http.RoundTripper. It selects a backend using the
// configured load balancing algorithm and rewrites the request URL's Scheme
// and Host before forwarding.
type Transport struct {
	transport      http.RoundTripper
	upstreams      map[upstreamKey]*upstream
	timeout        time.Duration
	connectTimeout time.Duration
	maxBodyBuffer  int64
	nextUpstream   NextUpstreamCondition
	initErr        error
}

// NextUpstreamCondition determines whether a response should be considered
// a failure and the request should be retried on the next upstream server.
// It is called after each backend attempt. If it returns true, the Transport
// will retry on the next available peer.
//
// The resp parameter is nil when err is non-nil (connection error / timeout).
type NextUpstreamCondition func(resp *http.Response, err error) bool

// upstreamKey identifies an upstream by scheme+host.
type upstreamKey struct {
	scheme string
	host   string
}

// upstream holds the balancer and peer-to-address mapping for one upstream group.
type upstream struct {
	balancer  Balancer
	backends  map[*Peer]*backendAddr
	hashFunc  func(*http.Request) string
	algo      AlgorithmType
	sslName   string            // SNI override for TLS connections (proxy_ssl_name)
	transport http.RoundTripper // per-upstream transport with custom TLS ServerName
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
	for _, opt := range opts {
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

// IPHash selects IP-hash for the upstream.
func (u UpstreamConfig) IPHash() UpstreamConfig {
	u.Algorithm = AlgoIPHash
	u.HashFunc = nil
	return u
}

// Hash selects key-based hash for the upstream.
func (u UpstreamConfig) Hash(keyFunc func(*http.Request) string) UpstreamConfig {
	u.Algorithm = AlgoHash
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

// ProxySSLName sets the TLS SNI override for the upstream.
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
	return func(t *Transport) { t.transport = rt }
}

// WithTimeout sets the per-attempt request timeout. Each backend attempt gets
// its own timeout derived from the original request context.
func WithTimeout(d time.Duration) TransportOption {
	return func(t *Transport) { t.timeout = d }
}

// WithConnectTimeout sets the TCP connect timeout for the default transport.
// Ignored if WithRoundTripper is used.
func WithConnectTimeout(d time.Duration) TransportOption {
	return func(t *Transport) { t.connectTimeout = d }
}

// WithMaxBodyBuffer sets the maximum request body size to buffer in memory.
// Bodies larger than this are spilled to a temporary file. Default is 32MB.
func WithMaxBodyBuffer(size int64) TransportOption {
	return func(t *Transport) { t.maxBodyBuffer = size }
}

// WithNextUpstream sets a custom condition to determine whether a response
// is a failure that should trigger retrying on the next backend.
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
	return func(t *Transport) { t.nextUpstream = cond }
}

// WithNextUpstreamCodes configures which HTTP status codes are considered
// failures that trigger retrying on the next backend. Connection errors and
// timeouts always trigger a retry regardless of this setting.
//
// This corresponds to nginx's proxy_next_upstream http_xxx directives.
// Default behavior (if not called): all 5xx status codes trigger retry.
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

// Weight sets the server's weight. Higher weight means more traffic.
func Weight(w int) ServerOption {
	return func(p *Peer) { p.Weight = w }
}

// MaxFails sets the number of failures before a server is temporarily disabled.
func MaxFails(n int) ServerOption {
	return func(p *Peer) { p.MaxFails = n }
}

// FailTimeout sets the duration a failed server stays disabled.
func FailTimeout(d time.Duration) ServerOption {
	return func(p *Peer) { p.FailTimeout = d }
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

func (cfg *upstreamConfig) addPeer(peer *Peer, addr string) {
	if addr == "" {
		cfg.addError(errors.New("cslb: backend address must not be empty"))
		return
	}

	scheme := cfg.defaultScheme
	host := addr

	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil || u.Scheme == "" || u.Host == "" {
			cfg.addError(fmt.Errorf("cslb: invalid backend address: %q", addr))
			return
		}
		scheme = u.Scheme
		host = u.Host
	}
	if host == "" {
		cfg.addError(fmt.Errorf("cslb: invalid backend address: %q", addr))
		return
	}

	cfg.peers = append(cfg.peers, peer)
	cfg.addrs[peer] = &backendAddr{scheme: scheme, host: host}
}

func (cfg *upstreamConfig) normalize() {
	if cfg.hashFunc != nil && cfg.algo == AlgoRoundRobin {
		cfg.algo = AlgoHash
	}
	if cfg.hashFunc != nil && cfg.algo != AlgoHash {
		cfg.addError(errors.New("cslb: hash key function can only be used with hash algorithm"))
	}
	if cfg.algo == AlgoHash && cfg.hashFunc == nil {
		cfg.addError(errors.New("cslb: hash key function must not be nil"))
	}
}

func (t *Transport) registerExplicitUpstream(upstream UpstreamConfig) {
	cfg := newUpstreamConfig("")
	cfg.algo = upstream.Algorithm
	cfg.hashFunc = upstream.HashFunc
	cfg.sslName = upstream.SSLName

	for _, server := range upstream.Servers {
		peer := &Peer{
			Addr:        server.Address,
			Weight:      server.Weight,
			Down:        server.Down,
			MaxConns:    server.MaxConns,
			MaxFails:    server.MaxFails,
			FailTimeout: server.FailTimeout,
			Backup:      server.Backup,
		}
		cfg.addPeer(peer, server.Address)
	}

	t.registerUpstream(upstream.Pattern, cfg)
}

func (t *Transport) registerUpstream(pattern string, cfg *upstreamConfig) {
	u, err := url.Parse(pattern)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.addInitError(fmt.Errorf("cslb: invalid upstream pattern: %q", pattern))
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

	key := upstreamKey{scheme: u.Scheme, host: u.Host}
	t.upstreams[key] = &upstream{
		balancer: newBalancer(cfg.algo, cfg.peers),
		backends: cfg.addrs,
		hashFunc: cfg.hashFunc,
		algo:     cfg.algo,
		sslName:  cfg.sslName,
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

// RoundTrip implements http.RoundTripper. It matches the request against
// configured upstreams, selects a backend, rewrites the URL, and forwards
// the request. Failed attempts are retried on other backends.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil {
		return nil, ErrNilRoundTripper
	}
	if t.initErr != nil {
		return nil, t.initErr
	}
	if req == nil {
		return nil, ErrNilRequest
	}
	if req.URL == nil {
		return nil, ErrNilRequestURL
	}
	if t.transport == nil {
		return nil, ErrNilRoundTripper
	}

	key := upstreamKey{scheme: req.URL.Scheme, host: req.URL.Host}
	ups, ok := t.upstreams[key]
	if !ok {
		return t.roundTrip(t.transport, req)
	}

	// Create picker based on algorithm
	pk := t.newPicker(ups, req)
	if pk == nil {
		return nil, ErrInvalidUpstreamConfig
	}

	// Prepare body for retries
	getBody, cleanup, err := t.prepareBody(req)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	reqCtx := req.Context()

	for {
		peer := pk.Pick()
		if peer == nil {
			// No more peers available, send to transport directly
			return t.transport.RoundTrip(req)
		}

		addr := ups.backends[peer]
		if addr == nil {
			pk.Done(peer, true)
			return nil, fmt.Errorf("cslb: selected peer %q has no backend address", peer.Addr)
		}

		// Clone URL to avoid mutating shared state
		u := *req.URL
		req.URL = &u

		// Preserve original Host header
		if req.Host == "" {
			req.Host = key.host
		}

		// Rewrite to selected backend
		req.URL.Scheme = addr.scheme
		req.URL.Host = addr.host

		// Restore body for this attempt
		if getBody != nil {
			body, err := getBody()
			if err != nil {
				pk.Done(peer, true)
				return nil, err
			}
			req.Body = body
			req.GetBody = getBody
		}

		// Apply per-attempt timeout
		var cancel context.CancelFunc
		if t.timeout > 0 {
			var ctx context.Context
			ctx, cancel = context.WithTimeout(reqCtx, t.timeout)
			req = req.WithContext(ctx)
		}

		// Use per-upstream transport (with custom SNI) if available
		rt := t.transport
		if ups.transport != nil {
			rt = ups.transport
		}

		resp, err := t.roundTrip(rt, req)

		// Handle context cancellation
		if err != nil {
			if cancel != nil {
				cancel()
			}
		} else if cancel != nil {
			resp.Body = &cancelCloser{ReadCloser: resp.Body, cancel: cancel}
		}

		// Determine if this attempt should be retried on next upstream.
		// Default: retry on connection error or any 5xx status code.
		// Configurable via WithNextUpstream / WithNextUpstreamCodes.
		shouldRetry := t.shouldRetry(resp, err)
		pk.Done(peer, shouldRetry)

		if !shouldRetry {
			return resp, err
		}

		// Close failed response body before retrying
		if err == nil {
			resp.Body.Close()
		}
	}
}

// shouldRetry determines whether the request should be retried on the next peer.
func (t *Transport) shouldRetry(resp *http.Response, err error) bool {
	if t.nextUpstream != nil {
		return t.nextUpstream(resp, err)
	}
	// Default: retry on connection error or any 5xx
	return err != nil || resp.StatusCode >= 500
}

func (t *Transport) roundTrip(rt http.RoundTripper, req *http.Request) (*http.Response, error) {
	if rt == nil {
		return nil, ErrNilRoundTripper
	}

	resp, err := rt.RoundTrip(req)
	if resp == nil && err == nil {
		return nil, ErrInvalidRoundTrip
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
	}
	return ups.balancer.NewPicker()
}

// prepareBody buffers the request body so it can be replayed on retries.
// Returns a getBody function, an optional cleanup function, and any error.
func (t *Transport) prepareBody(req *http.Request) (getBody func() (io.ReadCloser, error), cleanup func(), err error) {
	if req.GetBody != nil {
		return req.GetBody, nil, nil
	}

	if req.Body == nil {
		return nil, nil, nil
	}

	// Optimization: seekable body (e.g., *os.File)
	if seeker, ok := req.Body.(io.Seeker); ok {
		initialBody := req.Body
		startPos, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, nil, err
		}
		return func() (io.ReadCloser, error) {
			_, err := seeker.Seek(startPos, io.SeekStart)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(initialBody), nil
		}, func() { initialBody.Close() }, nil
	}

	// Buffer body: memory or spill to temp file
	b, f, err := t.readBody(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, nil, err
	}

	if f != nil {
		// Spilled to file
		stat, _ := f.Stat()
		req.ContentLength = stat.Size()
		return func() (io.ReadCloser, error) {
				_, err := f.Seek(0, 0)
				if err != nil {
					return nil, err
				}
				return io.NopCloser(f), nil
			}, func() {
				name := f.Name()
				f.Close()
				os.Remove(name)
			}, nil
	}

	// Fits in memory
	req.ContentLength = int64(len(b))
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}, nil, nil
}

// readBody reads a request body, buffering small bodies in memory and
// spilling large ones to a temporary file.
func (t *Transport) readBody(r io.Reader) ([]byte, *os.File, error) {
	limit := t.maxBodyBuffer
	if limit <= 0 {
		limit = defaultMaxBodyBuffer
	}

	lr := io.LimitReader(r, limit+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, nil, err
	}

	if int64(len(b)) <= limit {
		return b, nil, nil
	}

	// Spill to temp file
	f, err := os.CreateTemp("", "cslb-body-*")
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
// X-Forwarded-For, and RemoteAddr in order.
func clientIP(req *http.Request) net.IP {
	if ip := req.Header.Get("X-Real-IP"); ip != "" {
		return net.ParseIP(ip)
	}
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return net.ParseIP(strings.TrimSpace(xff[:i]))
		}
		return net.ParseIP(strings.TrimSpace(xff))
	}
	if req.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(req.RemoteAddr)
		if err != nil {
			return net.ParseIP(req.RemoteAddr)
		}
		return net.ParseIP(host)
	}
	return nil
}
