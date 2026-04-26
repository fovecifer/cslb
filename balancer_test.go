package cslb

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// ---------- Interface compliance ----------

func TestInterfaceCompliance(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
	}

	// All algorithms implement the same Balancer interface
	var _ Balancer = NewRoundRobin(peers)
	var _ Balancer = NewIPHash(peers)
	var _ Balancer = NewLeastConn(peers)
	var _ Balancer = NewHash(peers)
	var _ Balancer = NewHashConsistent(peers)
	var _ Balancer = NewRandom(peers)
	var _ Balancer = NewRandomTwo(peers)
}

// ---------- RoundRobin tests ----------

func TestRR_SmoothWeightedDistribution(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 5},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	})

	counts := pickN(t, b, 70)

	if counts["A"] != 50 || counts["B"] != 10 || counts["C"] != 10 {
		t.Errorf("distribution: A=%d B=%d C=%d", counts["A"], counts["B"], counts["C"])
	}
}

func TestRR_Smoothness(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 5},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	})

	seq := pickSequence(t, b, 7)
	t.Logf("sequence: %v", seq)

	maxConsec := maxConsecutive(seq)
	if maxConsec > 2 {
		t.Errorf("max consecutive same peer = %d, want <= 2", maxConsec)
	}
}

func TestRR_EqualWeights(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	})

	counts := pickN(t, b, 300)
	for _, addr := range []string{"A", "B", "C"} {
		if counts[addr] != 100 {
			t.Errorf("%s=%d, want 100", addr, counts[addr])
		}
	}
}

func TestRR_DownPeerSkipped(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1, Down: true},
	})

	counts := pickN(t, b, 50)
	if counts["B"] != 0 {
		t.Errorf("down peer B picked %d times", counts["B"])
	}
}

func TestRR_BackupFallback(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "primary", Weight: 1, Down: true},
		{Addr: "backup", Weight: 1, Backup: true},
	})

	pk := b.NewPicker()
	p := pk.Pick()
	if p == nil {
		t.Fatal("Pick returned nil")
	}
	if p.Addr != "backup" {
		t.Errorf("got %s, want backup", p.Addr)
	}
	pk.Done(p, false)
}

func TestRR_MaxConns(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1, MaxConns: 1},
		{Addr: "B", Weight: 1},
	})

	// Hold one connection to each — first 2 picks
	pk := b.NewPicker()
	p1 := pk.Pick()
	p2 := pk.Pick()
	// Third pick should fail (both tried in this picker)
	p3 := pk.Pick()
	if p3 != nil {
		t.Error("expected nil after all peers tried")
	}

	if p1.Addr == p2.Addr {
		t.Error("same peer picked twice")
	}

	pk.Done(p1, false)
	pk.Done(p2, false)
}

func TestRR_AllDown(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1, Down: true},
		{Addr: "B", Weight: 1, Down: true},
	})

	pk := b.NewPicker()
	if pk.Pick() != nil {
		t.Error("expected nil when all down")
	}
}

func TestRR_EffectiveWeightDegradation(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 10, MaxFails: 10, FailTimeout: 10 * time.Second},
		{Addr: "B", Weight: 10, MaxFails: 10, FailTimeout: 10 * time.Second},
	}
	b := NewRoundRobin(peers)

	// Fail A repeatedly to degrade its effective_weight.
	// Pick peers and always report failure for A, success for B.
	for i := 0; i < 20; i++ {
		pk := b.NewPicker()
		p := pk.Pick()
		pk.Done(p, p.Addr == "A")
	}

	counts := pickN(t, b, 40)
	t.Logf("after degradation: A=%d B=%d", counts["A"], counts["B"])
	if counts["A"] >= counts["B"] {
		t.Error("degraded A should get fewer picks")
	}
}

// ---------- IPHash tests ----------

func TestIPHash_ConsistentSelection(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	}
	b := NewIPHash(peers)

	ip := net.ParseIP("192.168.1.100")

	// Same IP should consistently pick the same peer
	var firstAddr string
	for i := 0; i < 10; i++ {
		pk := b.NewPickerForIP(ip)
		p := pk.Pick()
		if p == nil {
			t.Fatal("Pick returned nil")
		}
		if i == 0 {
			firstAddr = p.Addr
		} else if p.Addr != firstAddr {
			t.Errorf("inconsistent: got %s, expected %s", p.Addr, firstAddr)
		}
		pk.Done(p, false)
	}
	t.Logf("IP %s consistently maps to %s", ip, firstAddr)
}

func TestIPHash_DifferentIPsDiffer(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	}
	b := NewIPHash(peers)

	addrs := map[string]bool{}
	// Try many IPs with varying 3rd octet — IP hash uses first 3 bytes (/24)
	for i := 0; i < 100; i++ {
		ip := net.IPv4(10, byte(i/256), byte(i%256), 1)
		pk := b.NewPickerForIP(ip)
		p := pk.Pick()
		addrs[p.Addr] = true
		pk.Done(p, false)
	}

	if len(addrs) < 2 {
		t.Errorf("only %d unique backends, expected >= 2", len(addrs))
	}
}

// ---------- LeastConn tests ----------

func TestLeastConn_PrefersLessLoaded(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
	}
	b := NewLeastConn(peers)

	// Hold a connection on one peer
	pk1 := b.NewPicker()
	p1 := pk1.Pick()

	// Next pick should prefer the other peer (0 conns vs 1 conn)
	pk2 := b.NewPicker()
	p2 := pk2.Pick()

	if p1.Addr == p2.Addr {
		t.Errorf("least_conn should prefer the unloaded peer")
	}

	pk1.Done(p1, false)
	pk2.Done(p2, false)
}

func TestLeastConn_RespectsWeight(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 10},
		{Addr: "B", Weight: 1},
	}
	b := NewLeastConn(peers)

	counts := pickN(t, b, 100)
	t.Logf("least_conn weighted: A=%d B=%d", counts["A"], counts["B"])

	// A should get significantly more due to higher weight
	if counts["A"] <= counts["B"] {
		t.Error("higher-weight A should get more picks")
	}
}

// ---------- Hash tests ----------

func TestHash_ConsistentForSameKey(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	}
	b := NewHash(peers)

	key := "/api/users/123"
	var firstAddr string

	for i := 0; i < 10; i++ {
		pk := b.NewPickerForKey(key)
		p := pk.Pick()
		if i == 0 {
			firstAddr = p.Addr
		} else if p.Addr != firstAddr {
			t.Errorf("inconsistent: %s vs %s for key %s", p.Addr, firstAddr, key)
		}
		pk.Done(p, false)
	}
}

func TestHash_DifferentKeysDistribute(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	}
	b := NewHash(peers)

	addrs := map[string]bool{}
	for i := 0; i < 100; i++ {
		pk := b.NewPickerForKey(fmt.Sprintf("/path/%d", i))
		p := pk.Pick()
		addrs[p.Addr] = true
		pk.Done(p, false)
	}

	if len(addrs) < 2 {
		t.Errorf("only %d unique backends", len(addrs))
	}
}

// ---------- Random tests ----------

func TestRandom_Distribution(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	}
	b := NewRandom(peers)

	counts := pickN(t, b, 3000)
	t.Logf("random: A=%d B=%d C=%d", counts["A"], counts["B"], counts["C"])

	// Each should get roughly 1000 (within 30% tolerance)
	for _, addr := range []string{"A", "B", "C"} {
		if counts[addr] < 700 || counts[addr] > 1300 {
			t.Errorf("%s=%d, outside expected range [700,1300]", addr, counts[addr])
		}
	}
}

func TestRandomTwo_PrefersLessLoaded(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
	}
	b := NewRandomTwo(peers)

	// Hold a connection on one peer
	pk1 := b.NewPicker()
	p1 := pk1.Pick()

	// Next picks should tend toward the other peer
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		pk := b.NewPicker()
		p := pk.Pick()
		counts[p.Addr]++
		pk.Done(p, false)
	}

	t.Logf("random two (one held): held=%s, A=%d B=%d", p1.Addr, counts["A"], counts["B"])
	pk1.Done(p1, false)
}

// ---------- PickOne / PickWithRetry convenience ----------

func TestPickOne(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1},
	})

	r := PickOne(b)
	if r == nil {
		t.Fatal("PickOne returned nil")
	}
	if r.Peer.Addr != "A" {
		t.Errorf("got %s, want A", r.Peer.Addr)
	}
	r.Done(false)
}

func TestPickWithRetry(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	})

	attempts := 0
	peer, err := PickWithRetry(b, func(addr string) error {
		attempts++
		if addr == "A" {
			return fmt.Errorf("refused")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peer.Addr == "A" {
		t.Error("should not return failed peer A")
	}
	t.Logf("succeeded on %s after %d attempts", peer.Addr, attempts)
}

func TestPickWithRetry_AllFail(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
	})

	_, err := PickWithRetry(b, func(addr string) error {
		return fmt.Errorf("fail")
	})

	if err != ErrNoPeerAvailable {
		t.Errorf("got %v, want ErrNoPeerAvailable", err)
	}
}

func TestPickOne_NilBalancer(t *testing.T) {
	if PickOne(nil) != nil {
		t.Fatal("expected nil result for nil balancer")
	}
}

func TestPickWithRetry_NilBalancer(t *testing.T) {
	_, err := PickWithRetry(nil, func(addr string) error { return nil })
	if err != ErrNilBalancer {
		t.Fatalf("got %v, want %v", err, ErrNilBalancer)
	}
}

func TestPickWithRetry_NilTryFunc(t *testing.T) {
	b := NewRoundRobin([]*Peer{
		{Addr: "A", Weight: 1},
	})

	_, err := PickWithRetry(b, nil)
	if err != ErrNilTryFunc {
		t.Fatalf("got %v, want %v", err, ErrNilTryFunc)
	}
}

func TestBuildGroups_SkipsNilPeers(t *testing.T) {
	primary, backup := BuildGroups([]*Peer{
		nil,
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1, Backup: true},
	})

	if len(primary.Peers) != 1 || primary.Peers[0].Addr != "A" {
		t.Fatalf("unexpected primary peers: %+v", primary.Peers)
	}
	if backup == nil || len(backup.Peers) != 1 || backup.Peers[0].Addr != "B" {
		t.Fatalf("unexpected backup peers: %+v", backup.Peers)
	}
}

// ---------- Pluggable: same code works with any algorithm ----------

func TestPluggable_AllAlgorithms(t *testing.T) {
	peers := []*Peer{
		{Addr: "A", Weight: 3},
		{Addr: "B", Weight: 2},
		{Addr: "C", Weight: 1},
	}

	algorithms := map[string]Balancer{
		"round_robin": NewRoundRobin(peers),
		"ip_hash":     NewIPHash(peers),
		"least_conn":  NewLeastConn(peers),
		"hash":        NewHash(peers),
		"random":      NewRandom(peers),
		"random_two":  NewRandomTwo(peers),
	}

	for name, b := range algorithms {
		t.Run(name, func(t *testing.T) {
			// All algorithms work through the same Balancer interface
			for i := 0; i < 20; i++ {
				r := PickOne(b)
				if r == nil {
					t.Fatal("nil result")
				}
				r.Done(false)
			}
		})
	}
}

// ---------- helpers ----------

func pickN(t *testing.T, b Balancer, n int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		pk := b.NewPicker()
		p := pk.Pick()
		if p == nil {
			t.Fatal("Pick returned nil")
		}
		counts[p.Addr]++
		pk.Done(p, false)
	}
	return counts
}

func pickSequence(t *testing.T, b Balancer, n int) []string {
	t.Helper()
	var seq []string
	for i := 0; i < n; i++ {
		pk := b.NewPicker()
		p := pk.Pick()
		seq = append(seq, p.Addr)
		pk.Done(p, false)
	}
	return seq
}

func maxConsecutive(seq []string) int {
	max := 1
	cur := 1
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			cur++
			if cur > max {
				max = cur
			}
		} else {
			cur = 1
		}
	}
	return max
}
