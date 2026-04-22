// Package cslb (Client Side Load Balance) implements an http.RoundTripper
// that performs client-side load balancing with nginx-style algorithms.
//
// It works by intercepting HTTP requests, selecting a backend server using
// the configured load balancing algorithm, and rewriting the request URL's
// Scheme and Host before forwarding. Failed requests are automatically
// retried on the next available backend.
//
// Architecture mirrors nginx's three-layer callback design:
//
//	Layer 1: Balancer      (init_upstream)  — build peer list, one per upstream
//	Layer 2: Picker        (init_peer)      — per-request selection context
//	Layer 3: Pick / Done   (get / free)     — select peer, report result
//
// Supported algorithms: Round-Robin, Least-Conn, IP-Hash, Hash, Random,
// Random-Two (Power of Two Choices). All algorithms wrap RoundRobin as a
// base layer, matching nginx's decorator pattern.
package cslb

import (
	"errors"
	"sync"
	"time"
)

// ---------- Peer: a single backend server ----------

// Peer represents a backend server.
type Peer struct {
	Addr   string
	Weight int
	Down   bool

	MaxConns    int
	MaxFails    int
	FailTimeout time.Duration

	Backup bool // if true, only used when all primary peers fail

	// Mutable state — protected by PeerGroup.mu
	currentWeight   int
	effectiveWeight int
	conns           int
	fails           int
	accessed        time.Time
	checked         time.Time
}

func (p *Peer) init() {
	if p.Weight <= 0 {
		p.Weight = 1
	}
	if p.MaxFails == 0 {
		p.MaxFails = 1
	}
	if p.FailTimeout == 0 {
		p.FailTimeout = 10 * time.Second
	}
	p.effectiveWeight = p.Weight
	p.currentWeight = 0
}

// ---------- PeerGroup: primary or backup ----------

// PeerGroup holds a list of peers with shared lock.
type PeerGroup struct {
	mu          sync.Mutex
	Peers       []*Peer
	TotalWeight int
	Tries       int // number of non-down peers
}

// BuildGroups splits peers into primary and backup groups.
func BuildGroups(peers []*Peer) (primary *PeerGroup, backup *PeerGroup) {
	primary = &PeerGroup{}
	backup = &PeerGroup{}

	for _, p := range peers {
		if p == nil {
			continue
		}
		p.init()
		if p.Backup {
			backup.Peers = append(backup.Peers, p)
			backup.TotalWeight += p.Weight
			if !p.Down {
				backup.Tries++
			}
		} else {
			primary.Peers = append(primary.Peers, p)
			primary.TotalWeight += p.Weight
			if !p.Down {
				primary.Tries++
			}
		}
	}

	if len(backup.Peers) == 0 {
		backup = nil
	}
	return
}

// ---------- Core interfaces (nginx's pluggable callbacks) ----------

// Balancer creates per-request Pickers.
// Corresponds to nginx's init_upstream + init_peer.
//
//	nginx:  us->peer.init_upstream = ngx_http_upstream_init_ip_hash;
//	Go:     balancer := cslb.NewIPHash(peers)
type Balancer interface {
	// Pick creates a per-request Picker.
	// Corresponds to nginx's us->peer.init(r, us).
	NewPicker() Picker
}

// Picker is the per-request peer selection context.
// Corresponds to nginx's peer.get / peer.free on ngx_peer_connection_t.
type Picker interface {
	// Pick selects the next available peer.
	// Returns nil if no peer is available.
	// Corresponds to nginx's r->upstream->peer.get(pc, data).
	Pick() *Peer

	// Done reports the result of a request to the selected peer.
	// Corresponds to nginx's r->upstream->peer.free(pc, data, state).
	Done(peer *Peer, failed bool)
}

// ---------- Result helper for simple use cases ----------

// PickResult bundles a peer with its picker for convenience.
type PickResult struct {
	Peer   *Peer
	picker Picker
}

// Done reports the request result. Must be called exactly once.
func (r *PickResult) Done(failed bool) {
	if r == nil || r.picker == nil {
		return
	}
	r.picker.Done(r.Peer, failed)
}

// PickOne is a convenience that creates a picker, picks one peer,
// and returns a result that must be Done()'d.
func PickOne(b Balancer) *PickResult {
	if b == nil {
		return nil
	}
	pk := b.NewPicker()
	if pk == nil {
		return nil
	}
	p := pk.Pick()
	if p == nil {
		return nil
	}
	return &PickResult{Peer: p, picker: pk}
}

// PickWithRetry creates a picker and retries across peers on failure.
func PickWithRetry(b Balancer, tryFunc func(addr string) error) (*Peer, error) {
	if b == nil {
		return nil, ErrNilBalancer
	}
	if tryFunc == nil {
		return nil, ErrNilTryFunc
	}

	pk := b.NewPicker()
	if pk == nil {
		return nil, ErrNoPeerAvailable
	}

	for {
		p := pk.Pick()
		if p == nil {
			return nil, ErrNoPeerAvailable
		}

		err := tryFunc(p.Addr)
		if err == nil {
			pk.Done(p, false)
			return p, nil
		}

		pk.Done(p, true)
		// Loop continues — picker tracks tried peers internally
	}
}

// ErrNoPeerAvailable is returned when no peer can be selected.
var (
	ErrNoPeerAvailable = &noPeerError{}
	ErrNilBalancer     = errors.New("cslb: nil balancer")
	ErrNilTryFunc      = errors.New("cslb: nil tryFunc")
)

type noPeerError struct{}

func (e *noPeerError) Error() string { return "no peer available" }
