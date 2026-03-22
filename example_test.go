package cslb_test

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/fovecifer/cslb"
)

// ----------------------------------------------------------------
// Example 1: Minimal usage - weighted round-robin
// ----------------------------------------------------------------
// The simplest scenario: one upstream group, two backends, weighted round-robin.
// Fully transparent to the caller - just replace http.Client's Transport.
func Example_minimal() {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "b1")
	}))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "b2")
	}))
	defer b2.Close()

	transport := cslb.NewTransport(
		cslb.WithUpstream("http://api.example.com",
			cslb.Backend(b1.URL, cslb.Weight(3)), // weight 3, receives 3/4 of traffic
			cslb.Backend(b2.URL, cslb.Weight(1)), // weight 1, receives 1/4 of traffic
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://api.example.com/hello")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// b1
}

// ----------------------------------------------------------------
// Example 2: All 6 load balancing algorithms
// ----------------------------------------------------------------
// Demonstrates how to select a different algorithm for each upstream group.
func Example_algorithms() {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer b2.Close()

	transport := cslb.NewTransport(
		// Algorithm 1: Weighted round-robin (default, UseRoundRobin can be omitted)
		cslb.WithUpstream("http://rr.local",
			cslb.Backend(b1.URL, cslb.Weight(5)),
			cslb.Backend(b2.URL, cslb.Weight(1)),
			cslb.UseRoundRobin(), // optional, this is the default
		),

		// Algorithm 2: Least connections - prefers the backend with fewest active connections
		cslb.WithUpstream("http://lc.local",
			cslb.Backend(b1.URL),
			cslb.Backend(b2.URL),
			cslb.UseLeastConn(),
		),

		// Algorithm 3: IP Hash - same client IP always routes to the same backend (session affinity)
		// Client IP is extracted from X-Real-IP -> X-Forwarded-For -> RemoteAddr
		cslb.WithUpstream("http://iphash.local",
			cslb.Backend(b1.URL),
			cslb.Backend(b2.URL),
			cslb.UseIPHash(),
		),

		// Algorithm 4: Key Hash - consistent routing based on a custom key
		// Useful for caching: same URL always hits the same backend, improving cache hit rate
		cslb.WithUpstream("http://hash.local",
			cslb.Backend(b1.URL),
			cslb.Backend(b2.URL),
			cslb.UseHash(func(r *http.Request) string {
				return r.URL.Path // hash by path
			}),
		),

		// Algorithm 5: Random
		cslb.WithUpstream("http://rand.local",
			cslb.Backend(b1.URL),
			cslb.Backend(b2.URL),
			cslb.UseRandom(),
		),

		// Algorithm 6: Random two (Power of Two Choices)
		// Picks two random backends and selects the one with fewer connections
		cslb.WithUpstream("http://rand2.local",
			cslb.Backend(b1.URL),
			cslb.Backend(b2.URL),
			cslb.UseRandomTwo(),
		),
	)

	client := &http.Client{Transport: transport}

	// Each upstream group operates independently
	for _, host := range []string{"rr", "lc", "iphash", "hash", "rand", "rand2"} {
		resp, _ := client.Get("http://" + host + ".local/test")
		resp.Body.Close()
		fmt.Printf("%s -> %d\n", host, resp.StatusCode)
	}

	// Output:
	// rr -> 200
	// lc -> 200
	// iphash -> 200
	// hash -> 200
	// rand -> 200
	// rand2 -> 200
}

// ----------------------------------------------------------------
// Example 3: All Backend options
// ----------------------------------------------------------------
// Demonstrates every configuration parameter supported by Backend.
func Example_backendOptions() {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer b.Close()

	// Extract host:port from URL to demonstrate scheme inheritance
	addr := strings.TrimPrefix(b.URL, "http://")

	transport := cslb.NewTransport(
		cslb.WithUpstream("http://backend-opts.local",
			// Backend 1: all parameters configured
			cslb.Backend(b.URL,
				cslb.Weight(10),                       // weight (default 1)
				cslb.MaxFails(3),                      // mark unavailable after 3 consecutive failures (default 1)
				cslb.FailTimeout(30*time.Second),       // how long a failed backend stays unavailable (default 10s)
				cslb.MaxConns(200),                    // max concurrent connections (default 0 = unlimited)
			),

			// Backend 2: scheme omitted, inherits "http" from the upstream pattern
			cslb.Backend(addr), // equivalent to Backend("http://"+addr)

			// Backend 3: backup server - only used when all primary servers are unavailable
			cslb.Backend(b.URL, cslb.AsBackup()),

			// Backend 4: marked as initially down
			cslb.Backend(b.URL, cslb.AsDown()),
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://backend-opts.local/test")
	resp.Body.Close()
	fmt.Println(resp.StatusCode)

	// Output:
	// 200
}

// ----------------------------------------------------------------
// Example 4: Automatic failover and backup servers
// ----------------------------------------------------------------
// When a primary server returns 5xx, the request is automatically retried
// on the next peer, falling back to backup servers if needed.
func Example_failoverAndBackup() {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "backup OK")
	}))
	defer good.Close()

	transport := cslb.NewTransport(
		cslb.WithUpstream("http://failover.local",
			cslb.Backend(bad.URL),                    // primary (returns 502)
			cslb.Backend(good.URL, cslb.AsBackup()),  // backup server
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://failover.local/test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Primary returns 502 -> automatically falls back to backup
	fmt.Println(string(body))

	// Output:
	// backup OK
}

// ----------------------------------------------------------------
// Example 5: Custom retry conditions (proxy_next_upstream)
// ----------------------------------------------------------------
// Similar to nginx's proxy_next_upstream directive.
// Configure which HTTP status codes should trigger a retry on the next backend.
func Example_nextUpstreamCodes() {
	ratelimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429) // Too Many Requests
	}))
	defer ratelimited.Close()

	normal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer normal.Close()

	// ---- Method 1: specify a list of status codes ----
	// Equivalent to nginx: proxy_next_upstream error timeout http_429 http_502 http_503 http_504;
	t1 := cslb.NewTransport(
		cslb.WithNextUpstreamCodes(429, 502, 503, 504),
		cslb.WithUpstream("http://codes.local",
			cslb.Backend(ratelimited.URL),
			cslb.Backend(normal.URL),
		),
	)
	client := &http.Client{Transport: t1}
	resp, _ := client.Get("http://codes.local/test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println("codes:", string(body))

	// ---- Method 2: fully custom condition function ----
	// Can inspect headers, status codes, or any request/response property
	t2 := cslb.NewTransport(
		cslb.WithNextUpstream(func(resp *http.Response, err error) bool {
			if err != nil {
				return true // always retry on connection error / timeout
			}
			// 429 rate-limited or 5xx server error -> retry
			return resp.StatusCode == 429 || resp.StatusCode >= 500
		}),
		cslb.WithUpstream("http://custom.local",
			cslb.Backend(ratelimited.URL),
			cslb.Backend(normal.URL),
		),
	)
	client2 := &http.Client{Transport: t2}
	resp, _ = client2.Get("http://custom.local/test")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println("custom:", string(body))

	// Output:
	// codes: ok
	// custom: ok
}

// ----------------------------------------------------------------
// Example 6: Frontend HTTP / Backend HTTPS (different schemes)
// ----------------------------------------------------------------
// The client sends HTTP requests, but backends are HTTPS services.
// Backend's scheme can differ from the upstream pattern's scheme.
// You can also mix HTTP and HTTPS backends within the same upstream group.
func Example_httpToHttps() {
	// Start an HTTPS backend
	httpsBackend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "https ok, host=%s", r.Host)
	}))
	defer httpsBackend.Close()

	// Use the HTTPS backend's TLS client config to trust its certificate
	transport := cslb.NewTransport(
		cslb.WithRoundTripper(httpsBackend.Client().Transport),
		cslb.WithUpstream("http://frontend.local",
			// Frontend is HTTP, backend is HTTPS - scheme is automatically rewritten
			cslb.Backend(httpsBackend.URL), // "https://127.0.0.1:xxx"
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://frontend.local/api/data")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// https ok, host=frontend.local
}

// ----------------------------------------------------------------
// Example 7: Mixed HTTP and HTTPS backends in the same upstream
// ----------------------------------------------------------------
// Different backends can use different schemes. The load balancer
// connects to each backend using its own scheme.
func Example_mixedScheme() {
	httpBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "http-backend")
	}))
	defer httpBackend.Close()

	httpsBackend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "https-backend")
	}))
	defer httpsBackend.Close()

	// Need a transport that can access both HTTP and trust the test certificate
	transport := cslb.NewTransport(
		cslb.WithRoundTripper(httpsBackend.Client().Transport),
		cslb.WithUpstream("http://mixed.local",
			cslb.Backend(httpBackend.URL),  // http://127.0.0.1:xxx
			cslb.Backend(httpsBackend.URL), // https://127.0.0.1:yyy
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://mixed.local/test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// http-backend
}

// ----------------------------------------------------------------
// Example 8: Timeout control
// ----------------------------------------------------------------
// WithTimeout sets an independent timeout for each backend attempt.
// If a backend times out, the request is automatically retried on the next one.
func Example_timeout() {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, "slow")
	}))
	defer slow.Close()

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fast")
	}))
	defer fast.Close()

	transport := cslb.NewTransport(
		cslb.WithTimeout(500*time.Millisecond), // max 500ms per attempt
		cslb.WithUpstream("http://timeout.local",
			cslb.Backend(slow.URL), // will time out
			cslb.Backend(fast.URL), // automatically falls back here
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://timeout.local/test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// fast
}

// ----------------------------------------------------------------
// Example 9: Custom underlying Transport
// ----------------------------------------------------------------
// Use WithRoundTripper to provide a custom http.Transport with your own
// TLS configuration, connection pool settings, proxy, etc.
func Example_customTransport() {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer b.Close()

	// Custom underlying Transport
	customRT := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 60 * time.Second,
		}).DialContext,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     120 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}

	transport := cslb.NewTransport(
		cslb.WithRoundTripper(customRT), // provide custom Transport
		cslb.WithUpstream("http://custom-rt.local",
			cslb.Backend(b.URL),
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://custom-rt.local/test")
	resp.Body.Close()
	fmt.Println(resp.StatusCode)

	// Output:
	// 200
}

// ----------------------------------------------------------------
// Example 10: POST body automatic replay on retry
// ----------------------------------------------------------------
// When a POST request fails on one backend, the body is automatically
// buffered and replayed when retrying on the next backend.
// Supports three modes: GetBody callback, Seekable body, memory/file buffer.
func Example_postBodyReplay() {
	attempt := 0
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt++
		if attempt == 1 {
			w.WriteHeader(500)
			return
		}
		fmt.Fprintf(w, "got: %s", string(body))
	}))
	defer b.Close()

	transport := cslb.NewTransport(
		cslb.WithUpstream("http://post.local",
			cslb.Backend(b.URL), // first attempt fails
			cslb.Backend(b.URL), // body is automatically replayed on retry
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Post(
		"http://post.local/submit",
		"application/json",
		strings.NewReader(`{"name":"test"}`),
	)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// got: {"name":"test"}
}

// ----------------------------------------------------------------
// Example 11: Large body spill to temporary file
// ----------------------------------------------------------------
// WithMaxBodyBuffer controls the maximum body size buffered in memory.
// Bodies exceeding this limit are written to a temporary file to avoid OOM.
func Example_maxBodyBuffer() {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "received %d bytes", n)
	}))
	defer b.Close()

	transport := cslb.NewTransport(
		cslb.WithMaxBodyBuffer(1<<20), // 1MB in-memory limit, larger bodies spill to temp file
		cslb.WithUpstream("http://bigbody.local",
			cslb.Backend(b.URL),
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Post(
		"http://bigbody.local/upload",
		"application/octet-stream",
		strings.NewReader(strings.Repeat("x", 1024)),
	)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// received 1024 bytes
}

// ----------------------------------------------------------------
// Example 12: Host header preservation
// ----------------------------------------------------------------
// When a request is routed to a backend, the original Host header is preserved.
// The backend receives "preserve-host.local" instead of "127.0.0.1:xxx".
func Example_hostHeaderPreserved() {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Host)
	}))
	defer b.Close()

	transport := cslb.NewTransport(
		cslb.WithUpstream("http://preserve-host.local",
			cslb.Backend(b.URL),
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("http://preserve-host.local/test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// preserve-host.local
}

// ----------------------------------------------------------------
// Example 13: Unmatched requests pass through directly
// ----------------------------------------------------------------
// Only requests matching a registered upstream (scheme://host) are load-balanced.
// All other requests pass through to the underlying Transport unmodified.
func Example_passthrough() {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "direct")
	}))
	defer b.Close()

	transport := cslb.NewTransport(
		cslb.WithRoundTripper(http.DefaultTransport),
		cslb.WithUpstream("http://registered.local",
			cslb.Backend(b.URL),
		),
	)

	client := &http.Client{Transport: transport}

	// This request does not match "registered.local", passes through directly
	resp, _ := client.Get(b.URL + "/direct")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// direct
}

// ----------------------------------------------------------------
// Example 14: ProxySSLName - override TLS SNI for backend connections
// ----------------------------------------------------------------
// When backends are addressed by IP, TLS handshakes normally send
// the IP as SNI. ProxySSLName overrides the SNI to the specified
// hostname, matching nginx's proxy_ssl_server_name + proxy_ssl_name.
func Example_proxySSLName() {
	// Start a TLS backend that reports the SNI it received
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "sni=%s", r.TLS.ServerName)
	}))
	defer backend.Close()

	// Trust the test server's certificate
	testTransport := backend.Client().Transport.(*http.Transport)

	transport := cslb.NewTransport(
		cslb.WithRoundTripper(testTransport),
		cslb.WithUpstream("https://api.example.com",
			// Backend is an IP address — without ProxySSLName, SNI would be the IP
			cslb.Backend(backend.URL),
			// Override SNI to the desired hostname
			cslb.ProxySSLName("api.example.com"),
		),
	)

	client := &http.Client{Transport: transport}
	resp, _ := client.Get("https://api.example.com/data")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))

	// Output:
	// sni=api.example.com
}

// ----------------------------------------------------------------
// Example 15: Hash by cookie for session affinity
// ----------------------------------------------------------------
// UseHash's keyFunc can extract any request attribute as the hash key.
// Common use cases: route by Cookie, Header, or query parameter for session affinity.
func Example_hashByCookie() {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "b1")
	}))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "b2")
	}))
	defer b2.Close()

	transport := cslb.NewTransport(
		cslb.WithUpstream("http://session.local",
			cslb.Backend(b1.URL),
			cslb.Backend(b2.URL),
			cslb.UseHash(func(r *http.Request) string {
				// Hash by session cookie for session affinity
				if c, err := r.Cookie("session_id"); err == nil {
					return c.Value
				}
				// Fall back to URI when no cookie is present
				return r.URL.RequestURI()
			}),
		),
	)

	client := &http.Client{Transport: transport}

	// Same session_id always routes to the same backend
	req, _ := http.NewRequest("GET", "http://session.local/page", nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})

	var firstResult string
	for i := 0; i < 5; i++ {
		resp, _ := client.Do(req)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if i == 0 {
			firstResult = string(body)
		}
	}
	fmt.Println("consistent:", firstResult)

	// Output:
	// consistent: b1
}
