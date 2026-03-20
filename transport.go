package cslb

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultMaxBodyBuffer = 32 << 20 // 32MB

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
	balancer Balancer
	backends map[*Peer]*backendAddr
	hashFunc func(*http.Request) string
	algo     AlgorithmType
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

// upstreamConfig collects configuration before building an upstream.
type upstreamConfig struct {
	defaultScheme string
	peers         []*Peer
	addrs         map[*Peer]*backendAddr
	algo          AlgorithmType
	hashFunc      func(*http.Request) string
}

// UpstreamOption configures an upstream group.
type UpstreamOption func(*upstreamConfig)

// BackendOption configures a backend peer.
type BackendOption func(*Peer)

// ---------- Constructor ----------

// NewTransport creates a new load-balancing transport with the given options.
//
// Usage:
//
//	transport := cslb.NewTransport(
//	    cslb.WithUpstream("http://api.example.com",
//	        cslb.Backend("http://10.0.0.1:8080", cslb.Weight(5)),
//	        cslb.Backend("http://10.0.0.2:8080", cslb.Weight(3)),
//	        cslb.Backend("http://10.0.0.3:8080", cslb.AsBackup()),
//	        cslb.UseLeastConn(),
//	    ),
//	    cslb.WithTimeout(5*time.Second),
//	    cslb.WithConnectTimeout(3*time.Second),
//	)
//	client := &http.Client{Transport: transport}
func NewTransport(opts ...TransportOption) *Transport {
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
	return t
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

// WithUpstream registers an upstream group. The pattern is a URL like
// "http://api.example.com" that incoming requests are matched against by
// scheme and host. When matched, the request is rewritten to one of the
// configured backends.
func WithUpstream(pattern string, opts ...UpstreamOption) TransportOption {
	return func(t *Transport) {
		u, err := url.Parse(pattern)
		if err != nil {
			panic("cslb: invalid upstream pattern: " + pattern)
		}
		key := upstreamKey{scheme: u.Scheme, host: u.Host}

		cfg := &upstreamConfig{
			defaultScheme: u.Scheme,
			addrs:         make(map[*Peer]*backendAddr),
		}
		for _, opt := range opts {
			opt(cfg)
		}

		// Build balancer based on algorithm
		var b Balancer
		switch cfg.algo {
		case AlgoLeastConn:
			b = NewLeastConn(cfg.peers)
		case AlgoIPHash:
			b = NewIPHash(cfg.peers)
		case AlgoHash:
			b = NewHash(cfg.peers)
		case AlgoRandom:
			b = NewRandom(cfg.peers)
		case AlgoRandomTwo:
			b = NewRandomTwo(cfg.peers)
		default:
			b = NewRoundRobin(cfg.peers)
		}

		t.upstreams[key] = &upstream{
			balancer: b,
			backends: cfg.addrs,
			hashFunc: cfg.hashFunc,
			algo:     cfg.algo,
		}
	}
}

// ---------- Upstream options ----------

// Backend adds a backend server to the upstream group. The addr is a URL like
// "http://10.0.0.1:8080". If the scheme is omitted (e.g., "10.0.0.1:8080"),
// it inherits from the upstream pattern.
func Backend(addr string, opts ...BackendOption) UpstreamOption {
	return func(cfg *upstreamConfig) {
		scheme := cfg.defaultScheme
		host := addr

		if strings.Contains(addr, "://") {
			u, err := url.Parse(addr)
			if err != nil {
				panic("cslb: invalid backend address: " + addr)
			}
			scheme = u.Scheme
			host = u.Host
		}

		p := &Peer{Addr: addr}
		for _, opt := range opts {
			opt(p)
		}

		cfg.peers = append(cfg.peers, p)
		cfg.addrs[p] = &backendAddr{scheme: scheme, host: host}
	}
}

// UseRoundRobin selects smooth weighted round-robin (default).
func UseRoundRobin() UpstreamOption {
	return func(cfg *upstreamConfig) { cfg.algo = AlgoRoundRobin }
}

// UseLeastConn selects least-connections algorithm.
func UseLeastConn() UpstreamOption {
	return func(cfg *upstreamConfig) { cfg.algo = AlgoLeastConn }
}

// UseIPHash selects IP-hash algorithm. Client IP is extracted from
// X-Real-IP, X-Forwarded-For, or RemoteAddr.
func UseIPHash() UpstreamOption {
	return func(cfg *upstreamConfig) { cfg.algo = AlgoIPHash }
}

// UseHash selects key-based hash algorithm. The keyFunc extracts a hash key
// from each request (e.g., URI path, cookie value, header).
func UseHash(keyFunc func(*http.Request) string) UpstreamOption {
	return func(cfg *upstreamConfig) {
		cfg.algo = AlgoHash
		cfg.hashFunc = keyFunc
	}
}

// UseRandom selects random algorithm.
func UseRandom() UpstreamOption {
	return func(cfg *upstreamConfig) { cfg.algo = AlgoRandom }
}

// UseRandomTwo selects random-two (Power of Two Choices) algorithm.
func UseRandomTwo() UpstreamOption {
	return func(cfg *upstreamConfig) { cfg.algo = AlgoRandomTwo }
}

// ---------- Backend options ----------

// Weight sets the backend's weight. Higher weight means more traffic.
func Weight(w int) BackendOption {
	return func(p *Peer) { p.Weight = w }
}

// MaxFails sets the number of failures before a backend is temporarily disabled.
func MaxFails(n int) BackendOption {
	return func(p *Peer) { p.MaxFails = n }
}

// FailTimeout sets the duration a failed backend stays disabled.
func FailTimeout(d time.Duration) BackendOption {
	return func(p *Peer) { p.FailTimeout = d }
}

// MaxConns sets the maximum concurrent connections to a backend.
func MaxConns(n int) BackendOption {
	return func(p *Peer) { p.MaxConns = n }
}

// AsBackup marks this backend as a backup server, only used when all
// primary servers are unavailable.
func AsBackup() BackendOption {
	return func(p *Peer) { p.Backup = true }
}

// AsDown marks this backend as initially down.
func AsDown() BackendOption {
	return func(p *Peer) { p.Down = true }
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
	key := upstreamKey{scheme: req.URL.Scheme, host: req.URL.Host}
	ups, ok := t.upstreams[key]
	if !ok {
		return t.transport.RoundTrip(req)
	}

	// Create picker based on algorithm
	pk := t.newPicker(ups, req)

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

		resp, err := t.transport.RoundTrip(req)

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

// newPicker creates a per-request picker, using algorithm-specific constructors
// for IPHash and Hash that require request context.
func (t *Transport) newPicker(ups *upstream, req *http.Request) Picker {
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
