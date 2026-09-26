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

// Weight limits. They are far above realistic nginx weights and keep every
// algorithm's weight arithmetic in range; Transport rejects configurations that
// exceed them.
const (
	// maxServerWeight bounds a single server's weight.
	maxServerWeight = 1 << 16
	// maxUpstreamWeight bounds the sum of server weights in one upstream, so
	// hash-based selection can reduce a hash modulo a uint32 total weight.
	maxUpstreamWeight = 1 << 24
	// maxConsistentHashWeight bounds the sum of server weights for
	// HashConsistent, whose ring holds 160 points per unit of weight.
	maxConsistentHashWeight = 1 << 16
)

// hashModulus returns a group's total weight as the modulus for hash-based
// selection. It reports false when the total weight is not in (0, MaxUint32],
// for example on a group built directly with out-of-range weights, so callers
// fall back to round-robin instead of dividing by zero or truncating.
func hashModulus(group *PeerGroup) (uint32, bool) {
	modulus := uint32(group.TotalWeight)
	if group.TotalWeight <= 0 || int64(modulus) != int64(group.TotalWeight) {
		return 0, false
	}
	return modulus, true
}

// compareLoad compares the connection counts of a and b normalized by weight,
// returning the sign of a.conns/a.Weight - b.conns/b.Weight. It
// cross-multiplies like nginx, in 64 bits so the products cannot overflow int
// on 32-bit platforms.
func compareLoad(a, b *Peer) int {
	x := int64(a.conns) * int64(b.Weight)
	y := int64(b.conns) * int64(a.Weight)
	if x < y {
		return -1
	}
	if x > y {
		return 1
	}
	return 0
}
