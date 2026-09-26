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
	primary     *PeerGroup
	backup      *PeerGroup
	tried       map[*Peer]peerMark // nginx uses a bitmap; Go map is cleaner
	backupPhase bool

	// probeChecked records the checked timestamps this picker advanced in
	// commitPeer, so a neutral release can put them back.
	probeChecked map[*Peer]probeCheck
}

// peerMark records what one picker has done with a peer.
type peerMark uint8

const (
	// markTried excludes the peer from further selection by this picker.
	markTried peerMark = 1 << iota
	// markActive means the peer was selected and its Done is still pending.
	// It makes Done idempotent: a repeated Done, or a Done for a peer this
	// picker never selected, must not release a connection count twice.
	markActive
)

// probeCheck remembers one advanced checked timestamp: prior is the value to
// restore, seq is the peer's version counter immediately after this picker
// wrote it. Comparing seq lets a neutral release detect that a concurrent
// request has since recorded a real outcome and leave that newer, authoritative
// timestamp alone.
type probeCheck struct {
	prior time.Time
	seq   uint64
}

// NewRRPicker creates a round-robin picker.
func NewRRPicker(primary, backup *PeerGroup) *RRPicker {
	return &RRPicker{
		primary: primary,
		backup:  backup,
		tried:   make(map[*Peer]peerMark),
	}
}

// Pick implements Picker.Pick using smooth weighted round-robin.
// Corresponds to nginx's ngx_http_upstream_get_round_robin_peer().
func (rr *RRPicker) Pick() *Peer {
	return rr.pickWithBackup(rr.pickFromGroup)
}

func (rr *RRPicker) pickWithBackup(pick func(*PeerGroup) *Peer) *Peer {
	if !rr.backupPhase {
		if p := rr.pickFromCurrentGroup(rr.primary, pick); p != nil {
			return p
		}
	}

	if group := rr.backupGroupForPick(); group != nil {
		return rr.pickFromCurrentGroup(group, pick)
	}

	return nil
}

func (rr *RRPicker) pickFromCurrentGroup(group *PeerGroup, pick func(*PeerGroup) *Peer) *Peer {
	if group.single {
		return rr.pickFromGroup(group)
	}
	return pick(group)
}

func (rr *RRPicker) backupGroupForPick() *PeerGroup {
	if rr.backup == nil || len(rr.backup.Peers) == 0 {
		return nil
	}

	if !rr.backupPhase {
		rr.backupPhase = true
		for _, bp := range rr.backup.Peers {
			delete(rr.tried, bp)
		}
	}

	return rr.backup
}

// pickFromGroup is the core smooth weighted round-robin algorithm.
// Corresponds to nginx's ngx_http_upstream_get_peer() (lines 703-779).
func (rr *RRPicker) pickFromGroup(group *PeerGroup) *Peer {
	group.mu.Lock()
	defer group.mu.Unlock()

	if group.single {
		peer := group.Peers[0]
		if rr.Tried(peer) || peer.Down ||
			(peer.MaxConns > 0 && peer.conns >= peer.MaxConns) {
			return nil
		}

		peer.conns++
		rr.tried[peer] = markTried | markActive
		return peer
	}

	now := time.Now()
	best := smoothWeightedPeer(group.Peers, func(peer *Peer) bool {
		// Skip already tried (nginx: rrp->tried[n] & m)
		if rr.Tried(peer) {
			return false
		}

		// Skip down peers
		if peer.Down {
			return false
		}

		// Skip peers exceeding max_fails within fail_timeout
		// (nginx lines 736-741)
		if peer.MaxFails > 0 &&
			peer.fails >= peer.MaxFails &&
			now.Sub(peer.checked) <= peer.FailTimeout {
			return false
		}

		// Skip peers at max_conns
		if peer.MaxConns > 0 && peer.conns >= peer.MaxConns {
			return false
		}
		return true
	})

	if best == nil {
		return nil
	}

	return rr.commitPeer(best, now)
}

// smoothWeightedPeer runs nginx's smooth weighted round-robin core over the
// eligible peers. The caller must hold the owning PeerGroup lock.
func smoothWeightedPeer(peers []*Peer, eligible func(*Peer) bool) *Peer {
	var best *Peer
	total := 0

	for _, peer := range peers {
		if !eligible(peer) {
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

	if best != nil {
		best.currentWeight -= total
	}
	return best
}

// commitPeer records a selected winner. The caller must hold the owning
// PeerGroup lock. The single-peer fast path intentionally does not use this
// helper because nginx does not update checked in that path.
func (rr *RRPicker) commitPeer(peer *Peer, now time.Time) *Peer {
	if now.Sub(peer.checked) > peer.FailTimeout {
		// Advancing checked starts this peer's recovery probe: while it is set
		// to now, a peer at max_fails is skipped by everyone else. Remember the
		// previous value so a neutral release can hand the probe back instead
		// of quarantining the peer for another fail_timeout over an attempt
		// that never reached it.
		if rr.probeChecked == nil {
			rr.probeChecked = make(map[*Peer]probeCheck, 1)
		}
		prior := peer.checked
		peer.setChecked(now)
		rr.probeChecked[peer] = probeCheck{prior: prior, seq: peer.checkedSeq}
	}
	peer.conns++
	rr.tried[peer] = markTried | markActive
	return peer
}

// Done implements Picker.Done.
// Corresponds to nginx's ngx_http_upstream_free_round_robin_peer() (lines 782-863).
// Only the first Done for a peer selected by this picker has an effect; later
// calls, and calls for peers this picker never selected, are ignored.
func (rr *RRPicker) Done(peer *Peer, failed bool) {
	rr.done(peer, failed, false)
}

// doneNeutral releases a selected peer without changing its health state.
// Transport uses this for caller cancellation and local replay/configuration
// errors that say nothing about the upstream server.
func (rr *RRPicker) doneNeutral(peer *Peer) {
	rr.done(peer, false, true)
}

func (rr *RRPicker) done(peer *Peer, failed bool, neutral bool) {
	// Ignore a Done for a peer that has no pending selection by this picker:
	// nil, never selected, or already released.
	if peer == nil || rr.tried[peer]&markActive == 0 {
		return
	}
	rr.tried[peer] &^= markActive

	group := rr.primary
	if peer.Backup && rr.backup != nil {
		group = rr.backup
	}

	group.mu.Lock()
	defer group.mu.Unlock()

	if group.single {
		peer.fails = 0
		peer.conns--
		return
	}
	if neutral {
		// This attempt said nothing about the upstream, so give back the
		// recovery probe that selection consumed. Without this a peer whose
		// fail_timeout had just elapsed would be re-quarantined for another
		// full window by a caller cancellation or a local defect.
		if probe, ok := rr.probeChecked[peer]; ok {
			// Roll back only if no one has written checked since this picker
			// did. A concurrent request may have recorded a real failure in the
			// meantime, and that outcome outranks our missing one.
			if peer.checkedSeq == probe.seq {
				peer.setChecked(probe.prior)
			}
			delete(rr.probeChecked, peer)
		}
		peer.conns--
		return
	}

	now := time.Now()

	if failed {
		// nginx lines 819-841
		peer.fails++
		peer.accessed = now
		peer.setChecked(now)

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
	return rr.tried[p]&markTried != 0
}

// SetTried marks a peer as tried (used by wrapping algorithms).
func (rr *RRPicker) SetTried(p *Peer) {
	rr.tried[p] |= markTried
}

// PrimaryGroup returns the primary peer group.
func (rr *RRPicker) PrimaryGroup() *PeerGroup { return rr.primary }

// BackupGroup returns the backup peer group.
func (rr *RRPicker) BackupGroup() *PeerGroup { return rr.backup }
