package cslb

// Regression tests for the findings of the 2026-09-25 code review.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fullDuplexCountServer answers with headers first, then drains the request
// body and writes the number of bytes it received. This is the shape of any
// streaming or bidirectional endpoint that responds before it has consumed the
// whole request body.
func fullDuplexCountServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.EnableFullDuplex(); err != nil {
			t.Errorf("EnableFullDuplex: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush: %v", err)
			return
		}
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			fmt.Fprintf(w, "read-error after %d bytes: %v", n, err)
			return
		}
		fmt.Fprintf(w, "%d", n)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// closeSignalBody records when the transport finally closes the caller's body.
type closeSignalBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeSignalBody) Close() error {
	b.closed.Store(true)
	return nil
}

// The request body must stay usable after RoundTrip returns: a RoundTripper
// may keep streaming it once response headers have arrived, and closes it when
// it is done.
func TestTransport_StreamingBodyOutlivesRoundTrip(t *testing.T) {
	srv := fullDuplexCountServer(t)

	const chunk = 64 << 10
	const chunks = 16
	want := strconv.Itoa(chunk * chunks)

	run := func(t *testing.T, rt http.RoundTripper, url string) {
		pr, pw := io.Pipe()
		body := &closeSignalBody{Reader: pr}
		req, err := http.NewRequest(http.MethodPut, url, body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()

		writeErr := make(chan error, 1)
		go func() {
			buf := bytes.Repeat([]byte("x"), chunk)
			for i := 0; i < chunks; i++ {
				if _, err := pw.Write(buf); err != nil {
					writeErr <- err
					return
				}
			}
			writeErr <- pw.Close()
		}()

		got, _ := io.ReadAll(resp.Body)
		if err := <-writeErr; err != nil {
			t.Fatalf("upload aborted after RoundTrip returned: %v (server said %q)", err, got)
		}
		if string(got) != want {
			t.Fatalf("server received %q bytes, want %s", got, want)
		}
		waitFor(t, "request body close", body.closed.Load)
	}

	t.Run("baseline plain http.Transport", func(t *testing.T) {
		run(t, &http.Transport{}, srv.URL)
	})

	t.Run("cslb single backend", func(t *testing.T) {
		tr, err := NewTransportE(
			WithRoundTripper(&http.Transport{}),
			WithUpstreams(Upstream("http://svc.test", Server(srv.URL))),
		)
		if err != nil {
			t.Fatal(err)
		}
		run(t, tr, "http://svc.test/upload")
	})
}

type repeatReader byte

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestTransport_SpilledBodyOutlivesRoundTrip(t *testing.T) {
	srv := fullDuplexCountServer(t)
	tempDir := t.TempDir()
	setProcessTempDir(t, tempDir)

	const size = 8 << 20 // larger than socket buffers, so it cannot all be in flight
	tr, err := NewTransportE(
		WithRoundTripper(&http.Transport{}),
		WithMaxBodyBuffer(0),
		WithUpstreams(Upstream("http://svc.test",
			Server(srv.URL),
			Server(srv.URL),
		)),
	)
	if err != nil {
		t.Fatal(err)
	}

	// A non-seekable body without GetBody takes the spill path.
	body := io.NopCloser(io.LimitReader(repeatReader('y'), size))
	req, err := http.NewRequest(http.MethodPut, "http://svc.test/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != strconv.Itoa(size) {
		t.Fatalf("server received %q, want %d bytes", got, size)
	}
	waitFor(t, "temporary body file removal", func() bool {
		entries, err := os.ReadDir(tempDir)
		return err == nil && len(entries) == 0
	})
}

// seekableFileBody hides nothing of *os.File except Close, which it records.
type seekableFileBody struct {
	*os.File
	closed atomic.Bool
}

func (b *seekableFileBody) Close() error {
	b.closed.Store(true)
	return b.File.Close()
}

func TestTransport_SeekableBodyOutlivesRoundTrip(t *testing.T) {
	srv := fullDuplexCountServer(t)

	const size = 8 << 20
	file, err := os.CreateTemp(t.TempDir(), "body-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(file, io.LimitReader(repeatReader('z'), size)); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	body := &seekableFileBody{File: file}

	tr, err := NewTransportE(
		WithRoundTripper(&http.Transport{}),
		WithUpstreams(Upstream("http://svc.test", Server(srv.URL), Server(srv.URL))),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, "http://svc.test/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != strconv.Itoa(size) {
		t.Fatalf("server received %q, want %d bytes", got, size)
	}
	waitFor(t, "request body close", body.closed.Load)
}

// Replays of a seekable body must not share one file offset: a RoundTripper
// may still be reading a superseded attempt's body while the retry reads.
func TestReplaySource_ReadersKeepIndependentOffsets(t *testing.T) {
	const payload = "0123456789abcdefghij"
	sources := map[string]io.ReadSeeker{
		"reader at": strings.NewReader(payload),
		"seek only": &failingSeekBody{reader: strings.NewReader(payload)},
	}
	for name, rs := range sources {
		t.Run(name, func(t *testing.T) {
			src := &replaySource{rs: rs}
			readers := []io.Reader{src.reader(0, -1), src.reader(0, int64(len(payload)))}
			got := make([][]byte, len(readers))
			buf := make([]byte, 3)
			for active := len(readers); active > 0; {
				active = 0
				for i, r := range readers {
					n, err := r.Read(buf)
					got[i] = append(got[i], buf[:n]...)
					if err == nil {
						active++
					} else if err != io.EOF {
						t.Fatalf("reader %d: %v", i, err)
					}
				}
			}
			for i := range got {
				if string(got[i]) != payload {
					t.Fatalf("reader %d read %q, want %q", i, got[i], payload)
				}
			}
		})
	}
}

func TestOffsetReader_HonorsRange(t *testing.T) {
	src := &replaySource{rs: strings.NewReader("0123456789")}
	tests := []struct {
		start, end int64
		want       string
	}{
		{start: 2, end: 5, want: "234"},
		{start: 0, end: 0, want: ""},
		{start: 7, end: -1, want: "789"},
	}
	for _, test := range tests {
		got, err := io.ReadAll(src.reader(test.start, test.end))
		if err != nil || string(got) != test.want {
			t.Errorf("reader(%d, %d) = (%q, %v), want %q", test.start, test.end, got, err, test.want)
		}
	}
}

func TestPrepareBody_UnknownSizeReplaysToEOF(t *testing.T) {
	// The second seek, which probes the end of the body, fails.
	body := &failingSeekBody{reader: strings.NewReader("payload"), err: errors.New("no size"), failAt: 2}
	getBody, cleanup, err := NewTransport().prepareBody(&http.Request{Body: body, ContentLength: -1}, true)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	defer cleanup()
	assertBodyReplay(t, getBody, "payload")
	assertBodyReplay(t, getBody, "payload")
}

func TestRequestBody_GetBodyOnlyWhenReplayable(t *testing.T) {
	replayable := newRequestBody(func() (io.ReadCloser, error) { return http.NoBody, nil }, nil, nil)
	if replayable.getBody() == nil {
		t.Fatal("replayable body has no GetBody")
	}
	streaming := newRequestBody(nil, strings.NewReader("stream"), nil)
	if streaming.getBody() != nil {
		t.Fatal("streaming body exposes GetBody, which would replay a consumed stream")
	}
}

// readAfterCloseBody fails reads after Close, like a network request body.
type readAfterCloseBody struct {
	r      *strings.Reader
	closed atomic.Bool
}

func (b *readAfterCloseBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, errors.New("read after close")
	}
	return b.r.Read(p)
}

func (b *readAfterCloseBody) Close() error {
	b.closed.Store(true)
	return nil
}

// A streaming body that cannot be replayed must survive a connection failure
// before anything was sent, even though http.Transport closes the request body
// of the failed attempt.
func TestTransport_StreamingBodySurvivesPreSendFailover(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	}))
	t.Cleanup(live.Close)

	tr, err := NewTransportE(
		WithRoundTripper(&http.Transport{}),
		WithUpstreams(Upstream("http://svc.test", Server("http://"+dead), Server(live.URL))),
	)
	if err != nil {
		t.Fatal(err)
	}
	body := &readAfterCloseBody{r: strings.NewReader("streamed payload")}
	req, err := http.NewRequest(http.MethodPost, "http://svc.test/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(got) != "streamed payload" {
		t.Fatalf("live backend received (%q, %v), want the full payload", got, err)
	}
	waitFor(t, "request body close", body.closed.Load)
}

func TestRequestBody_ReleasesAfterRoundTripAndEveryAttempt(t *testing.T) {
	cleanups := 0
	body := newRequestBody(func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("x")), nil
	}, nil, func() { cleanups++ })

	first, err := body.next()
	if err != nil {
		t.Fatal(err)
	}
	second, err := body.next()
	if err != nil {
		t.Fatal(err)
	}
	body.release() // RoundTrip returned
	first.Close()
	first.Close()
	if cleanups != 0 {
		t.Fatalf("cleanup ran with an attempt body still open (%d runs)", cleanups)
	}
	second.Close()
	if cleanups != 1 {
		t.Fatalf("cleanup runs = %d, want 1", cleanups)
	}
	if _, err := body.next(); !errors.Is(err, errRequestBodyReleased) {
		t.Fatalf("next after release = %v, want %v", err, errRequestBodyReleased)
	}
}

func TestRequestBody_ReleaseNowDoesNotWaitForAttempts(t *testing.T) {
	cleanups := 0
	body := newRequestBody(nil, strings.NewReader("stream"), func() { cleanups++ })
	attempt, err := body.next()
	if err != nil {
		t.Fatal(err)
	}
	body.releaseNow()
	if cleanups != 1 {
		t.Fatalf("cleanup runs after releaseNow = %d, want 1", cleanups)
	}
	attempt.Close()
	body.releaseNow()
	if cleanups != 1 {
		t.Fatalf("cleanup runs after late close = %d, want 1", cleanups)
	}
}

func TestRequestBody_UntrackedReplayBodies(t *testing.T) {
	for name, rc := range map[string]io.ReadCloser{"nil": nil, "NoBody": http.NoBody} {
		t.Run(name, func(t *testing.T) {
			cleanups := 0
			body := newRequestBody(func() (io.ReadCloser, error) { return rc, nil }, nil, func() { cleanups++ })
			got, err := body.next()
			if err != nil || got != rc {
				t.Fatalf("next = (%v, %v), want (%v, nil)", got, err, rc)
			}
			body.release()
			if cleanups != 1 {
				t.Fatalf("cleanup runs = %d, want 1", cleanups)
			}
		})
	}
}

// nginx calls peer.free(NGX_PEER_FAILED) for every transport error, whether or
// not proxy_next_upstream lets it move to another server.
func TestTransport_NonRetriedTransportErrorCountsAsFailure(t *testing.T) {
	dead := errors.New("connection refused")
	var calls []string
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Host)
		if req.URL.Host == "dead:1" {
			return nil, dead
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
	})

	tr, err := NewTransportE(
		WithRoundTripper(rt),
		// Never move to the next peer, like "proxy_next_upstream off".
		WithNextUpstream(func(*http.Response, error) bool { return false }),
		WithUpstreams(Upstream("http://svc.test",
			Server("http://dead:1", MaxFails(1), FailTimeout(time.Hour)),
			Server("http://live:1"),
		)),
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 6; i++ {
		req, _ := http.NewRequest(http.MethodGet, "http://svc.test/", nil)
		resp, err := tr.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	deadHits := 0
	for _, h := range calls {
		if h == "dead:1" {
			deadHits++
		}
	}
	if deadHits != 1 {
		t.Fatalf("dead peer received %d of %d requests; want 1 (calls: %s)",
			deadHits, len(calls), strings.Join(calls, ","))
	}
}

func TestTransport_HugeWeightsAreRejected(t *testing.T) {
	_, err := NewTransportE(
		WithUpstreams(Upstream("http://svc.test",
			Server("http://a:1", Weight(1<<31)),
			Server("http://b:1", Weight(1<<31)),
		).Hash(func(*http.Request) string { return "k" })),
	)
	if err == nil || !strings.Contains(err.Error(), "weight must not exceed") {
		t.Fatalf("NewTransportE error = %v, want per-server weight limit", err)
	}
}

func TestTransport_TotalWeightLimits(t *testing.T) {
	many := make([]ServerConfig, maxUpstreamWeight/maxServerWeight+1)
	for i := range many {
		many[i] = Server(fmt.Sprintf("10.0.%d.%d:80", i/256, i%256), Weight(maxServerWeight))
	}
	tests := map[string]UpstreamConfig{
		"upstream": Upstream("http://svc.test", many...),
		"consistent hash": Upstream("http://svc.test",
			Server("http://a:1", Weight(maxServerWeight)),
			Server("http://b:1"),
		).HashConsistent(func(*http.Request) string { return "k" }),
	}
	for name, upstream := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewTransportE(WithUpstreams(upstream))
			if err == nil || !strings.Contains(err.Error(), "total server weight") {
				t.Fatalf("NewTransportE error = %v, want total weight limit", err)
			}
		})
	}

	atLimit := Upstream("http://svc.test", many[:len(many)-1]...)
	if _, err := NewTransportE(WithUpstreams(atLimit)); err != nil {
		t.Fatalf("upstream at the total weight limit rejected: %v", err)
	}
}

func TestHashPickers_FallBackFromOutOfRangeTotalWeight(t *testing.T) {
	totals := []int{0}
	if strconv.IntSize == 64 {
		totals = append(totals, 1<<32)
	}
	for _, total := range totals {
		hashPicker := NewHash([]*Peer{{Addr: "a"}, {Addr: "b"}}).NewPickerForKey("key").(*hashPicker)
		hashPicker.PrimaryGroup().TotalWeight = total
		if peer := hashPicker.Pick(); peer == nil {
			t.Fatalf("hash picker with total weight %d did not fall back to round-robin", total)
		}

		ipHashPicker := NewIPHash([]*Peer{{Addr: "a"}, {Addr: "b"}}).NewPickerForIP(net.IPv4(10, 0, 0, 1)).(*ipHashPicker)
		ipHashPicker.PrimaryGroup().TotalWeight = total
		if peer := ipHashPicker.Pick(); peer == nil {
			t.Fatalf("IP hash picker with total weight %d did not fall back to round-robin", total)
		}
	}
}

func TestBuildChashRing_RefusesOversizedRing(t *testing.T) {
	small := []*Peer{{Addr: "a:1", Weight: 2}, {Addr: "b:1", Weight: 1}}
	if ring := buildChashRing(small, 3); len(ring) == 0 {
		t.Fatal("ring at the weight limit is empty")
	}
	if ring := buildChashRing(small, 2); ring != nil {
		t.Fatalf("ring over the weight limit has %d points, want none", len(ring))
	}

	peers := []*Peer{
		{Addr: "a:1", Weight: maxConsistentHashWeight},
		{Addr: "b:1", Weight: 1},
	}
	if peer := NewHashConsistent(peers).NewPickerForKey("key").Pick(); peer == nil {
		t.Fatal("consistent hash without a ring did not fall back to round-robin")
	}
}

func TestNewTransportE_ValidatesOrigins(t *testing.T) {
	invalidBackends := []string{
		"ftp://10.0.0.1:21",         // unsupported scheme
		"http://10.0.0.1:8080/api",  // path silently dropped
		"10.0.0.1:8080/api",         // no scheme: whole string became URL.Host
		"http://user:pw@10.0.0.1:1", // userinfo silently dropped
		"http://10.0.0.1:1?x=1",     // query
		"http://10.0.0.1:1?",        // empty query
		"http://10.0.0.1:1#frag",    // fragment
		"http://:8080",              // missing host
		"http://[::1",               // unparsable
	}
	for _, addr := range invalidBackends {
		_, err := NewTransportE(WithUpstreams(Upstream("http://svc.test", Server(addr))))
		if err == nil || !strings.Contains(err.Error(), "invalid backend address") {
			t.Errorf("backend %q: error = %v, want invalid backend address", addr, err)
		}
	}

	for _, addr := range []string{"10.0.0.1:8080", "localhost", "http://10.0.0.1:8080/", "https://[::1]:8443"} {
		if _, err := NewTransportE(WithUpstreams(Upstream("http://svc.test", Server(addr)))); err != nil {
			t.Errorf("backend %q rejected: %v", addr, err)
		}
	}

	for _, pattern := range []string{"//svc.test", "ftp://svc.test", "http://svc.test/api"} {
		_, err := NewTransportE(WithUpstreams(Upstream(pattern, Server("10.0.0.1:80"))))
		if err == nil || !strings.Contains(err.Error(), "invalid upstream pattern") {
			t.Errorf("pattern %q: error = %v, want invalid upstream pattern", pattern, err)
		}
	}
}

func TestTransport_CloseIdleConnectionsReachesEveryTransport(t *testing.T) {
	var nilTransport *Transport
	nilTransport.CloseIdleConnections()

	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// ProxySSLName gives the upstream its own cloned transport; the httptest
	// certificate is valid for example.com.
	tr, err := NewTransportE(
		WithRoundTripper(srv.Client().Transport),
		WithUpstreams(Upstream("https://svc.test", Server(srv.URL)).ProxySSLName("example.com")),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://svc.test/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	tr.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("idle upstream connection was not closed")
	}

	// A RoundTripper without CloseIdleConnections is skipped.
	NewTransport(WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}))).CloseIdleConnections()
}

func TestTransport_MatchesOriginsCaseInsensitivelyWithoutDefaultPort(t *testing.T) {
	var hosts []string
	tr, err := NewTransportE(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			hosts = append(hosts, req.URL.Host+" "+req.Host)
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
		})),
		WithUpstreams(
			Upstream("http://svc.test", Server("10.0.0.1:80")),
			Upstream("https://secure.test:443", Server("10.0.0.2:443")),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"http://SVC.test:80/a", "HTTPS://secure.TEST/b"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip %s: %v", target, err)
		}
		resp.Body.Close()
	}
	want := []string{"10.0.0.1:80 SVC.test:80", "10.0.0.2:443 secure.TEST"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Fatalf("attempts = %q, want %q", hosts, want)
	}

	_, err = NewTransportE(WithUpstreams(
		Upstream("http://svc.test", Server("10.0.0.1:80")),
		Upstream("HTTP://SVC.TEST:80", Server("10.0.0.2:80")),
	))
	if err == nil || !strings.Contains(err.Error(), "duplicate upstream") {
		t.Fatalf("NewTransportE error = %v, want duplicate upstream", err)
	}
}

func TestRRPicker_DoneIsIdempotent(t *testing.T) {
	a, b := &Peer{Addr: "a"}, &Peer{Addr: "b"}
	rr := NewRoundRobin([]*Peer{a, b})

	picker := rr.NewPicker()
	picked := picker.Pick()
	other := b
	if picked == b {
		other = a
	}
	picker.Done(picked, false)
	picker.Done(picked, false)
	picker.Done(other, false) // never selected by this picker
	picker.Done(nil, false)
	if a.conns != 0 || b.conns != 0 {
		t.Fatalf("conns = a:%d b:%d, want 0 after repeated Done", a.conns, b.conns)
	}

	excluding := NewRRPicker(rr.Primary(), rr.Backup())
	excluding.SetTried(a)
	if got := excluding.Pick(); got != b {
		t.Fatalf("Pick = %v, want b after SetTried(a)", got)
	}
	if got := excluding.Pick(); got != nil {
		t.Fatalf("Pick = %v, want nil once every peer is tried", got)
	}
	excluding.Done(a, true) // tried, but never selected
	excluding.Done(b, false)
	if a.conns != 0 || b.conns != 0 || a.fails != 0 {
		t.Fatalf("state = a:{conns %d fails %d} b:{conns %d}, want untouched", a.conns, a.fails, b.conns)
	}
}

func TestPickWithRetry_ReportsEachOutcome(t *testing.T) {
	a, b := &Peer{Addr: "a"}, &Peer{Addr: "b"}
	balancer := NewRoundRobin([]*Peer{a, b})
	got, err := PickWithRetry(balancer, func(addr string) error {
		if addr == "a" {
			return errors.New("down")
		}
		return nil
	})
	if err != nil || got != b {
		t.Fatalf("PickWithRetry = (%v, %v), want b", got, err)
	}
	if a.fails != 1 || b.fails != 0 || a.conns != 0 || b.conns != 0 {
		t.Fatalf("state = a:{fails %d conns %d} b:{fails %d conns %d}, want a failed and b succeeded",
			a.fails, a.conns, b.fails, b.conns)
	}
}

func TestPickWithRetry_PanicReleasesPeer(t *testing.T) {
	peer := &Peer{Addr: "a"}
	balancer := NewRoundRobin([]*Peer{peer, {Addr: "b"}})

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("tryFunc panic was swallowed")
			}
		}()
		PickWithRetry(balancer, func(string) error { panic("boom") })
	}()

	for _, p := range balancer.Primary().Peers {
		if p.conns != 0 || p.fails != 0 {
			t.Fatalf("peer %s: conns %d fails %d, want a neutral release", p.Addr, p.conns, p.fails)
		}
	}
}
