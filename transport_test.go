package cslb

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// ---------- Transport basic tests ----------

func TestTransport_RoundRobinDistribution(t *testing.T) {
	// Start 3 backend servers
	counts := [3]*atomic.Int32{{}, {}, {}}
	for i := range counts {
		counts[i] = &atomic.Int32{}
	}

	servers := make([]*httptest.Server, 3)
	for i := range servers {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[idx].Add(1)
			fmt.Fprintf(w, "backend-%d", idx)
		}))
		defer servers[i].Close()
	}

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://myservice.local",
				Server(servers[0].URL, Weight(5)),
				Server(servers[1].URL, Weight(3)),
				Server(servers[2].URL, Weight(2)),
			),
		),
	)
	client := &http.Client{Transport: tr}

	for i := 0; i < 100; i++ {
		resp, err := client.Get("http://myservice.local/test")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}

	c0, c1, c2 := counts[0].Load(), counts[1].Load(), counts[2].Load()
	t.Logf("distribution: backend-0=%d backend-1=%d backend-2=%d", c0, c1, c2)

	if c0 <= c1 || c1 <= c2 {
		t.Errorf("expected c0 > c1 > c2, got %d %d %d", c0, c1, c2)
	}
}

func TestTransport_NoUpstreamPassthrough(t *testing.T) {
	// Requests not matching any upstream pass through to the underlying transport
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct"))
	}))
	defer backend.Close()

	tr := NewTransport(
		WithRoundTripper(http.DefaultTransport),
		WithUpstreams(
			Upstream("http://other.local",
				Server(backend.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	// This request goes to a real server, not matching "other.local"
	resp, err := client.Get(backend.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct" {
		t.Errorf("expected direct, got %s", string(body))
	}
}

func TestTransport_FailoverToNextPeer(t *testing.T) {
	// First backend always returns 500, second returns 200
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer good.Close()

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://failover.local",
				Server(bad.URL),
				Server(good.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://failover.local/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected ok, got %s", string(body))
	}
}

func TestTransport_BackupFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer primary.Close()

	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backup"))
	}))
	defer backup.Close()

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://backup.local",
				Server(primary.URL),
				Server(backup.URL, Backup()),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://backup.local/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "backup" {
		t.Errorf("expected backup, got %s", string(body))
	}
}

func TestTransport_HostHeaderPreserved(t *testing.T) {
	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://original-host.com",
				Server(backend.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://original-host.com/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if receivedHost != "original-host.com" {
		t.Errorf("host header = %q, want original-host.com", receivedHost)
	}
}

func TestTransport_PostBodyRetried(t *testing.T) {
	attempt := &atomic.Int32{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := attempt.Add(1)
		if n == 1 {
			// First attempt: verify body, then fail
			if string(body) != "hello" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(500)
			return
		}
		// Second attempt: verify body is replayed
		if string(body) != "hello" {
			w.WriteHeader(400)
			fmt.Fprintf(w, "body mismatch: %q", string(body))
			return
		}
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Two backends pointing to the same server to test retry with body replay
	tr := NewTransport(
		WithUpstreams(
			Upstream("http://post.local",
				Server(backend.URL),
				Server(backend.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Post("http://post.local/test", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, body = %s", resp.StatusCode, string(body))
	}
}

// ---------- Algorithm-specific transport tests ----------

func TestTransport_LeastConn(t *testing.T) {
	counts := [2]*atomic.Int32{{}, {}}
	for i := range counts {
		counts[i] = &atomic.Int32{}
	}

	servers := make([]*httptest.Server, 2)
	for i := range servers {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[idx].Add(1)
			w.Write([]byte("ok"))
		}))
		defer servers[i].Close()
	}

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://lc.local",
				Server(servers[0].URL),
				Server(servers[1].URL),
			).LeastConn(),
		),
	)
	client := &http.Client{Transport: tr}

	for i := 0; i < 100; i++ {
		resp, err := client.Get("http://lc.local/test")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	c0, c1 := counts[0].Load(), counts[1].Load()
	t.Logf("least_conn: %d %d", c0, c1)

	// Should be roughly even
	if c0 < 30 || c1 < 30 {
		t.Errorf("uneven distribution: %d %d", c0, c1)
	}
}

func TestTransport_Hash(t *testing.T) {
	servers := make([]*httptest.Server, 3)
	for i := range servers {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "backend-%d", idx)
		}))
		defer servers[i].Close()
	}

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://hash.local",
				Server(servers[0].URL),
				Server(servers[1].URL),
				Server(servers[2].URL),
			).Hash(func(r *http.Request) string {
				return r.URL.Path
			}),
		),
	)
	client := &http.Client{Transport: tr}

	// Same path should consistently go to the same backend
	var firstBody string
	for i := 0; i < 10; i++ {
		resp, err := client.Get("http://hash.local/consistent-path")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if i == 0 {
			firstBody = string(body)
		} else if string(body) != firstBody {
			t.Errorf("inconsistent hash: got %s, expected %s", string(body), firstBody)
		}
	}
}

func TestTransport_Timeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("slow"))
	}))
	defer slow.Close()

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fast"))
	}))
	defer fast.Close()

	tr := NewTransport(
		WithTimeout(500*time.Millisecond),
		WithUpstreams(
			Upstream("http://timeout.local",
				Server(slow.URL),
				Server(fast.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://timeout.local/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "fast" {
		t.Errorf("expected fast, got %s", string(body))
	}
}

func TestTransport_BackendSchemeInherited(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Extract host:port from backend URL
	addr := strings.TrimPrefix(backend.URL, "http://")

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://inherit.local",
				Server(addr), // no scheme — should inherit "http"
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://inherit.local/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected ok, got %s", string(body))
	}
}

// ---------- NextUpstream tests ----------

func TestTransport_NextUpstreamCodes(t *testing.T) {
	// backend1 returns 403, backend2 returns 200
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("forbidden"))
	}))
	defer b1.Close()

	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer b2.Close()

	// Default behavior: 403 is NOT retried (only 5xx)
	tr := NewTransport(
		WithUpstreams(
			Upstream("http://default.local",
				Server(b1.URL),
				Server(b2.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://default.local/test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "forbidden" {
		t.Errorf("default: expected forbidden (no retry on 403), got %s", string(body))
	}

	// With 403 configured as retry code
	tr2 := NewTransport(
		WithNextUpstreamCodes(403, 502, 503, 504),
		WithUpstreams(
			Upstream("http://custom.local",
				Server(b1.URL),
				Server(b2.URL),
			),
		),
	)
	client2 := &http.Client{Transport: tr2}

	resp, err = client2.Get("http://custom.local/test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("custom: expected ok (retry on 403), got %s", string(body))
	}
}

func TestTransport_NextUpstreamCustomCondition(t *testing.T) {
	// backend1 returns 429, backend2 returns 200
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer b1.Close()

	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer b2.Close()

	tr := NewTransport(
		WithNextUpstream(func(resp *http.Response, err error) bool {
			if err != nil {
				return true
			}
			// Retry on 429 (rate limited) and 5xx
			return resp.StatusCode == 429 || resp.StatusCode >= 500
		}),
		WithUpstreams(
			Upstream("http://cond.local",
				Server(b1.URL),
				Server(b2.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://cond.local/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected ok (retry on 429), got %s", string(body))
	}
}

func TestTransport_NextUpstreamCodesNoRetry(t *testing.T) {
	// Both backends return 500, but we only retry on 502
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	}))
	defer b.Close()

	tr := NewTransport(
		WithNextUpstreamCodes(502, 503), // 500 is NOT in the list
		WithUpstreams(
			Upstream("http://noskip.local",
				Server(b.URL),
				Server(b.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://noskip.local/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should NOT retry — 500 is not in the configured codes
	if resp.StatusCode != 500 {
		t.Errorf("expected 500 (no retry), got %d", resp.StatusCode)
	}
}

// ---------- clientIP tests ----------

func TestClientIP_XRealIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Real-IP", "1.2.3.4")
	ip := clientIP(req)
	if ip.String() != "1.2.3.4" {
		t.Errorf("got %s, want 1.2.3.4", ip)
	}
}

func TestClientIP_XForwardedFor(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	ip := clientIP(req)
	if ip.String() != "10.0.0.1" {
		t.Errorf("got %s, want 10.0.0.1", ip)
	}
}

func TestClientIP_RemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	ip := clientIP(req)
	if ip.String() != "192.168.1.1" {
		t.Errorf("got %s, want 192.168.1.1", ip)
	}
}

// ---------- Option tests ----------

func TestOptions_Weight(t *testing.T) {
	p := &Peer{}
	Weight(5)(p)
	if p.Weight != 5 {
		t.Errorf("Weight = %d, want 5", p.Weight)
	}
}

func TestOptions_MaxFails(t *testing.T) {
	p := &Peer{}
	MaxFails(3)(p)
	if p.MaxFails != 3 {
		t.Errorf("MaxFails = %d, want 3", p.MaxFails)
	}
}

func TestOptions_FailTimeout(t *testing.T) {
	p := &Peer{}
	FailTimeout(30 * time.Second)(p)
	if p.FailTimeout != 30*time.Second {
		t.Errorf("FailTimeout = %v, want 30s", p.FailTimeout)
	}
}

func TestOptions_Backup(t *testing.T) {
	p := &Peer{}
	Backup()(p)
	if !p.Backup {
		t.Error("Backup should be true")
	}
}

func TestOptions_Down(t *testing.T) {
	p := &Peer{}
	Down()(p)
	if !p.Down {
		t.Error("Down should be true")
	}
}

func TestTransport_ProxySSLName(t *testing.T) {
	// Start a TLS backend that reports the SNI it received
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni := r.TLS.ServerName
		fmt.Fprintf(w, "sni=%s", sni)
	}))
	defer backend.Close()

	// Extract the test server's cert pool so we can trust it
	certPool := backend.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	// Create a custom transport that trusts the test cert but does NOT set ServerName,
	// so we can verify that ProxySSLName is what sets it.
	baseRT := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: certPool,
			// InsecureSkipVerify so the test works despite ServerName mismatch
			InsecureSkipVerify: true,
		},
	}

	transport := NewTransport(
		WithRoundTripper(baseRT),
		WithUpstreams(
			Upstream("https://myservice.local",
				Server(backend.Listener.Addr().String()),
			).ProxySSLName("custom-sni.example.com"),
		),
	)

	client := &http.Client{Transport: transport}
	resp, err := client.Get("https://myservice.local/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	expected := "sni=custom-sni.example.com"
	if string(body) != expected {
		t.Errorf("got %q, want %q", string(body), expected)
	}
}

func TestTransport_ProxySSLName_DefaultTransport(t *testing.T) {
	// Verify ProxySSLName works with the default transport (no WithRoundTripper)
	transport := NewTransport(
		WithUpstreams(
			Upstream("https://ssl.local",
				Server("https://10.0.0.1:443"),
			).ProxySSLName("ssl.example.com"),
		),
	)

	// Check that the upstream got a cloned transport with the right ServerName
	key := upstreamKey{scheme: "https", host: "ssl.local"}
	ups := transport.upstreams[key]
	if ups == nil {
		t.Fatal("upstream not found")
	}
	if ups.sslName != "ssl.example.com" {
		t.Errorf("sslName = %q, want %q", ups.sslName, "ssl.example.com")
	}
	if ups.transport == nil {
		t.Fatal("per-upstream transport should be set")
	}
	ht, ok := ups.transport.(*http.Transport)
	if !ok {
		t.Fatal("per-upstream transport should be *http.Transport")
	}
	if ht.TLSClientConfig == nil || ht.TLSClientConfig.ServerName != "ssl.example.com" {
		t.Errorf("TLSClientConfig.ServerName = %q, want %q",
			ht.TLSClientConfig.ServerName, "ssl.example.com")
	}
	// Ensure it's a different transport instance than the base
	if ups.transport == transport.transport {
		t.Error("per-upstream transport should be a clone, not the same instance")
	}
}

func TestNewTransportE_InvalidUpstreamPattern(t *testing.T) {
	_, err := NewTransportE(
		WithUpstreams(
			Upstream("not-a-valid-upstream",
				Server("http://127.0.0.1:8080"),
			),
		),
	)
	if err == nil {
		t.Fatal("expected error for invalid upstream pattern")
	}
	if !strings.Contains(err.Error(), `invalid upstream pattern: "not-a-valid-upstream"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewTransportE_WithUpstreams_InvalidServerAddress(t *testing.T) {
	_, err := NewTransportE(
		WithUpstreams(
			Upstream("http://service.local",
				Server("http://"),
			),
		),
	)
	if err == nil {
		t.Fatal("expected error for invalid server address")
	}
	if !strings.Contains(err.Error(), `invalid backend address: "http://"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewTransportE_WithUpstreams_HashRequiresKeyFunc(t *testing.T) {
	_, err := NewTransportE(
		WithUpstreams(
			Upstream("http://hash.local",
				Server("http://127.0.0.1:8080"),
			).Hash(nil),
		),
	)
	if err == nil {
		t.Fatal("expected error for nil hash key func")
	}
	if !strings.Contains(err.Error(), "hash key function must not be nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewTransportE_ProxySSLNameRequiresHTTPTransport(t *testing.T) {
	_, err := NewTransportE(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		})),
		WithUpstreams(
			Upstream("https://ssl.local",
				Server("https://10.0.0.1:443"),
			).ProxySSLName("ssl.example.com"),
		),
	)
	if err == nil {
		t.Fatal("expected error for non-*http.Transport with ProxySSLName")
	}
	if !strings.Contains(err.Error(), "ProxySSLName requires the underlying transport to be *http.Transport") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewTransport_InitError(t *testing.T) {
	transport := NewTransport(
		WithUpstreams(
			Upstream("not-a-valid-upstream",
				Server("http://127.0.0.1:8080"),
			),
		),
	)

	if err := transport.Err(); err == nil {
		t.Fatal("expected construction error to be retained on transport")
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/test", nil)
	_, err := transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected RoundTrip to surface retained init error")
	}
	if !strings.Contains(err.Error(), `invalid upstream pattern: "not-a-valid-upstream"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTransport_RoundTripNilRequest(t *testing.T) {
	transport := NewTransport()

	_, err := transport.RoundTrip(nil)
	if err != ErrNilRequest {
		t.Fatalf("got %v, want %v", err, ErrNilRequest)
	}
}

func TestTransport_InvalidUnderlyingRoundTrip(t *testing.T) {
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, nil
		})),
	)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/test", nil)
	_, err := transport.RoundTrip(req)
	if err != ErrInvalidRoundTrip {
		t.Fatalf("got %v, want %v", err, ErrInvalidRoundTrip)
	}
}

func TestTransport_AllPeersFailReturnsLast(t *testing.T) {
	// Both backends return 500. After exhausting retries the caller should
	// see the last upstream's actual response (mirroring nginx's
	// "last status wins" semantics in ngx_http_upstream_next).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("boom"))
	}))
	defer bad.Close()

	tr := NewTransport(
		WithUpstreams(
			Upstream("http://allbad.local",
				Server(bad.URL),
				Server(bad.URL),
			),
		),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://allbad.local/test")
	if err != nil {
		t.Fatalf("expected last upstream response, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "boom" {
		t.Errorf("body = %q, want %q", body, "boom")
	}
}

func TestTransport_NoPeerAvailable(t *testing.T) {
	// All configured peers are marked Down — Pick() returns nil on the
	// first attempt and we expect an explicit ErrNoPeerAvailable rather
	// than a misleading DNS error from the upstream pattern host.
	tr := NewTransport(
		WithUpstreams(
			Upstream("http://nopeer.local",
				Server("http://10.0.0.1:1", Down()),
			),
		),
	)

	req := httptest.NewRequest(http.MethodGet, "http://nopeer.local/test", nil)
	_, err := tr.RoundTrip(req)
	if err != ErrNoPeerAvailable {
		t.Fatalf("got %v, want ErrNoPeerAvailable", err)
	}
}
