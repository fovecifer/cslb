package cslb

import "time"

// peerAvailable checks if a peer can accept connections.
// Shared by all algorithms for max_fails and max_conns checks.
func peerAvailable(peer *Peer) bool {
	now := time.Now()

	if peer.MaxFails > 0 &&
		peer.fails >= peer.MaxFails &&
		now.Sub(peer.checked) <= peer.FailTimeout {
		return false
	}

	if peer.MaxConns > 0 && peer.conns >= peer.MaxConns {
		return false
	}

	return true
}
