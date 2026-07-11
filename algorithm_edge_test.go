package cslb

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

type algorithmPickerFactory func([]*Peer) func(int) Picker

func algorithmPickerFactories() map[string]algorithmPickerFactory {
	return map[string]algorithmPickerFactory{
		"round_robin": func(peers []*Peer) func(int) Picker {
			balancer := NewRoundRobin(peers)
			return func(_ int) Picker { return balancer.NewPicker() }
		},
		"least_conn": func(peers []*Peer) func(int) Picker {
			balancer := NewLeastConn(peers)
			return func(_ int) Picker { return balancer.NewPicker() }
		},
		"ip_hash": func(peers []*Peer) func(int) Picker {
			balancer := NewIPHash(peers)
			return func(key int) Picker {
				return balancer.NewPickerForIP(net.IPv4(10, 0, byte(key), 1))
			}
		},
		"hash": func(peers []*Peer) func(int) Picker {
			balancer := NewHash(peers)
			return func(key int) Picker {
				return balancer.NewPickerForKey(fmt.Sprintf("key-%d", key))
			}
		},
		"hash_consistent": func(peers []*Peer) func(int) Picker {
			balancer := NewHashConsistent(peers)
			return func(key int) Picker {
				return balancer.NewPickerForKey(fmt.Sprintf("key-%d", key))
			}
		},
		"random": func(peers []*Peer) func(int) Picker {
			balancer := NewRandom(peers)
			return func(_ int) Picker { return balancer.NewPicker() }
		},
		"random_two": func(peers []*Peer) func(int) Picker {
			balancer := NewRandomTwo(peers)
			return func(_ int) Picker { return balancer.NewPicker() }
		},
	}
}

func TestAlgorithms_FallBackToBackup(t *testing.T) {
	for name, newPickerFactory := range algorithmPickerFactories() {
		t.Run(name, func(t *testing.T) {
			peers := []*Peer{
				{Addr: "primary", Weight: 1, Down: true},
				{Addr: "backup", Weight: 1, Backup: true},
			}
			newPicker := newPickerFactory(peers)
			picker := newPicker(1)

			peer := picker.Pick()
			if peer == nil {
				t.Fatal("Pick returned nil")
			}
			if peer.Addr != "backup" {
				t.Fatalf("Pick = %q, want backup", peer.Addr)
			}
			picker.Done(peer, false)

			if peer := picker.Pick(); peer != nil {
				picker.Done(peer, false)
				t.Fatalf("second Pick = %q, want nil after backup was tried", peer.Addr)
			}
		})
	}
}

func TestAlgorithms_SinglePeerIgnoresFailureAccounting(t *testing.T) {
	for name, newPickerFactory := range algorithmPickerFactories() {
		t.Run(name, func(t *testing.T) {
			peer := &Peer{
				Addr:        "only",
				Weight:      10,
				MaxFails:    1,
				FailTimeout: time.Minute,
			}
			newPicker := newPickerFactory([]*Peer{peer})

			picker := newPicker(1)
			got := picker.Pick()
			if got != peer {
				t.Fatalf("Pick = %v, want only peer", got)
			}
			picker.Done(got, true)

			if peer.fails != 0 {
				t.Fatalf("fails = %d, want 0", peer.fails)
			}
			if peer.effectiveWeight != peer.Weight {
				t.Fatalf("effective weight = %d, want %d", peer.effectiveWeight, peer.Weight)
			}
			if peer.conns != 0 {
				t.Fatalf("connections = %d, want 0", peer.conns)
			}

			nextPicker := newPicker(2)
			next := nextPicker.Pick()
			if next != peer {
				t.Fatalf("next Pick = %v, want only peer", next)
			}
			nextPicker.Done(next, false)
		})
	}
}

func TestAlgorithms_OnePrimaryAndBackupIsNotSingle(t *testing.T) {
	for name, newPickerFactory := range algorithmPickerFactories() {
		t.Run(name, func(t *testing.T) {
			primary := &Peer{
				Addr:        "primary",
				Weight:      10,
				MaxFails:    1,
				FailTimeout: time.Minute,
			}
			backup := &Peer{Addr: "backup", Weight: 1, Backup: true}
			newPicker := newPickerFactory([]*Peer{primary, backup})

			picker := newPicker(1)
			got := picker.Pick()
			if got != primary {
				t.Fatalf("first Pick = %v, want primary", got)
			}
			picker.Done(got, true)

			if primary.fails != 1 {
				t.Fatalf("primary fails = %d, want 1", primary.fails)
			}
			if primary.effectiveWeight >= primary.Weight {
				t.Fatalf("primary effective weight = %d, want degraded below %d", primary.effectiveWeight, primary.Weight)
			}

			got = picker.Pick()
			if got != backup {
				t.Fatalf("second Pick = %v, want backup", got)
			}
			picker.Done(got, false)
		})
	}
}

func TestIPHash_NoIPFallsBackToRoundRobin(t *testing.T) {
	balancer := NewIPHash([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
	})

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		picker := balancer.NewPicker()
		peer := picker.Pick()
		if peer == nil {
			t.Fatal("Pick returned nil")
		}
		counts[peer.Addr]++
		picker.Done(peer, false)
	}

	if counts["A"] != 2 || counts["B"] != 2 {
		t.Fatalf("round-robin fallback distribution = %v, want A=2 B=2", counts)
	}
}

func TestIPHash_IPv4Uses24BitSubnet(t *testing.T) {
	balancer := NewIPHash([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
		{Addr: "C", Weight: 1},
	})

	firstPicker := balancer.NewPickerForIP(net.ParseIP("192.0.2.1"))
	first := firstPicker.Pick()
	if first == nil {
		t.Fatal("first Pick returned nil")
	}
	firstPicker.Done(first, false)

	secondPicker := balancer.NewPickerForIP(net.ParseIP("192.0.2.254"))
	second := secondPicker.Pick()
	if second == nil {
		t.Fatal("second Pick returned nil")
	}
	secondPicker.Done(second, false)

	if first != second {
		t.Fatalf("same /24 subnet mapped to %q and %q", first.Addr, second.Addr)
	}
}

func TestAlgorithms_ConcurrentPickAndDone(t *testing.T) {
	for name, newPickerFactory := range algorithmPickerFactories() {
		t.Run(name, func(t *testing.T) {
			peers := []*Peer{
				{Addr: "A", Weight: 5},
				{Addr: "B", Weight: 3},
				{Addr: "C", Weight: 2},
			}
			newPicker := newPickerFactory(peers)

			const goroutines = 16
			const picksPerGoroutine = 100
			errCh := make(chan error, goroutines)
			var wg sync.WaitGroup
			for g := 0; g < goroutines; g++ {
				g := g
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < picksPerGoroutine; i++ {
						picker := newPicker(g*picksPerGoroutine + i)
						peer := picker.Pick()
						if peer == nil {
							errCh <- fmt.Errorf("pick %d returned nil", i)
							return
						}
						picker.Done(peer, false)
					}
				}()
			}
			wg.Wait()
			close(errCh)

			for err := range errCh {
				t.Error(err)
			}
		})
	}
}

func TestAlgorithms_ResetFailuresAfterSuccessfulProbe(t *testing.T) {
	for name, newPickerFactory := range algorithmPickerFactories() {
		t.Run(name, func(t *testing.T) {
			peers := []*Peer{
				{Addr: "A", Weight: 1, FailTimeout: time.Second},
				{Addr: "B", Weight: 1, FailTimeout: time.Second},
			}
			newPicker := newPickerFactory(peers)

			firstPicker := newPicker(7)
			target := firstPicker.Pick()
			if target == nil {
				t.Fatal("first Pick returned nil")
			}
			firstPicker.Done(target, true)
			if target.fails != 1 {
				t.Fatalf("fails after failed attempt = %d, want 1", target.fails)
			}

			for _, peer := range peers {
				if peer != target {
					peer.Down = true
				}
			}
			target.checked = time.Now().Add(-2 * target.FailTimeout)

			probePicker := newPicker(7)
			probe := probePicker.Pick()
			if probe != target {
				if probe != nil {
					probePicker.Done(probe, false)
				}
				t.Fatalf("probe Pick = %v, want recovered peer %q", probe, target.Addr)
			}
			probePicker.Done(probe, false)
			if target.fails != 0 {
				t.Fatalf("fails after successful probe = %d, want 0", target.fails)
			}
		})
	}
}
