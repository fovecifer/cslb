package cslb

import "time"

// ---------- LeastConn Balancer ----------
// Corresponds to nginx's ngx_http_upstream_least_conn_module.c
//
// Selects the peer with the fewest active connections,
// weighted by configured weight. Ties are broken by
// round-robin's current_weight.

// LeastConn implements least-connections load balancing.
type LeastConn struct {
	rr *RoundRobin
}

// NewLeastConn creates a least-connections balancer.
// Corresponds to nginx's ngx_http_upstream_init_least_conn().
func NewLeastConn(peers []*Peer) *LeastConn {
	return &LeastConn{rr: NewRoundRobin(peers)}
}

func (lc *LeastConn) NewPicker() Picker {
	return &leastConnPicker{
		RRPicker: NewRRPicker(lc.rr.primary, lc.rr.backup),
	}
}

// ---------- leastConnPicker ----------
// Corresponds to nginx's per-request data.
// No extra fields needed — only overrides get, inherits free from RR.
//
// nginx's least_conn is unique in that its init_peer does:
//
//	ngx_http_upstream_init_round_robin_peer(r, us);
//	r->upstream->peer.get = ngx_http_upstream_get_least_conn_peer;
//	// free is NOT overridden — inherits RR's free

type leastConnPicker struct {
	*RRPicker // embedded RR picker (nginx: rrp as first field)
}

// Pick selects the peer with fewest weighted connections.
// Corresponds to nginx's ngx_http_upstream_get_least_conn_peer() (lines 100-246).
func (p *leastConnPicker) Pick() *Peer {
	peer := p.pickFromGroup(p.PrimaryGroup())
	if peer != nil {
		return peer
	}

	// Fallback to backup
	if bg := p.BackupGroup(); bg != nil && len(bg.Peers) > 0 {
		for _, bp := range bg.Peers {
			delete(p.tried, bp)
		}
		return p.pickFromGroup(bg)
	}

	return nil
}

func (p *leastConnPicker) pickFromGroup(group *PeerGroup) *Peer {
	group.mu.Lock()
	defer group.mu.Unlock()

	now := time.Now()
	var best *Peer
	total := 0

	for _, peer := range group.Peers {
		if p.Tried(peer) {
			continue
		}
		if peer.Down {
			continue
		}
		if peer.MaxFails > 0 &&
			peer.fails >= peer.MaxFails &&
			now.Sub(peer.checked) <= peer.FailTimeout {
			continue
		}
		if peer.MaxConns > 0 && peer.conns >= peer.MaxConns {
			continue
		}

		// ---- Least-conn selection (nginx lines 154-186) ----
		//
		// nginx compares: peer->conns * best->weight vs best->conns * peer->weight
		// This is the normalized comparison: conns/weight < best_conns/best_weight
		// without floating point.

		if best == nil ||
			peer.conns*best.Weight < best.conns*peer.Weight {
			best = peer
			total = peer.effectiveWeight
			// Reset RR state for tie-breaking
			best.currentWeight = best.effectiveWeight
		} else if peer.conns*best.Weight == best.conns*peer.Weight {
			// Tie: use weighted round-robin to break it
			// (nginx lines 175-186: same logic as RR core)
			peer.currentWeight += peer.effectiveWeight
			total += peer.effectiveWeight

			if peer.effectiveWeight < peer.Weight {
				peer.effectiveWeight++
			}

			if peer.currentWeight > best.currentWeight {
				best = peer
			}
		}
	}

	if best == nil {
		return nil
	}

	// Same as RR: subtract total from winner
	best.currentWeight -= total

	if now.Sub(best.checked) > best.FailTimeout {
		best.checked = now
	}

	best.conns++
	p.SetTried(best)

	return best
}

// Done delegates to the embedded RR picker.
// nginx's least_conn does NOT override free — inherits from round-robin.
func (p *leastConnPicker) Done(peer *Peer, failed bool) {
	p.RRPicker.Done(peer, failed)
}
