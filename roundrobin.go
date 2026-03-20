package cslb

import "time"

// ---------- RoundRobin Balancer ----------
// Corresponds to nginx's ngx_http_upstream_init_round_robin()
// and ngx_http_upstream_round_robin.c

// RoundRobin implements smooth weighted round-robin load balancing.
type RoundRobin struct {
	primary *PeerGroup
	backup  *PeerGroup
}

// NewRoundRobin creates a round-robin balancer.
func NewRoundRobin(peers []*Peer) *RoundRobin {
	primary, backup := BuildGroups(peers)
	return &RoundRobin{primary: primary, backup: backup}
}

func (rr *RoundRobin) NewPicker() Picker {
	return NewRRPicker(rr.primary, rr.backup)
}

// Primary returns the primary peer group (used by wrapping algorithms).
func (rr *RoundRobin) Primary() *PeerGroup { return rr.primary }

// Backup returns the backup peer group.
func (rr *RoundRobin) Backup() *PeerGroup { return rr.backup }

// ---------- RRPicker: per-request round-robin state ----------
// Corresponds to nginx's ngx_http_upstream_rr_peer_data_t

// RRPicker is the per-request state for round-robin selection.
// Other algorithms embed this as their first field, mirroring
// nginx's pattern where rrp is the first field of the per-request struct.
type RRPicker struct {
	primary *PeerGroup
	backup  *PeerGroup
	tried   map[*Peer]bool // nginx uses a bitmap; Go map is cleaner
}

// NewRRPicker creates a round-robin picker.
func NewRRPicker(primary, backup *PeerGroup) *RRPicker {
	return &RRPicker{
		primary: primary,
		backup:  backup,
		tried:   make(map[*Peer]bool),
	}
}

// Pick implements Picker.Pick using smooth weighted round-robin.
// Corresponds to nginx's ngx_http_upstream_get_round_robin_peer().
func (rr *RRPicker) Pick() *Peer {
	// Try primary group
	p := rr.pickFromGroup(rr.primary)
	if p != nil {
		return p
	}

	// Fallback to backup group (nginx lines 667-689)
	if rr.backup != nil && len(rr.backup.Peers) > 0 {
		// Reset tried for backup group
		for _, bp := range rr.backup.Peers {
			delete(rr.tried, bp)
		}
		return rr.pickFromGroup(rr.backup)
	}

	return nil
}

// pickFromGroup is the core smooth weighted round-robin algorithm.
// Corresponds to nginx's ngx_http_upstream_get_peer() (lines 703-779).
func (rr *RRPicker) pickFromGroup(group *PeerGroup) *Peer {
	group.mu.Lock()
	defer group.mu.Unlock()

	now := time.Now()
	var best *Peer
	total := 0

	for _, peer := range group.Peers {
		// Skip already tried (nginx: rrp->tried[n] & m)
		if rr.tried[peer] {
			continue
		}

		// Skip down peers
		if peer.Down {
			continue
		}

		// Skip peers exceeding max_fails within fail_timeout
		// (nginx lines 736-741)
		if peer.MaxFails > 0 &&
			peer.fails >= peer.MaxFails &&
			now.Sub(peer.checked) <= peer.FailTimeout {
			continue
		}

		// Skip peers at max_conns
		if peer.MaxConns > 0 && peer.conns >= peer.MaxConns {
			continue
		}

		// --- Core algorithm (nginx lines 747-757) ---
		peer.currentWeight += peer.effectiveWeight
		total += peer.effectiveWeight

		// Slowly recover effective_weight (nginx line 750-752)
		if peer.effectiveWeight < peer.Weight {
			peer.effectiveWeight++
		}

		if best == nil || peer.currentWeight > best.currentWeight {
			best = peer
		}
	}

	if best == nil {
		return nil
	}

	// Subtract total from winner (nginx line 772)
	best.currentWeight -= total

	if now.Sub(best.checked) > best.FailTimeout {
		best.checked = now
	}

	best.conns++
	rr.tried[best] = true

	return best
}

// Done implements Picker.Done.
// Corresponds to nginx's ngx_http_upstream_free_round_robin_peer() (lines 782-863).
func (rr *RRPicker) Done(peer *Peer, failed bool) {
	if peer == nil {
		return
	}

	// Determine which group owns this peer
	group := rr.primary
	for _, p := range rr.primary.Peers {
		if p == peer {
			goto found
		}
	}
	if rr.backup != nil {
		group = rr.backup
	}
found:

	group.mu.Lock()
	defer group.mu.Unlock()

	now := time.Now()

	if failed {
		// nginx lines 819-841
		peer.fails++
		peer.accessed = now
		peer.checked = now

		if peer.MaxFails > 0 {
			peer.effectiveWeight -= peer.Weight / peer.MaxFails
		}
		if peer.effectiveWeight < 0 {
			peer.effectiveWeight = 0
		}
	} else {
		// nginx lines 847-849: reset fails if recovered
		if peer.accessed.Before(peer.checked) {
			peer.fails = 0
		}
	}

	peer.conns--
}

// Tried returns whether a peer has been tried (used by wrapping algorithms).
func (rr *RRPicker) Tried(p *Peer) bool {
	return rr.tried[p]
}

// SetTried marks a peer as tried (used by wrapping algorithms).
func (rr *RRPicker) SetTried(p *Peer) {
	rr.tried[p] = true
}

// PrimaryGroup returns the primary peer group.
func (rr *RRPicker) PrimaryGroup() *PeerGroup { return rr.primary }

// BackupGroup returns the backup peer group.
func (rr *RRPicker) BackupGroup() *PeerGroup { return rr.backup }
