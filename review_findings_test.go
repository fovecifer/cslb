//go:build review

// Reproduction tests for the findings recorded in REVIEW.md.
//
// These tests are expected to FAIL against the current implementation; each
// one demonstrates a defect. They are kept behind the "review" build tag so the
// regular suite stays green:
//
//	go test -tags review -run '^TestReview' -v ./...
package cslb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fullDuplexCountServer answers with headers first, then drains the request
// body and writes the number of bytes it received. This is the shape of any
// streaming/bidirectional endpoint (upload progress, log tailing, gRPC-like
// streams) that responds before it has consumed the whole request body.
func fullDuplexCountServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("EnableFullDuplex: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
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

// Finding S1 (streaming path): with a single usable backend, RoundTrip wraps
// the caller's body in io.NopCloser and closes the real body in a deferred
// cleanup as soon as RoundTrip returns. A response that arrives before the
// upload finishes therefore aborts the upload. The same request through a
// plain *http.Transport succeeds.
func TestReviewStreamingBodyClosedWhenRoundTripReturns(t *testing.T) {
	srv := fullDuplexCountServer(t)

	const chunk = 64 << 10
	const chunks = 16
	want := strconv.Itoa(chunk * chunks)

	run := func(t *testing.T, rt http.RoundTripper, url string) {
		pr, pw := io.Pipe()
		req, err := http.NewRequest(http.MethodPut, url, pr)
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

// Finding S1 (spill path): with more than one usable peer a non-seekable body
// is buffered; WithMaxBodyBuffer(0) forces it into a temp file. The deferred
// cleanup closes and deletes that file when RoundTrip returns, while
// http.Transport is still streaming it to a backend that already responded.
func TestReviewSpilledBodyClosedWhenRoundTripReturns(t *testing.T) {
	srv := fullDuplexCountServer(t)

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

	// Wrap in a non-seekable reader without GetBody so the spill path is used.
	body := io.NopCloser(io.LimitReader(repeatReader('y'), size))
	req, err := http.NewRequest(http.MethodPut, "http://svc.test/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if string(got) != strconv.Itoa(size) {
		t.Fatalf("server received %q, want %d bytes", got, size)
	}
}

type repeatReader byte

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// Finding G1: nginx calls peer.free(NGX_PEER_FAILED) for every transport
// error, whether or not proxy_next_upstream lets it move to another server.
// cslb reports a non-retried transport error as a success, so a custom
// condition that declines to retry errors never quarantines a dead peer and
// even clears an existing failure counter.
func TestReviewNonRetriedTransportErrorCountsAsSuccess(t *testing.T) {
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
	// With max_fails=1 / fail_timeout=1h the dead peer must be selected once
	// and then skipped; nginx would send it exactly one request here.
	if deadHits != 1 {
		t.Fatalf("dead peer received %d of %d requests; want 1 (calls: %s)",
			deadHits, len(calls), strings.Join(calls, ","))
	}
}

// Finding G2: Weight has no upper bound. Two servers of weight 1<<31 make
// TotalWeight == 1<<32, which truncates to 0 in uint32(group.TotalWeight) and
// panics with an integer divide by zero inside RoundTrip. NewTransportE
// accepted the configuration.
func TestReviewHugeWeightsPanicInsteadOfValidationError(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("needs 64-bit int")
	}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
	})
	tr, err := NewTransportE(
		WithRoundTripper(rt),
		WithUpstreams(Upstream("http://svc.test",
			Server("http://a:1", Weight(1<<31)),
			Server("http://b:1", Weight(1<<31)),
		).Hash(func(*http.Request) string { return "k" })),
	)
	if err != nil {
		return // rejected at construction: acceptable fix
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RoundTrip panicked on a configuration NewTransportE accepted: %v", r)
		}
	}()
	req, _ := http.NewRequest(http.MethodGet, "http://svc.test/", nil)
	resp, err := tr.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
	}
}

// Finding G3: backend addresses are only checked for "non-empty" and, when
// they contain "://", for a scheme and host. An unsupported scheme or a path
// is accepted at construction and only surfaces per request, where it is
// counted against peer health.
func TestReviewInvalidBackendAddressesAccepted(t *testing.T) {
	for _, addr := range []string{
		"ftp://10.0.0.1:21",         // unsupported scheme
		"http://10.0.0.1:8080/api",  // path silently dropped
		"10.0.0.1:8080/api",         // no scheme: whole string becomes URL.Host
		"http://user:pw@10.0.0.1:1", // userinfo silently dropped
	} {
		_, err := NewTransportE(WithUpstreams(Upstream("http://svc.test", Server(addr))))
		if err == nil {
			t.Errorf("NewTransportE accepted backend address %q", addr)
		}
	}
}
