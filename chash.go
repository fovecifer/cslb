package cslb

import (
	"encoding/binary"
	"hash/crc32"
	"sort"
	"strings"
)

// ---------- Consistent (ketama) Hash Balancer ----------
// Corresponds to nginx's "hash <key> consistent" mode in
// ngx_http_upstream_hash_module.c (ngx_http_upstream_init_chash and
// ngx_http_upstream_get_chash_peer).
//
// Ring construction is byte-compatible with Cache::Memcached::Fast:
//
//	crc32(HOST \0 PORT PREV_HASH_LE)
//
// chained for weight*160 points per peer. This means cslb shares one ring
// with any standard ketama client (memcached, nginx) configured against the
// same backend addresses.

const consistentReplicas = 160

type chashPoint struct {
	hash uint32
	peer *Peer
}

// HashConsistent implements ketama-style consistent hashing.
//
// Compared to plain modulo Hash, removing or adding one peer redistributes
// only ~1/N of keys instead of nearly all of them, avoiding cache-miss
// storms during rolling restarts and scaling events.
type HashConsistent struct {
	rr     *RoundRobin
	points []chashPoint // sorted ascending by hash, deduplicated
}

// NewHashConsistent creates a consistent-hash balancer.
// Corresponds to nginx's ngx_http_upstream_init_chash().
func NewHashConsistent(peers []*Peer) *HashConsistent {
	rr := NewRoundRobin(peers)
	return &HashConsistent{
		rr:     rr,
		points: buildChashRing(rr.primary.Peers),
	}
}

func (h *HashConsistent) NewPicker() Picker {
	// No key yet — caller should use NewPickerForKey for the request's key.
	return h.NewPickerForKey("")
}

// NewPickerForKey creates a picker bound to a hash key derived from the
// request (URI, header, cookie, etc.).
func (h *HashConsistent) NewPickerForKey(key string) Picker {
	return &chashPicker{
		RRPicker: NewRRPicker(h.rr.primary, h.rr.backup),
		balancer: h,
		key:      key,
	}
}

// Primary returns the primary peer group.
func (h *HashConsistent) Primary() *PeerGroup { return h.rr.primary }

// Backup returns the backup peer group.
func (h *HashConsistent) Backup() *PeerGroup { return h.rr.backup }

// ---------- chashPicker: per-request state ----------
// Mirrors nginx's ngx_http_upstream_hash_peer_data_t with the consistent
// extension fields (cursor along the ring, retry counter).

type chashPicker struct {
	*RRPicker
	balancer *HashConsistent
	key      string
	started  bool
	cursor   int
	tries    int
}

// Pick selects a peer by walking the ring forward from the key's hash.
// Corresponds to nginx's ngx_http_upstream_get_chash_peer().
func (p *chashPicker) Pick() *Peer {
	points := p.balancer.points
	if p.key == "" || len(points) == 0 {
		return p.RRPicker.Pick()
	}

	if !p.started {
		p.cursor = findChashStart(points, crc32.ChecksumIEEE([]byte(p.key)))
		p.started = true
	}

	group := p.PrimaryGroup()
	group.mu.Lock()
	defer group.mu.Unlock()

	for p.tries < maxHashTries {
		peer := points[(p.cursor+p.tries)%len(points)].peer
		p.tries++
		if !peer.Down && !p.Tried(peer) && peerAvailable(peer) {
			peer.conns++
			p.SetTried(peer)
			return peer
		}
	}

	// Fallback to round-robin once the ring has exhausted its budget,
	// matching nginx's hp->tries > 20 fallback to hp->get_rr_peer.
	group.mu.Unlock()
	result := p.RRPicker.Pick()
	group.mu.Lock()
	return result
}

// Done delegates to the embedded RR picker.
// nginx's chash does NOT override free — it inherits from round-robin.
func (p *chashPicker) Done(peer *Peer, failed bool) {
	p.RRPicker.Done(peer, failed)
}

// ---------- Ring construction ----------

// buildChashRing produces a ketama ring over peers. Each peer contributes
// weight*160 points, position = chained crc32(host \0 port PREV_HASH_LE).
// The result is sorted ascending and deduplicated, matching the layout
// nginx's ngx_http_upstream_update_chash() builds.
func buildChashRing(peers []*Peer) []chashPoint {
	totalPoints := 0
	for _, p := range peers {
		if p == nil || p.Weight <= 0 {
			continue
		}
		totalPoints += p.Weight * consistentReplicas
	}
	if totalPoints == 0 {
		return nil
	}

	pts := make([]chashPoint, 0, totalPoints)
	for _, p := range peers {
		if p == nil || p.Weight <= 0 {
			continue
		}
		host, port := splitKetamaHostPort(p.Addr)
		seedLen := len(host) + 1 + len(port)
		buf := make([]byte, seedLen+4)
		copy(buf, host)
		buf[len(host)] = 0
		copy(buf[len(host)+1:], port)
		// trailing 4 bytes hold the rolling prev-hash, initialized to zero.

		n := p.Weight * consistentReplicas
		for j := 0; j < n; j++ {
			h := crc32.ChecksumIEEE(buf)
			pts = append(pts, chashPoint{hash: h, peer: p})
			binary.LittleEndian.PutUint32(buf[seedLen:], h)
		}
	}

	// Stable sort so that CRC32 collisions retain their generation order;
	// the dedupe step below then keeps the first generated point for any
	// colliding hash, making the ring layout reproducible across builds.
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].hash < pts[j].hash })

	// Deduplicate adjacent identical hashes, keeping the first occurrence —
	// nginx does the same so the ring layout is reproducible.
	out := pts[:1]
	for i := 1; i < len(pts); i++ {
		if pts[i].hash != out[len(out)-1].hash {
			out = append(out, pts[i])
		}
	}
	return out
}

// findChashStart returns the index of the first point with hash >= keyHash,
// wrapping to 0 when keyHash exceeds every point on the ring.
// Corresponds to nginx's ngx_http_upstream_find_chash_point().
func findChashStart(points []chashPoint, keyHash uint32) int {
	idx := sort.Search(len(points), func(i int) bool {
		return points[i].hash >= keyHash
	})
	if idx == len(points) {
		idx = 0
	}
	return idx
}

// splitKetamaHostPort extracts (host, port) from a peer address using the
// same right-to-left scan nginx uses. URL prefixes and paths are stripped
// first so callers can pass either "10.0.0.1:8080" or
// "http://10.0.0.1:8080/api" and get the same seed bytes.
func splitKetamaHostPort(addr string) (host, port string) {
	if i := strings.Index(addr, "://"); i >= 0 {
		rest := addr[i+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		addr = rest
	}
	for j := len(addr) - 1; j >= 0; j-- {
		c := addr[j]
		if c == ':' {
			return addr[:j], addr[j+1:]
		}
		if c < '0' || c > '9' {
			return addr, ""
		}
	}
	return addr, ""
}
