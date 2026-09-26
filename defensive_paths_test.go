package cslb

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixedBalancer struct {
	picker Picker
}

func (b fixedBalancer) NewPicker() Picker {
	return b.picker
}

type fixedPicker struct {
	peer      *Peer
	doneCalls int
	failed    bool
}

func (p *fixedPicker) Pick() *Peer {
	return p.peer
}

func (p *fixedPicker) Done(_ *Peer, failed bool) {
	p.doneCalls++
	p.failed = failed
}

func TestPickHelpers_HandleUnavailablePickers(t *testing.T) {
	var result *PickResult
	result.Done(false)
	(&PickResult{}).Done(false)

	if got := PickOne(fixedBalancer{}); got != nil {
		t.Fatalf("PickOne with nil picker = %v, want nil", got)
	}
	if got := PickOne(fixedBalancer{picker: &fixedPicker{}}); got != nil {
		t.Fatalf("PickOne with unavailable picker = %v, want nil", got)
	}

	peer, err := PickWithRetry(fixedBalancer{}, func(string) error { return nil })
	if peer != nil || err != ErrNoPeerAvailable {
		t.Fatalf("PickWithRetry with nil picker = (%v, %v), want (nil, ErrNoPeerAvailable)", peer, err)
	}
	if got := ErrNoPeerAvailable.Error(); got != "no peer available" {
		t.Fatalf("ErrNoPeerAvailable.Error() = %q", got)
	}
}

func TestBalancerGroupAccessors(t *testing.T) {
	primary := &Peer{Addr: "primary", Weight: 1}
	backup := &Peer{Addr: "backup", Weight: 1, Backup: true}

	roundRobin := NewRoundRobin([]*Peer{primary, backup})
	if roundRobin.Primary() != roundRobin.primary {
		t.Fatal("RoundRobin.Primary returned the wrong group")
	}
	if roundRobin.Backup() != roundRobin.backup {
		t.Fatal("RoundRobin.Backup returned the wrong group")
	}

	consistent := NewHashConsistent([]*Peer{
		{Addr: "primary", Weight: 1},
		{Addr: "backup", Weight: 1, Backup: true},
	})
	if consistent.Primary() != consistent.rr.primary {
		t.Fatal("HashConsistent.Primary returned the wrong group")
	}
	if consistent.Backup() != consistent.rr.backup {
		t.Fatal("HashConsistent.Backup returned the wrong group")
	}

	picker := consistent.NewPicker()
	peer := picker.Pick()
	if peer == nil || peer.Backup {
		t.Fatalf("HashConsistent.NewPicker fallback selected %v, want primary peer", peer)
	}
	picker.Done(peer, false)
}

func TestBuildChashRing_SkipsInvalidEntries(t *testing.T) {
	if ring := buildChashRing([]*Peer{nil, {Addr: "zero", Weight: 0}}, maxConsistentHashWeight); ring != nil {
		t.Fatalf("invalid peers produced ring points: %v", ring)
	}

	valid := &Peer{Addr: "10.0.0.1:8080", Weight: 1}
	ring := buildChashRing([]*Peer{nil, {Addr: "zero", Weight: 0}, valid}, maxConsistentHashWeight)
	if len(ring) == 0 {
		t.Fatal("valid peer did not produce ring points")
	}
	for _, point := range ring {
		if point.peer != valid {
			t.Fatalf("ring contains point for invalid peer %v", point.peer)
		}
	}

	host, port := splitKetamaHostPort("")
	if host != "" || port != "" {
		t.Fatalf("splitKetamaHostPort(\"\") = (%q, %q)", host, port)
	}
}

func TestRoundRobin_DefensiveSelectionPaths(t *testing.T) {
	atCapacity := &Peer{Addr: "full", Weight: 1, MaxConns: 1}
	available := &Peer{Addr: "available", Weight: 1}
	roundRobin := NewRoundRobin([]*Peer{atCapacity, available})
	atCapacity.conns = 1

	picker := roundRobin.NewPicker()
	if got := picker.Pick(); got != available {
		t.Fatalf("Pick = %v, want peer below max_conns", got)
	} else {
		picker.Done(got, false)
	}
	picker.Done(nil, false)

	clamped := NewRoundRobin([]*Peer{
		{Addr: "clamped", Weight: 1, MaxFails: 1},
		{Addr: "other", Weight: 1},
	})
	failurePicker := clamped.NewPicker()
	failed := failurePicker.Pick()
	if failed == nil {
		t.Fatal("Pick returned nil")
	}
	failed.effectiveWeight = 0
	failurePicker.Done(failed, true)
	if failed.effectiveWeight != 0 {
		t.Fatalf("effective weight = %d, want clamp at zero", failed.effectiveWeight)
	}
}

func TestPeerAvailable_EnforcesFailureAndConnectionLimits(t *testing.T) {
	quarantined := &Peer{
		MaxFails:    1,
		FailTimeout: time.Minute,
		fails:       1,
		checked:     time.Now(),
	}
	if peerAvailable(quarantined) {
		t.Fatal("quarantined peer reported available")
	}

	atCapacity := &Peer{MaxConns: 1, conns: 1}
	if peerAvailable(atCapacity) {
		t.Fatal("peer at max_conns reported available")
	}
	if !peerAvailable(&Peer{}) {
		t.Fatal("unlimited healthy peer reported unavailable")
	}
}

func TestLeastConn_SkipsUnavailablePeers(t *testing.T) {
	quarantined := &Peer{Addr: "quarantined", Weight: 1, MaxFails: 1, FailTimeout: time.Minute}
	atCapacity := &Peer{Addr: "full", Weight: 1, MaxConns: 1}
	available := &Peer{Addr: "available", Weight: 1}
	balancer := NewLeastConn([]*Peer{quarantined, atCapacity, available})
	quarantined.fails = 1
	quarantined.checked = time.Now()
	atCapacity.conns = 1

	picker := balancer.NewPicker()
	if got := picker.Pick(); got != available {
		t.Fatalf("Pick = %v, want available peer", got)
	} else {
		picker.Done(got, false)
	}
}

func TestLeastConn_TieBreakSkipsEveryUnavailablePeerType(t *testing.T) {
	down := &Peer{Addr: "down", Weight: 1, Down: true}
	quarantined := &Peer{
		Addr:        "quarantined",
		Weight:      1,
		MaxFails:    1,
		FailTimeout: time.Minute,
	}
	atCapacity := &Peer{Addr: "full", Weight: 1, MaxConns: 1}
	availableA := &Peer{Addr: "available-a", Weight: 1}
	availableB := &Peer{Addr: "available-b", Weight: 1}

	balancer := NewLeastConn([]*Peer{
		down,
		quarantined,
		atCapacity,
		availableA,
		availableB,
	})
	for _, peer := range []*Peer{down, quarantined, atCapacity, availableA, availableB} {
		peer.conns = 1
	}
	quarantined.fails = 1
	quarantined.checked = time.Now()

	picker := balancer.NewPicker()
	peer := picker.Pick()
	if peer != availableA && peer != availableB {
		t.Fatalf("Pick = %v, want one of the available tied peers", peer)
	}
	picker.Done(peer, false)
}

func TestHashBasedPickers_FallBackAfterRejectedCandidates(t *testing.T) {
	newDownPeers := func() []*Peer {
		return []*Peer{
			{Addr: "down-a", Weight: 1, Down: true},
			{Addr: "down-b", Weight: 1, Down: true},
		}
	}

	tests := []struct {
		name   string
		picker Picker
	}{
		{name: "hash", picker: NewHash(newDownPeers()).NewPickerForKey("key")},
		{name: "ip hash", picker: NewIPHash(newDownPeers()).NewPickerForIP([]byte{10, 0, 0, 1})},
		{name: "random", picker: NewRandom(newDownPeers()).NewPicker()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.picker.Pick(); got != nil {
				test.picker.Done(got, false)
				t.Fatalf("Pick = %v, want nil after all candidates were rejected", got)
			}
		})
	}

	random := NewRandom([]*Peer{{Addr: "a", Weight: 1}, {Addr: "b", Weight: 1}})
	randomPicker := random.NewPicker().(*randomPicker)
	if got := randomPicker.randomSelect(&PeerGroup{}); got != nil {
		t.Fatalf("randomSelect with zero total weight = %v, want nil", got)
	}
}

func TestHashPickers_FallBackFromInconsistentTotalWeight(t *testing.T) {
	hash := NewHash([]*Peer{
		{Addr: "a", Weight: 1},
		{Addr: "b", Weight: 1},
	})
	hashPicker := hash.NewPickerForKey("key").(*hashPicker)
	hashPicker.PrimaryGroup().TotalWeight = 1 << 20
	hashPicker.hash = 100
	hashPeer := hashPicker.Pick()
	if hashPeer == nil {
		t.Fatal("hash picker did not fall back to round-robin")
	}
	hashPicker.Done(hashPeer, false)

	ipHash := NewIPHash([]*Peer{
		{Addr: "a", Weight: 1},
		{Addr: "b", Weight: 1},
	})
	ipHashPicker := ipHash.NewPickerForIP([]byte{10, 0, 0, 1}).(*ipHashPicker)
	ipHashPicker.PrimaryGroup().TotalWeight = 1 << 20
	ipHashPicker.hash = 100
	ipHashPeer := ipHashPicker.Pick()
	if ipHashPeer == nil {
		t.Fatal("IP hash picker did not fall back to round-robin")
	}
	ipHashPicker.Done(ipHashPeer, false)
}

func TestRandomPickers_FallBackFromInvalidZeroWeightGroup(t *testing.T) {
	tests := []struct {
		name     string
		balancer *Random
	}{
		{
			name: "random",
			balancer: NewRandom([]*Peer{
				{Addr: "a", Weight: 1},
				{Addr: "b", Weight: 1},
			}),
		},
		{
			name: "random two",
			balancer: NewRandomTwo([]*Peer{
				{Addr: "a", Weight: 1},
				{Addr: "b", Weight: 1},
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			picker := test.balancer.NewPicker().(*randomPicker)
			picker.PrimaryGroup().TotalWeight = 0

			peer := picker.Pick()
			if peer == nil {
				t.Fatal("Pick did not fall back to round-robin")
			}
			picker.Done(peer, false)
		})
	}
}

func TestTransportAndUpstreamErrorAggregation(t *testing.T) {
	var nilTransport *Transport
	if !errors.Is(nilTransport.Err(), ErrNilRoundTripper) {
		t.Fatalf("nil Transport.Err() = %v, want ErrNilRoundTripper", nilTransport.Err())
	}

	first := errors.New("first")
	second := errors.New("second")
	transport := &Transport{}
	transport.addInitError(nil)
	transport.addInitError(first)
	transport.addInitError(second)
	if !errors.Is(transport.initErr, first) || !errors.Is(transport.initErr, second) {
		t.Fatalf("joined transport error = %v", transport.initErr)
	}

	cfg := newUpstreamConfig("http")
	cfg.addError(nil)
	cfg.addError(first)
	cfg.addError(second)
	if !errors.Is(cfg.err, first) || !errors.Is(cfg.err, second) {
		t.Fatalf("joined upstream error = %v", cfg.err)
	}
}

func TestTransport_ProxySSLNameInitializesTLSConfig(t *testing.T) {
	base := &http.Transport{
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	transport, err := NewTransportE(
		WithRoundTripper(base),
		WithUpstreams(
			Upstream("https://tls-config.local",
				Server("https://127.0.0.1:443"),
			).ProxySSLName("backend.example"),
		),
	)
	if err != nil {
		t.Fatalf("NewTransportE: %v", err)
	}

	ups := transport.upstreams[upstreamKey{scheme: "https", host: "tls-config.local"}]
	cloned, ok := ups.transport.(*http.Transport)
	if !ok {
		t.Fatalf("upstream transport = %T, want *http.Transport", ups.transport)
	}
	if cloned.TLSClientConfig == nil || cloned.TLSClientConfig.ServerName != "backend.example" {
		t.Fatalf("cloned TLS config = %+v, want ServerName backend.example", cloned.TLSClientConfig)
	}
	if base.TLSClientConfig != nil {
		t.Fatal("constructing the upstream mutated the base TLS config")
	}
}

func TestUpstreamConfig_NormalizesHashSettings(t *testing.T) {
	keyFunc := func(*http.Request) string { return "key" }

	implicitHash := newUpstreamConfig("http")
	implicitHash.hashFunc = keyFunc
	implicitHash.normalize()
	if implicitHash.err != nil || implicitHash.algo != AlgoHash {
		t.Fatalf("implicit hash normalized to algo=%v err=%v", implicitHash.algo, implicitHash.err)
	}

	invalid := newUpstreamConfig("http")
	invalid.algo = AlgoLeastConn
	invalid.hashFunc = keyFunc
	invalid.normalize()
	if invalid.err == nil || !strings.Contains(invalid.err.Error(), "hash key function can only be used") {
		t.Fatalf("invalid hash combination error = %v", invalid.err)
	}
}

func TestNewTransportE_RejectsEmptyBackendAndUpstream(t *testing.T) {
	tests := []struct {
		name     string
		upstream UpstreamConfig
		want     string
	}{
		{
			name:     "empty backend address",
			upstream: Upstream("http://service.local", Server("")),
			want:     "backend address must not be empty",
		},
		{
			name:     "no backends",
			upstream: Upstream("http://service.local"),
			want:     "has no backends",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewTransportE(WithUpstreams(test.upstream))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewTransportE error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestTransport_SelectedPeerWithoutBackendAddress(t *testing.T) {
	attempts := 0
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("must not be called")
		})),
		WithUpstreams(Upstream("http://missing-address.local",
			Server("http://127.0.0.1:8080"),
		)),
	)
	ups := transport.upstreams[upstreamKey{scheme: "http", host: "missing-address.local"}]
	var peer *Peer
	for configured := range ups.backends {
		peer = configured
		delete(ups.backends, configured)
	}

	req, err := http.NewRequest(http.MethodGet, "http://missing-address.local/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "has no backend address") {
		t.Fatalf("RoundTrip error = %v", err)
	}
	if attempts != 0 {
		t.Fatalf("underlying RoundTripper attempts = %d, want 0", attempts)
	}
	if peer == nil || peer.conns != 0 {
		t.Fatalf("selected peer was not released: %+v", peer)
	}
}

func TestTransport_GetBodyErrorStopsBeforeRoundTrip(t *testing.T) {
	wantErr := errors.New("cannot replay body")
	attempts := 0
	original := newTrackingBody("payload")
	req, err := http.NewRequest(http.MethodPut, "http://getbody-error.local/test", original)
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		return nil, wantErr
	}

	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("must not be called")
		})),
		WithUpstreams(Upstream("http://getbody-error.local",
			Server("http://127.0.0.1:8080"),
		)),
	)
	if _, err := transport.RoundTrip(req); !errors.Is(err, wantErr) {
		t.Fatalf("RoundTrip error = %v, want %v", err, wantErr)
	}
	if attempts != 0 {
		t.Fatalf("underlying RoundTripper attempts = %d, want 0", attempts)
	}
	if !original.closed {
		t.Fatal("original body was not closed")
	}
}

func TestTransport_CancellationWhileBufferingStopsBeforeSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelOnReadBody{
		reader: strings.NewReader("payload"),
		cancel: cancel,
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		"http://buffer-cancel.local/test",
		body,
	)
	if err != nil {
		t.Fatal(err)
	}

	attempts := 0
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("must not be called")
		})),
		WithUpstreams(Upstream("http://buffer-cancel.local",
			Server("http://127.0.0.1:8080"),
			Server("http://127.0.0.1:8081"),
		)),
	)

	resp, err := transport.RoundTrip(req)
	if resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip = (%v, %v), want (nil, context.Canceled)", resp, err)
	}
	if attempts != 0 {
		t.Fatalf("underlying RoundTripper attempts = %d, want 0", attempts)
	}
	if !body.closed {
		t.Fatal("buffered body was not closed")
	}
}

func TestTransport_PrepareBodyFailureStopsBeforeSelection(t *testing.T) {
	wantErr := errors.New("body position unavailable")
	body := &failingSeekBody{
		reader: strings.NewReader("payload"),
		err:    wantErr,
		failAt: 1,
	}
	req, err := http.NewRequest(
		http.MethodPut,
		"http://prepare-error.local/test",
		body,
	)
	if err != nil {
		t.Fatal(err)
	}

	attempts := 0
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("must not be called")
		})),
		WithUpstreams(Upstream("http://prepare-error.local",
			Server("http://127.0.0.1:8080"),
			Server("http://127.0.0.1:8081"),
		)),
	)

	resp, err := transport.RoundTrip(req)
	if resp != nil || !errors.Is(err, wantErr) {
		t.Fatalf("RoundTrip = (%v, %v), want body seek error", resp, err)
	}
	if attempts != 0 {
		t.Fatalf("underlying RoundTripper attempts = %d, want 0", attempts)
	}
	if !body.closed {
		t.Fatal("body was not closed after prepare failure")
	}
}

func TestTransport_CancellationClosesReturnedResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	responseBody := newTrackingBody("error response")
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     make(http.Header),
				Body:       responseBody,
				Request:    req,
			}, errors.New("upstream failed")
		})),
		WithUpstreams(Upstream("http://cancel-response.local",
			Server("http://127.0.0.1:8080"),
		)),
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"http://cancel-response.local/test",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip = (%v, %v), want (nil, context.Canceled)", resp, err)
	}
	if !responseBody.closed {
		t.Fatal("response body was not closed after cancellation")
	}
}

func TestTransport_DefaultsHostBeforeSending(t *testing.T) {
	var gotHost string
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			gotHost = req.Host
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		})),
		WithUpstreams(Upstream("http://host-default.local",
			Server("http://127.0.0.1:8080"),
		)),
	)

	req, err := http.NewRequest(http.MethodGet, "http://host-default.local/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = ""
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if gotHost != "host-default.local" {
		t.Fatalf("backend request Host = %q, want %q", gotHost, "host-default.local")
	}
}

func TestTransportInternalFallbacks(t *testing.T) {
	picker := &fixedPicker{}
	peer := &Peer{}
	doneNeutral(picker, peer)
	if picker.doneCalls != 1 || picker.failed {
		t.Fatalf("neutral fallback reported doneCalls=%d failed=%v", picker.doneCalls, picker.failed)
	}

	sent := &atomic.Bool{}
	if requestWasSent(http.DefaultTransport, sent, nil, nil) {
		t.Fatal("nil response and nil error reported as sent")
	}

	body := newTrackingBody("payload")
	req, err := http.NewRequest(http.MethodPost, "http://service.local/test", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Transport{}).roundTrip(nil, req); !errors.Is(err, ErrNilRoundTripper) {
		t.Fatalf("roundTrip error = %v, want ErrNilRoundTripper", err)
	}
	if !body.closed {
		t.Fatal("roundTrip did not close the body")
	}
}

func TestPrepareBody_GetBodyWithoutInitialBody(t *testing.T) {
	factory := func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("payload")), nil
	}
	req := &http.Request{GetBody: factory}
	getBody, cleanup, err := NewTransport().prepareBody(req, true)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	if getBody == nil || cleanup != nil {
		t.Fatalf("prepareBody returned getBody=%v cleanup=%v", getBody != nil, cleanup != nil)
	}
	assertBodyReplay(t, getBody, "payload")
}

type prefixThenErrorReader struct {
	sent bool
	err  error
}

func (r *prefixThenErrorReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		p[0] = 'x'
		return 1, nil
	}
	return 0, r.err
}

type cancelOnReadBody struct {
	reader *strings.Reader
	cancel func()
	closed bool
}

func (b *cancelOnReadBody) Read(p []byte) (int, error) {
	b.cancel()
	return b.reader.Read(p)
}

func (b *cancelOnReadBody) Close() error {
	b.closed = true
	return nil
}

// failingSeekBody deliberately does not implement io.ReaderAt, so replay reads
// go through Seek.
type failingSeekBody struct {
	reader *strings.Reader
	err    error
	failAt int
	seeks  int
	closed bool
}

func (b *failingSeekBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *failingSeekBody) Seek(offset int64, whence int) (int64, error) {
	b.seeks++
	if b.seeks == b.failAt {
		return 0, b.err
	}
	return b.reader.Seek(offset, whence)
}

func (b *failingSeekBody) Close() error {
	b.closed = true
	return nil
}

type negativeRemainingSeekBody struct {
	closed bool
}

func (b *negativeRemainingSeekBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *negativeRemainingSeekBody) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		return 10, nil
	case io.SeekEnd:
		return 5, nil
	default:
		return offset, nil
	}
}

func (b *negativeRemainingSeekBody) Close() error {
	b.closed = true
	return nil
}

func TestPrepareBody_SeekFailures(t *testing.T) {
	t.Run("initial position", func(t *testing.T) {
		wantErr := errors.New("current position unavailable")
		body := &failingSeekBody{
			reader: strings.NewReader("payload"),
			err:    wantErr,
			failAt: 1,
		}
		req := &http.Request{Body: body}

		getBody, cleanup, err := NewTransport().prepareBody(req, true)
		if getBody != nil || cleanup != nil || !errors.Is(err, wantErr) {
			t.Fatalf("prepareBody = (%v, %v, %v), want seek error", getBody != nil, cleanup != nil, err)
		}
		if !body.closed {
			t.Fatal("body was not closed after initial seek failure")
		}
	})

	t.Run("restore after size probe", func(t *testing.T) {
		wantErr := errors.New("initial position could not be restored")
		body := &failingSeekBody{
			reader: strings.NewReader("payload"),
			err:    wantErr,
			failAt: 3,
		}
		req := &http.Request{Body: body}

		getBody, cleanup, err := NewTransport().prepareBody(req, true)
		if getBody != nil || cleanup != nil || !errors.Is(err, wantErr) {
			t.Fatalf("prepareBody = (%v, %v, %v), want restore error", getBody != nil, cleanup != nil, err)
		}
		if !body.closed {
			t.Fatal("body was not closed after restore failure")
		}
	})

	t.Run("replay position", func(t *testing.T) {
		wantErr := errors.New("replay position unavailable")
		body := &failingSeekBody{
			reader: strings.NewReader("payload"),
			err:    wantErr,
			failAt: 4,
		}
		req := &http.Request{Body: body, ContentLength: int64(len("payload"))}

		getBody, cleanup, err := NewTransport().prepareBody(req, true)
		if err != nil {
			t.Fatalf("prepareBody: %v", err)
		}
		if getBody == nil || cleanup == nil {
			t.Fatalf("prepareBody returned getBody=%v cleanup=%v", getBody != nil, cleanup != nil)
		}

		replay, err := getBody()
		if err != nil {
			t.Fatalf("getBody: %v", err)
		}
		_, err = io.ReadAll(replay)
		replay.Close()
		if !errors.Is(err, wantErr) {
			t.Fatalf("replay read error = %v, want %v", err, wantErr)
		}
		cleanup()
		if !body.closed {
			t.Fatal("body was not closed by cleanup")
		}
	})
}

func TestPrepareBody_ClampsNegativeRemainingLength(t *testing.T) {
	body := &negativeRemainingSeekBody{}
	req := &http.Request{
		Body:          body,
		ContentLength: 1,
	}

	getBody, cleanup, err := NewTransport().prepareBody(req, true)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	if req.ContentLength != 0 {
		t.Fatalf("ContentLength = %d, want 0", req.ContentLength)
	}
	replay, err := getBody()
	if err != nil {
		t.Fatalf("getBody: %v", err)
	}
	if replay != http.NoBody {
		replay.Close()
		t.Fatalf("getBody returned %T, want http.NoBody", replay)
	}
	cleanup()
	if !body.closed {
		t.Fatal("body was not closed by cleanup")
	}
}

func TestPrepareBody_ReplayAfterCleanupFailsToRead(t *testing.T) {
	body := newTrackingBody("payload")
	req := &http.Request{Body: body}
	transport := NewTransport(WithMaxBodyBuffer(0))

	getBody, cleanup, err := transport.prepareBody(req, true)
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}
	if getBody == nil || cleanup == nil {
		t.Fatalf("prepareBody returned getBody=%v cleanup=%v", getBody != nil, cleanup != nil)
	}
	cleanup()

	replay, err := getBody()
	if err != nil {
		t.Fatalf("getBody: %v", err)
	}
	defer replay.Close()
	if _, err := io.ReadAll(replay); err == nil {
		t.Fatal("replay read succeeded after its temporary file was closed")
	}
}

type faultBodyTempFile struct {
	name     string
	writeErr error
	seekErr  error
	statErr  error
	closed   bool
}

func (f *faultBodyTempFile) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (f *faultBodyTempFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *faultBodyTempFile) Seek(int64, int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	return 0, nil
}

func (f *faultBodyTempFile) Close() error {
	f.closed = true
	return nil
}

func (f *faultBodyTempFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return nil, errors.New("stat is not supported")
}

func (f *faultBodyTempFile) Name() string {
	return f.name
}

func TestPrepareSpilledBody_StatError(t *testing.T) {
	wantErr := errors.New("stat failed")
	file := &faultBodyTempFile{
		name:    filepath.Join(t.TempDir(), "fake-body"),
		statErr: wantErr,
	}

	getBody, cleanup, err := prepareSpilledBody(&http.Request{}, file)
	if getBody != nil || cleanup != nil || !errors.Is(err, wantErr) {
		t.Fatalf("prepareSpilledBody = (%v, %v, %v), want stat error", getBody != nil, cleanup != nil, err)
	}
	if !file.closed {
		t.Fatal("temporary file was not closed after stat failure")
	}
}

func TestReadBody_TemporaryFileOperationErrors(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*faultBodyTempFile, error)
	}{
		{
			name: "write",
			configure: func(file *faultBodyTempFile, err error) {
				file.writeErr = err
			},
		},
		{
			name: "rewind",
			configure: func(file *faultBodyTempFile, err error) {
				file.seekErr = err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wantErr := errors.New(test.name + " failed")
			file := &faultBodyTempFile{
				name: filepath.Join(t.TempDir(), "fake-body"),
			}
			test.configure(file, wantErr)
			transport := NewTransport(WithMaxBodyBuffer(0))

			body, spilled, err := transport.readBodyWithFactory(
				strings.NewReader("payload"),
				func() (bodyTempFile, error) {
					return file, nil
				},
			)
			if body != nil || spilled != nil || !errors.Is(err, wantErr) {
				t.Fatalf("readBodyWithFactory = (%v, %v, %v), want operation error", body, spilled, err)
			}
			if !file.closed {
				t.Fatal("temporary file was not closed after operation failure")
			}
		})
	}
}

func TestReadBody_TemporaryFileErrors(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		setProcessTempDir(t, filepath.Join(t.TempDir(), "missing"))
		transport := NewTransport(WithMaxBodyBuffer(0))
		if _, _, err := transport.readBody(strings.NewReader("x")); err == nil {
			t.Fatal("readBody succeeded with an unusable temporary directory")
		}
	})

	t.Run("copy", func(t *testing.T) {
		tempDir := t.TempDir()
		setProcessTempDir(t, tempDir)
		wantErr := errors.New("source failed")
		transport := NewTransport(WithMaxBodyBuffer(0))
		_, file, err := transport.readBody(&prefixThenErrorReader{err: wantErr})
		if file != nil {
			file.Close()
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("readBody error = %v, want %v", err, wantErr)
		}
		entries, readErr := os.ReadDir(tempDir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("temporary files after copy error = %v, want none", entries)
		}
	})
}
