package cslb

// Boundary tests that pin exact limits and constants. Each one fails if the
// comparison or arithmetic it covers is off by one or inverted.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewTransport_DefaultTransportSettings(t *testing.T) {
	ht, ok := NewTransport().transport.(*http.Transport)
	if !ok {
		t.Fatal("default transport is not *http.Transport")
	}
	if ht.IdleConnTimeout != 90*time.Second ||
		ht.TLSHandshakeTimeout != 10*time.Second ||
		ht.ExpectContinueTimeout != time.Second {
		t.Fatalf("timeouts = idle %v, TLS %v, expect-continue %v; want 90s, 10s, 1s",
			ht.IdleConnTimeout, ht.TLSHandshakeTimeout, ht.ExpectContinueTimeout)
	}
}

func TestRRPicker_FailureDegradesEffectiveWeightByWeightOverMaxFails(t *testing.T) {
	heavy := &Peer{Addr: "heavy", Weight: 10, MaxFails: 2}
	rr := NewRoundRobin([]*Peer{heavy, {Addr: "light", Weight: 1}})

	picker := rr.NewPicker()
	if got := picker.Pick(); got != heavy {
		t.Fatalf("Pick = %v, want the heavier peer", got)
	}
	picker.Done(heavy, true)
	if heavy.effectiveWeight != 5 {
		t.Fatalf("effective weight after one failure = %d, want 10 - 10/2 = 5", heavy.effectiveWeight)
	}
}

func TestRRPicker_SinglePeerHonorsMaxConns(t *testing.T) {
	peer := &Peer{Addr: "only", MaxConns: 1}
	rr := NewRoundRobin([]*Peer{peer})

	first := rr.NewPicker()
	if got := first.Pick(); got != peer {
		t.Fatalf("first Pick = %v, want the peer below max_conns", got)
	}
	if got := rr.NewPicker().Pick(); got != nil {
		t.Fatalf("Pick at max_conns = %v, want nil", got)
	}
	first.Done(peer, false)
	if got := rr.NewPicker().Pick(); got != peer {
		t.Fatalf("Pick after release = %v, want the peer", got)
	}
}

func TestLeastConn_SkipsPeerAtMaxConns(t *testing.T) {
	capped := &Peer{Addr: "capped", Weight: 3, MaxConns: 1}
	other := &Peer{Addr: "other", Weight: 1}
	lc := NewLeastConn([]*Peer{capped, other})

	if got := lc.NewPicker().Pick(); got != capped {
		t.Fatalf("first Pick = %v, want the heavier peer below max_conns", got)
	}
	if got := lc.NewPicker().Pick(); got != other {
		t.Fatalf("second Pick = %v, want the other peer", got)
	}
	// capped has the lower weighted load (1/3 < 1/1) but is at max_conns.
	if got := lc.NewPicker().Pick(); got != other {
		t.Fatalf("third Pick = %v, want the peer below max_conns", got)
	}
}

func TestLeastConn_SkipsPeerAtMaxFails(t *testing.T) {
	failed := &Peer{Addr: "failed", Weight: 1, MaxFails: 2, FailTimeout: time.Minute}
	busy := &Peer{Addr: "busy", Weight: 1}
	lc := NewLeastConn([]*Peer{failed, busy})
	failed.fails = 2
	failed.checked = time.Now()
	busy.conns = 5

	if got := lc.NewPicker().Pick(); got != busy {
		t.Fatalf("Pick = %v, want the busy peer while the other has reached max_fails", got)
	}
}

func TestCompareLoad_CrossMultipliesWeights(t *testing.T) {
	tests := []struct {
		a, b *Peer
		want int
	}{
		{a: &Peer{conns: 3, Weight: 1}, b: &Peer{conns: 1, Weight: 2}, want: 1},
		{a: &Peer{conns: 1, Weight: 3}, b: &Peer{conns: 2, Weight: 1}, want: -1},
		{a: &Peer{conns: 2, Weight: 2}, b: &Peer{conns: 1, Weight: 1}, want: 0},
	}
	for _, test := range tests {
		if got := compareLoad(test.a, test.b); got != test.want {
			t.Errorf("compareLoad(%d/%d, %d/%d) = %d, want %d",
				test.a.conns, test.a.Weight, test.b.conns, test.b.Weight, got, test.want)
		}
	}
}

func TestRandom_SelectsByWeightedRange(t *testing.T) {
	t.Run("two peers use random selection", func(t *testing.T) {
		b := NewRandom([]*Peer{{Addr: "a"}, {Addr: "b"}})
		b.intn = func(n int) int { return n - 1 }
		if got := b.NewPicker().Pick(); got == nil || got.Addr != "b" {
			t.Fatalf("Pick = %v, want b from the last weight range", got)
		}
	})

	t.Run("last of several ranges", func(t *testing.T) {
		b := NewRandom([]*Peer{{Addr: "a"}, {Addr: "b"}, {Addr: "c"}, {Addr: "d"}})
		b.intn = func(n int) int { return n - 1 }
		if got := b.NewPicker().Pick(); got == nil || got.Addr != "d" {
			t.Fatalf("Pick = %v, want d from the last weight range", got)
		}
	})
}

func TestReadBody_BodyAtLimitStaysInMemory(t *testing.T) {
	transport := NewTransport(WithMaxBodyBuffer(int64(len("payload"))))
	b, file, err := transport.readBody(strings.NewReader("payload"))
	if file != nil {
		file.Close()
	}
	if err != nil || file != nil || string(b) != "payload" {
		t.Fatalf("readBody = (%q, spilled %v, %v), want the body in memory", b, file != nil, err)
	}
}

func TestSplitKetamaHostPort_AllPortDigits(t *testing.T) {
	host, port := splitKetamaHostPort("10.0.0.1:8090")
	if host != "10.0.0.1" || port != "8090" {
		t.Fatalf("splitKetamaHostPort = (%q, %q), want (10.0.0.1, 8090)", host, port)
	}
}
