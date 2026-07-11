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
	return p.pickWithBackup(p.pickFromGroup)
}

func (p *leastConnPicker) pickFromGroup(group *PeerGroup) *Peer {
	group.mu.Lock()
	defer group.mu.Unlock()

	now := time.Now()
	var best *Peer
	many := false

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
			many = false
		} else if peer.conns*best.Weight == best.conns*peer.Weight {
			many = true
		}
	}

	if best == nil {
		return nil
	}

	if many {
		// Match nginx's second pass: only peers tied at the minimum normalized
		// connection count participate in smooth weighted round-robin.
		least := best
		best = nil
		total := 0
		for _, peer := range group.Peers {
			if p.Tried(peer) || peer.Down {
				continue
			}
			if peer.conns*least.Weight != least.conns*peer.Weight {
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

			peer.currentWeight += peer.effectiveWeight
			total += peer.effectiveWeight
			if peer.effectiveWeight < peer.Weight {
				peer.effectiveWeight++
			}
			if best == nil || peer.currentWeight > best.currentWeight {
				best = peer
			}
		}
		best.currentWeight -= total
	}

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
