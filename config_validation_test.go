package cslb

import (
	"strings"
	"testing"
	"time"
)

func TestMaxFails_ExplicitZeroDisablesTemporarySuppression(t *testing.T) {
	peer := &Peer{Addr: "A", Weight: 1}
	MaxFails(0)(peer)
	balancer := NewRoundRobin([]*Peer{peer})

	if peer.MaxFails != 0 {
		t.Fatalf("MaxFails = %d, want 0", peer.MaxFails)
	}

	for attempt := 0; attempt < 3; attempt++ {
		picker := balancer.NewPicker()
		got := picker.Pick()
		if got != peer {
			t.Fatalf("attempt %d: Pick = %v, want peer A", attempt+1, got)
		}
		picker.Done(got, true)
	}
}

func TestMaxFails_UnsetStillDefaultsToOne(t *testing.T) {
	peer := &Peer{Addr: "A", Weight: 1}
	NewRoundRobin([]*Peer{peer})

	if peer.MaxFails != 1 {
		t.Fatalf("MaxFails = %d, want default 1", peer.MaxFails)
	}
}

func TestNewTransportE_PreservesExplicitMaxFailsZero(t *testing.T) {
	transport, err := NewTransportE(
		WithUpstreams(
			Upstream("http://service.local",
				Server("http://127.0.0.1:8080", MaxFails(0)),
			),
		),
	)
	if err != nil {
		t.Fatalf("NewTransportE: %v", err)
	}

	ups := transport.upstreams[upstreamKey{scheme: "http", host: "service.local"}]
	roundRobin, ok := ups.balancer.(*RoundRobin)
	if !ok {
		t.Fatalf("balancer type = %T, want *RoundRobin", ups.balancer)
	}
	peer := roundRobin.primary.Peers[0]
	if peer.MaxFails != 0 {
		t.Fatalf("MaxFails = %d, want 0", peer.MaxFails)
	}
}

func TestFailTimeout_ExplicitZeroIsPreserved(t *testing.T) {
	peer := &Peer{Addr: "A", Weight: 1}
	FailTimeout(0)(peer)
	NewRoundRobin([]*Peer{peer})

	if peer.FailTimeout != 0 {
		t.Fatalf("FailTimeout = %v, want 0", peer.FailTimeout)
	}
}

func TestFailTimeout_UnsetStillDefaultsToTenSeconds(t *testing.T) {
	peer := &Peer{Addr: "A", Weight: 1}
	NewRoundRobin([]*Peer{peer})

	if peer.FailTimeout != 10*time.Second {
		t.Fatalf("FailTimeout = %v, want 10s", peer.FailTimeout)
	}
}

func TestNewTransportE_PreservesExplicitFailTimeoutZero(t *testing.T) {
	transport, err := NewTransportE(
		WithUpstreams(
			Upstream("http://service.local",
				Server("http://127.0.0.1:8080", FailTimeout(0)),
			),
		),
	)
	if err != nil {
		t.Fatalf("NewTransportE: %v", err)
	}

	ups := transport.upstreams[upstreamKey{scheme: "http", host: "service.local"}]
	roundRobin, ok := ups.balancer.(*RoundRobin)
	if !ok {
		t.Fatalf("balancer type = %T, want *RoundRobin", ups.balancer)
	}
	peer := roundRobin.primary.Peers[0]
	if peer.FailTimeout != 0 {
		t.Fatalf("FailTimeout = %v, want 0", peer.FailTimeout)
	}
}

func TestNewTransportE_RejectsInvalidServerSettings(t *testing.T) {
	tests := []struct {
		name    string
		server  ServerConfig
		wantErr string
	}{
		{
			name:    "zero weight",
			server:  Server("http://127.0.0.1:8080", Weight(0)),
			wantErr: "weight must be greater than zero",
		},
		{
			name:    "negative weight",
			server:  Server("http://127.0.0.1:8080", Weight(-1)),
			wantErr: "weight must be greater than zero",
		},
		{
			name:    "negative max conns",
			server:  Server("http://127.0.0.1:8080", MaxConns(-1)),
			wantErr: "max_conns must be non-negative",
		},
		{
			name:    "negative max fails",
			server:  Server("http://127.0.0.1:8080", MaxFails(-1)),
			wantErr: "max_fails must be non-negative",
		},
		{
			name:    "negative fail timeout",
			server:  Server("http://127.0.0.1:8080", FailTimeout(-time.Second)),
			wantErr: "fail_timeout must be non-negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewTransportE(
				WithUpstreams(
					Upstream("http://service.local", test.server),
				),
			)
			if err == nil {
				t.Fatal("expected construction error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestNewTransportE_RejectsInvalidTransportSettings(t *testing.T) {
	tests := []struct {
		name    string
		option  TransportOption
		wantErr string
	}{
		{
			name:    "negative attempt timeout",
			option:  WithTimeout(-time.Second),
			wantErr: "attempt timeout must be non-negative",
		},
		{
			name:    "negative connect timeout",
			option:  WithConnectTimeout(-time.Second),
			wantErr: "connect timeout must be non-negative",
		},
		{
			name:    "negative body buffer",
			option:  WithMaxBodyBuffer(-1),
			wantErr: "max body buffer must be non-negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewTransportE(test.option)
			if err == nil {
				t.Fatal("expected construction error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestNewTransportE_AcceptsZeroValuedLimits(t *testing.T) {
	_, err := NewTransportE(
		WithTimeout(0),
		WithConnectTimeout(0),
		WithMaxBodyBuffer(0),
		WithUpstreams(
			Upstream("http://service.local",
				Server("http://127.0.0.1:8080",
					Weight(1),
					MaxConns(0),
					MaxFails(0),
					FailTimeout(0),
				),
			),
		),
	)
	if err != nil {
		t.Fatalf("zero-valued limits should be valid: %v", err)
	}
}

func TestNewTransportE_RejectsUnknownAlgorithm(t *testing.T) {
	upstream := Upstream("http://service.local",
		Server("http://127.0.0.1:8080"),
	)
	upstream.Algorithm = AlgorithmType(999)

	_, err := NewTransportE(WithUpstreams(upstream))
	if err == nil {
		t.Fatal("expected construction error")
	}
	if !strings.Contains(err.Error(), "unsupported load-balancing algorithm: 999") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewTransportE_RejectsNilTransportOption(t *testing.T) {
	var option TransportOption
	_, err := NewTransportE(option)
	if err == nil {
		t.Fatal("expected construction error")
	}
	if !strings.Contains(err.Error(), "nil transport option") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewTransportE_RejectsNilServerOption(t *testing.T) {
	var option ServerOption
	server := Server("http://127.0.0.1:8080", option)

	_, err := NewTransportE(
		WithUpstreams(Upstream("http://service.local", server)),
	)
	if err == nil {
		t.Fatal("expected construction error")
	}
	if !strings.Contains(err.Error(), "nil server option") {
		t.Fatalf("unexpected error: %v", err)
	}
}
