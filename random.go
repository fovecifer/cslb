package cslb

import "math/rand"

// ---------- Random Balancer ----------
// Corresponds to nginx's ngx_http_upstream_random_module.c
//
// Two modes:
//   - Plain random: select one peer randomly (weighted)
//   - Random two:   select 2 random peers, pick the one with fewer connections
//                   (Power of Two Choices algorithm)

const maxRandomTries = 20

// Random implements random load balancing.
type Random struct {
	rr     *RoundRobin
	two    bool // "random two" mode (Power of Two Choices)
	ranges []weightRange
}

// weightRange maps cumulative weight to peer index.
// Corresponds to nginx's ngx_http_upstream_random_range_t.
type weightRange struct {
	peer  *Peer
	upper int // cumulative weight upper bound (exclusive)
}

// NewRandom creates a random balancer.
// Corresponds to nginx's ngx_http_upstream_init_random().
func NewRandom(peers []*Peer) *Random {
	return newRandom(peers, false)
}

// NewRandomTwo creates a "random two" balancer (Power of Two Choices).
// Corresponds to nginx's "random two least_conn" directive.
func NewRandomTwo(peers []*Peer) *Random {
	return newRandom(peers, true)
}

func newRandom(peers []*Peer, two bool) *Random {
	rr := NewRoundRobin(peers)
	r := &Random{
		rr:  rr,
		two: two,
	}

	// Build weight ranges for weighted random selection.
	// Corresponds to nginx's ngx_http_upstream_update_random().
	cumulative := 0
	for _, p := range rr.primary.Peers {
		cumulative += p.Weight
		r.ranges = append(r.ranges, weightRange{peer: p, upper: cumulative})
	}

	return r
}

func (r *Random) NewPicker() Picker {
	return &randomPicker{
		RRPicker: NewRRPicker(r.rr.primary, r.rr.backup),
		balancer: r,
	}
}

// ---------- randomPicker ----------
// Corresponds to nginx's ngx_http_upstream_random_peer_data_t:
//
//	typedef struct {
//	    ngx_http_upstream_rr_peer_data_t    rrp;   // ← embedded RR
//	    ngx_http_upstream_random_conf_t    *conf;
//	} ngx_http_upstream_random_peer_data_t;

type randomPicker struct {
	*RRPicker              // embedded RR
	balancer *Random       // reference to balancer config
	tries    int
}

// Pick selects a peer randomly.
// Plain random: ngx_http_upstream_get_random_peer() (lines 216-290)
// Random two:   ngx_http_upstream_get_random2_peer() (lines 318-428)
func (p *randomPicker) Pick() *Peer {
	if p.balancer.two {
		return p.pickTwo()
	}
	return p.pickOne()
}

// pickOne implements plain weighted random selection.
func (p *randomPicker) pickOne() *Peer {
	group := p.PrimaryGroup()

	if len(group.Peers) == 0 {
		return nil
	}

	group.mu.Lock()
	defer group.mu.Unlock()

	for p.tries < maxRandomTries {
		p.tries++

		// nginx: i = ngx_random() % peers->total_weight
		// then binary search in ranges
		peer := p.randomSelect(group)
		if peer == nil {
			break
		}

		if !peer.Down && !p.Tried(peer) && peerAvailable(peer) {
			peer.conns++
			p.SetTried(peer)
			return peer
		}
	}

	// Fallback to round-robin
	group.mu.Unlock()
	result := p.RRPicker.Pick()
	group.mu.Lock()
	return result
}

// pickTwo implements Power of Two Choices.
// Selects 2 random peers and picks the one with fewer weighted connections.
func (p *randomPicker) pickTwo() *Peer {
	group := p.PrimaryGroup()

	if len(group.Peers) == 0 {
		return nil
	}

	group.mu.Lock()
	defer group.mu.Unlock()

	for p.tries < maxRandomTries {
		p.tries++

		// Pick two random peers
		peer1 := p.randomSelect(group)
		peer2 := p.randomSelect(group)

		if peer1 == nil || peer2 == nil {
			break
		}

		// Skip unavailable peers
		if peer1.Down || !peerAvailable(peer1) {
			peer1 = nil
		}
		if peer2.Down || !peerAvailable(peer2) {
			peer2 = nil
		}

		// nginx (line 397): compare weighted connections
		// peer->conns * prev->weight > prev->conns * peer->weight
		var selected *Peer
		switch {
		case peer1 == nil && peer2 == nil:
			continue
		case peer1 == nil:
			selected = peer2
		case peer2 == nil:
			selected = peer1
		case peer2.conns*peer1.Weight < peer1.conns*peer2.Weight:
			selected = peer2
		default:
			selected = peer1
		}

		if selected != nil && !p.Tried(selected) {
			selected.conns++
			p.SetTried(selected)
			return selected
		}
	}

	// Fallback to round-robin
	group.mu.Unlock()
	result := p.RRPicker.Pick()
	group.mu.Lock()
	return result
}

// randomSelect picks a random peer weighted by configured weights.
// Uses binary search over cumulative weight ranges.
func (p *randomPicker) randomSelect(group *PeerGroup) *Peer {
	if group.TotalWeight <= 0 {
		return nil
	}

	x := rand.Intn(group.TotalWeight)

	// Binary search (nginx uses this in ngx_http_upstream_get_random_peer)
	lo, hi := 0, len(p.balancer.ranges)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if p.balancer.ranges[mid].upper <= x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return p.balancer.ranges[lo].peer
}

// Done delegates to the embedded RR picker.
func (p *randomPicker) Done(peer *Peer, failed bool) {
	p.RRPicker.Done(peer, failed)
}
