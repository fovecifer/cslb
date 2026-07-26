package cslb

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTransport_TLSHandshakeFailureBeforeSendRetriesStreamingPost(t *testing.T) {
	bad := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("plain HTTP backend unexpectedly received a parsed request")
	}))
	bad.Config.ErrorLog = log.New(io.Discard, "", 0)
	bad.Start()
	defer bad.Close()

	var received string
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		received = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	roots := x509.NewCertPool()
	roots.AddCert(good.Certificate())
	underlying := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs: roots,
		},
	}
	defer underlying.CloseIdleConnections()

	transport := NewTransport(
		WithRoundTripper(underlying),
		WithUpstreams(Upstream("https://tls-failover.local",
			Server("https://"+bad.Listener.Addr().String()),
			Server(good.URL),
		)),
	)
	original := newTrackingBody("payload")
	req, err := http.NewRequest(http.MethodPost, "https://tls-failover.local/test", original)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if received != "payload" {
		t.Fatalf("good backend body = %q, want payload", received)
	}
	if !original.closed {
		t.Fatal("streaming request body was not closed")
	}
}

func TestTransport_HTTP2StreamFailureFailsOver(t *testing.T) {
	var badAttempts atomic.Int64
	bad := newHTTP2TestServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		badAttempts.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer bad.Close()

	var goodProtocol atomic.Int64
	good := newHTTP2TestServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		goodProtocol.Store(int64(req.ProtoMajor))
		fmt.Fprint(w, "ok")
	}))
	defer good.Close()

	roots := x509.NewCertPool()
	roots.AddCert(bad.Certificate())
	roots.AddCert(good.Certificate())
	underlying := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs: roots,
		},
	}
	defer underlying.CloseIdleConnections()

	transport := NewTransport(
		WithRoundTripper(underlying),
		WithUpstreams(Upstream("https://http2-failover.local",
			Server(bad.URL),
			Server(good.URL),
		)),
	)
	resp, err := transport.RoundTrip(mustRequest(t, http.MethodGet, "https://http2-failover.local/test", nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "ok" {
		t.Fatalf("response body = %q, want ok", body)
	}
	if badAttempts.Load() != 1 {
		t.Fatalf("failed HTTP/2 attempts = %d, want 1", badAttempts.Load())
	}
	if goodProtocol.Load() != 2 {
		t.Fatalf("successful backend protocol = HTTP/%d, want HTTP/2", goodProtocol.Load())
	}
}

func TestTransport_RealWriteBoundaryControlsNonIdempotentRetry(t *testing.T) {
	tests := []struct {
		method      string
		wantRetry   bool
		wantSuccess bool
	}{
		{method: http.MethodPost},
		{method: http.MethodPut, wantRetry: true, wantSuccess: true},
	}

	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			resetURL, resetBody := startResetAfterRequestServer(t)
			var goodAttempts atomic.Int64
			var goodBody string
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				goodAttempts.Add(1)
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
					return
				}
				goodBody = string(body)
				w.WriteHeader(http.StatusOK)
			}))
			defer good.Close()

			underlying := &http.Transport{Proxy: nil}
			defer underlying.CloseIdleConnections()
			transport := NewTransport(
				WithRoundTripper(underlying),
				WithUpstreams(Upstream("http://write-boundary.local",
					Server(resetURL),
					Server(good.URL),
				)),
			)

			req := mustRequest(
				t,
				test.method,
				"http://write-boundary.local/test",
				strings.NewReader("payload"),
			)
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
			if (err == nil) != test.wantSuccess {
				t.Fatalf("RoundTrip error = %v, wantSuccess %v", err, test.wantSuccess)
			}
			if got := <-resetBody; got != "payload" {
				t.Fatalf("reset backend body = %q, want payload", got)
			}
			wantGoodAttempts := int64(0)
			if test.wantRetry {
				wantGoodAttempts = 1
			}
			if got := goodAttempts.Load(); got != wantGoodAttempts {
				t.Fatalf("good backend attempts = %d, want %d", got, wantGoodAttempts)
			}
			if test.wantRetry && goodBody != "payload" {
				t.Fatalf("good backend body = %q, want payload", goodBody)
			}
		})
	}
}

func TestTransport_ProxyCONNECTFailureBeforeSendRetriesStreamingPost(t *testing.T) {
	var received string
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		received = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	const rejectedAuthority = "reject.invalid:443"
	var rejected atomic.Int64
	var tunneled atomic.Int64
	proxy := newConnectProxy(t, rejectedAuthority, &rejected, &tunneled)
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(good.Certificate())
	underlying := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs: roots,
		},
	}
	defer underlying.CloseIdleConnections()
	transport := NewTransport(
		WithRoundTripper(underlying),
		WithUpstreams(Upstream("https://proxy-failover.local",
			Server("https://"+rejectedAuthority),
			Server(good.URL),
		)),
	)

	original := newTrackingBody("payload")
	req := mustRequest(t, http.MethodPost, "https://proxy-failover.local/test", original)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if received != "payload" {
		t.Fatalf("good backend body = %q, want payload", received)
	}
	if rejected.Load() != 1 || tunneled.Load() != 1 {
		t.Fatalf("proxy CONNECT counts = rejected:%d tunneled:%d, want 1 each",
			rejected.Load(), tunneled.Load())
	}
	if !original.closed {
		t.Fatal("streaming request body was not closed")
	}
}

func TestTransport_IPv6LoopbackBackend(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}

	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ipv6")
	}))
	backend.Listener = listener
	backend.Start()
	defer backend.Close()

	underlying := &http.Transport{Proxy: nil}
	defer underlying.CloseIdleConnections()
	transport := NewTransport(
		WithRoundTripper(underlying),
		WithUpstreams(Upstream("http://ipv6.local",
			Server(backend.URL),
		)),
	)

	resp, err := transport.RoundTrip(mustRequest(t, http.MethodGet, "http://ipv6.local/test", nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ipv6" {
		t.Fatalf("response body = %q, want ipv6", body)
	}
}

func newHTTP2TestServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	return server
}

func startResetAfterRequestServer(t *testing.T) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bodyCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			bodyCh <- ""
			return
		}
		defer conn.Close()
		req, readErr := http.ReadRequest(bufio.NewReader(conn))
		if readErr != nil {
			bodyCh <- ""
			return
		}
		body, readErr := io.ReadAll(req.Body)
		req.Body.Close()
		if readErr != nil {
			bodyCh <- ""
			return
		}
		bodyCh <- string(body)
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return "http://" + listener.Addr().String(), bodyCh
}

func newConnectProxy(
	t *testing.T,
	rejectedAuthority string,
	rejected, tunneled *atomic.Int64,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		if req.Host == rejectedAuthority {
			rejected.Add(1)
			http.Error(w, "rejected", http.StatusBadGateway)
			return
		}

		upstream, err := net.Dial("tcp", req.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		client, buffer, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		tunneled.Add(1)

		defer client.Close()
		defer upstream.Close()
		if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := buffer.Flush(); err != nil {
			return
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.Copy(upstream, client)
			if halfCloser, ok := upstream.(interface{ CloseWrite() error }); ok {
				halfCloser.CloseWrite()
			}
		}()
		io.Copy(client, upstream)
		if halfCloser, ok := client.(interface{ CloseWrite() error }); ok {
			halfCloser.CloseWrite()
		}
		wg.Wait()
	}))
}

func mustRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
