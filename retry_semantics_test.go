package cslb

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

func retryTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

func TestTransport_DefaultDoesNotRetryHTTPStatus(t *testing.T) {
	attempts := 0
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return retryTestResponse(req, http.StatusInternalServerError, "first"), nil
			}
			return retryTestResponse(req, http.StatusOK, "second"), nil
		})),
		WithUpstreams(
			Upstream("http://default-status.local",
				Server("http://10.0.0.1:8080"),
				Server("http://10.0.0.2:8080"),
			),
		),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://default-status.local/test", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}

	rr := transport.upstreams[upstreamKey{scheme: "http", host: "default-status.local"}].balancer.(*RoundRobin)
	if got := rr.primary.Peers[0].fails; got != 0 {
		t.Fatalf("first peer fails = %d, want 0", got)
	}
}

func TestTransport_DefaultRetriesTransportError(t *testing.T) {
	attempts := 0
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("dial failed")
			}
			return retryTestResponse(req, http.StatusOK, "ok"), nil
		})),
		WithUpstreams(
			Upstream("http://default-error.local",
				Server("http://10.0.0.1:8080"),
				Server("http://10.0.0.2:8080"),
			),
		),
	)

	req, _ := http.NewRequest(http.MethodGet, "http://default-error.local/test", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestTransport_DecideAttemptDefendsAgainstNilResponse(t *testing.T) {
	transport := &Transport{
		nextUpstream: func(*http.Response, error) bool { return true },
	}
	req, _ := http.NewRequest(http.MethodGet, "http://nil-response.local/test", nil)

	decision := transport.decideAttempt(req, nil, nil, false)
	if !decision.retry || !decision.failed {
		t.Fatalf("decision = %+v, want retry and failed", decision)
	}
}

func TestTransport_NonIdempotentResponseRetryGate(t *testing.T) {
	tests := []struct {
		name                 string
		method               string
		nonIdempotentRetries bool
		wantAttempts         int
		wantStatus           int
		wantFails            int
	}{
		{name: "post blocked", method: http.MethodPost, wantAttempts: 1, wantStatus: 500},
		{name: "lock blocked", method: "LOCK", wantAttempts: 1, wantStatus: 500},
		{name: "patch blocked", method: http.MethodPatch, wantAttempts: 1, wantStatus: 500},
		{name: "put retries", method: http.MethodPut, wantAttempts: 2, wantStatus: 200, wantFails: 1},
		{name: "post opt in", method: http.MethodPost, nonIdempotentRetries: true, wantAttempts: 2, wantStatus: 200, wantFails: 1},
		{name: "lock opt in", method: "LOCK", nonIdempotentRetries: true, wantAttempts: 2, wantStatus: 200, wantFails: 1},
		{name: "patch opt in", method: http.MethodPatch, nonIdempotentRetries: true, wantAttempts: 2, wantStatus: 200, wantFails: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			opts := []TransportOption{
				WithNextUpstreamCodes(http.StatusInternalServerError),
				WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						return retryTestResponse(req, http.StatusInternalServerError, "first"), nil
					}
					return retryTestResponse(req, http.StatusOK, "second"), nil
				})),
				WithUpstreams(
					Upstream("http://method-gate.local",
						Server("http://10.0.0.1:8080"),
						Server("http://10.0.0.2:8080"),
					),
				),
			}
			if test.nonIdempotentRetries {
				opts = append(opts, WithNonIdempotentRetries())
			}
			transport := NewTransport(opts...)

			req, _ := http.NewRequest(test.method, "http://method-gate.local/test", strings.NewReader("payload"))
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			resp.Body.Close()

			if attempts != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, test.wantAttempts)
			}
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, test.wantStatus)
			}

			rr := transport.upstreams[upstreamKey{scheme: "http", host: "method-gate.local"}].balancer.(*RoundRobin)
			if got := rr.primary.Peers[0].fails; got != test.wantFails {
				t.Fatalf("first peer fails = %d, want %d", got, test.wantFails)
			}
		})
	}
}

func TestTransport_NonIdempotentConnectFailureBeforeSendRetries(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("body = %q, want payload", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	dialer := &net.Dialer{}
	underlying := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == "before-send.invalid:80" {
				return nil, errors.New("dial failed before request write")
			}
			return dialer.DialContext(ctx, network, address)
		},
	}
	defer underlying.CloseIdleConnections()
	transport := NewTransport(
		WithRoundTripper(underlying),
		WithUpstreams(
			Upstream("http://connect-before-send.local",
				Server("http://before-send.invalid"),
				Server(good.URL),
			),
		),
	)

	client := &http.Client{Transport: transport}
	resp, err := client.Post(
		"http://connect-before-send.local/test",
		"text/plain",
		strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestTransport_SentNonIdempotentErrorGate(t *testing.T) {
	for _, test := range []struct {
		name         string
		enableRetry  bool
		wantAttempts int
		wantError    bool
	}{
		{name: "blocked but counted as failure", wantAttempts: 1, wantError: true},
		{name: "explicitly enabled", enableRetry: true, wantAttempts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			opts := []TransportOption{
				WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						trace := httptrace.ContextClientTrace(req.Context())
						if trace == nil || trace.WroteHeaders == nil {
							t.Fatal("request-sent trace was not installed")
						}
						trace.WroteHeaders()
						return nil, errors.New("connection reset after write")
					}
					return retryTestResponse(req, http.StatusOK, "ok"), nil
				})),
				WithUpstreams(
					Upstream("http://sent-error.local",
						Server("http://10.0.0.1:8080"),
						Server("http://10.0.0.2:8080"),
					),
				),
			}
			if test.enableRetry {
				opts = append(opts, WithNonIdempotentRetries())
			}
			transport := NewTransport(opts...)

			req, _ := http.NewRequest(http.MethodPost, "http://sent-error.local/test", strings.NewReader("payload"))
			resp, err := transport.RoundTrip(req)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %v", err, test.wantError)
			}
			if resp != nil {
				resp.Body.Close()
			}
			if attempts != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, test.wantAttempts)
			}

			rr := transport.upstreams[upstreamKey{scheme: "http", host: "sent-error.local"}].balancer.(*RoundRobin)
			if got := rr.primary.Peers[0].fails; got != 1 {
				t.Fatalf("first peer fails = %d, want 1", got)
			}
		})
	}
}

func TestTransport_Configured403And404DoNotPenalizePeer(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			attempts := 0
			transport := NewTransport(
				WithNextUpstreamCodes(status),
				WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						return retryTestResponse(req, status, "next"), nil
					}
					return retryTestResponse(req, http.StatusOK, "ok"), nil
				})),
				WithUpstreams(
					Upstream("http://status-health.local",
						Server("http://10.0.0.1:8080", Weight(10)),
						Server("http://10.0.0.2:8080", Weight(10)),
					),
				),
			)

			req, _ := http.NewRequest(http.MethodGet, "http://status-health.local/test", nil)
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			resp.Body.Close()

			rr := transport.upstreams[upstreamKey{scheme: "http", host: "status-health.local"}].balancer.(*RoundRobin)
			first := rr.primary.Peers[0]
			if first.fails != 0 {
				t.Fatalf("first peer fails = %d, want 0", first.fails)
			}
			if first.effectiveWeight != first.Weight {
				t.Fatalf("first peer effective weight = %d, want %d", first.effectiveWeight, first.Weight)
			}
		})
	}
}

func TestTransport_CallerCancellationDoesNotRetryOrPenalizePeers(t *testing.T) {
	var cancel context.CancelFunc
	attempts := 0
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				cancel()
				return nil, req.Context().Err()
			}
			return retryTestResponse(req, http.StatusOK, "ok"), nil
		})),
		WithUpstreams(
			Upstream("http://caller-cancel.local",
				Server("http://10.0.0.1:8080"),
				Server("http://10.0.0.2:8080"),
			),
		),
	)

	ctx, cancelRequest := context.WithCancel(context.Background())
	cancel = cancelRequest
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://caller-cancel.local/test", nil)
	_, err := transport.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}

	rr := transport.upstreams[upstreamKey{scheme: "http", host: "caller-cancel.local"}].balancer.(*RoundRobin)
	for _, peer := range rr.primary.Peers {
		if peer.fails != 0 || peer.conns != 0 {
			t.Fatalf("peer %q state: fails=%d conns=%d, want zero", peer.Addr, peer.fails, peer.conns)
		}
	}

	retryReq, _ := http.NewRequest(http.MethodGet, "http://caller-cancel.local/test", nil)
	resp, err := transport.RoundTrip(retryReq)
	if err != nil {
		t.Fatalf("subsequent RoundTrip: %v", err)
	}
	resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts after subsequent request = %d, want 2", attempts)
	}
}

func TestTransport_CallerCancellationDoesNotHealRecoveringPeer(t *testing.T) {
	var cancel context.CancelFunc
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			return nil, req.Context().Err()
		})),
		WithUpstreams(
			Upstream("http://cancel-probe.local",
				Server("http://10.0.0.1:8080", MaxFails(1), FailTimeout(time.Millisecond)),
				Server("http://10.0.0.2:8080"),
			),
		),
	)

	rr := transport.upstreams[upstreamKey{scheme: "http", host: "cancel-probe.local"}].balancer.(*RoundRobin)
	recovering := rr.primary.Peers[0]
	recovering.fails = 1
	recovering.accessed = time.Now().Add(-time.Second)
	recovering.checked = recovering.accessed

	ctx, cancelRequest := context.WithCancel(context.Background())
	cancel = cancelRequest
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://cancel-probe.local/test", nil)
	_, err := transport.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if recovering.fails != 1 {
		t.Fatalf("recovering peer fails = %d, want unchanged value 1", recovering.fails)
	}
	if recovering.conns != 0 {
		t.Fatalf("recovering peer connections = %d, want 0", recovering.conns)
	}
}

func TestTransport_PreCanceledRequestDoesNotReachRoundTripper(t *testing.T) {
	attempts := 0
	body := newTrackingBody("payload")
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			return nil, req.Context().Err()
		})),
		WithUpstreams(
			Upstream("http://pre-cancel.local",
				Server("http://10.0.0.1:8080"),
				Server("http://10.0.0.2:8080"),
			),
		),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://pre-cancel.local/test", body)
	_, err := transport.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0", attempts)
	}
	if !body.closed {
		t.Fatal("pre-canceled request body was not closed")
	}
}
