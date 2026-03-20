package cslb

import "hash/crc32"

// ---------- Hash Balancer ----------
// Corresponds to nginx's ngx_http_upstream_hash_module.c
//
// Routes requests to a consistent peer based on a user-provided key.
// For example: hash by request URI, cookie, header value, etc.

const maxHashTries = 20 // nginx: fallback to RR after 20 tries

// Hash implements key-based hash load balancing.
type Hash struct {
	rr *RoundRobin
}

// NewHash creates a hash-based balancer.
// Corresponds to nginx's ngx_http_upstream_init_hash().
func NewHash(peers []*Peer) *Hash {
	return &Hash{rr: NewRoundRobin(peers)}
}

func (h *Hash) NewPicker() Picker {
	// No key yet — caller should use NewPickerForKey
	return h.NewPickerForKey("")
}

// NewPickerForKey creates a picker for a specific hash key.
// The key is typically derived from the request (URI, header, cookie, etc.)
func (h *Hash) NewPickerForKey(key string) Picker {
	return &hashPicker{
		RRPicker: NewRRPicker(h.rr.primary, h.rr.backup),
		key:      key,
	}
}

// ---------- hashPicker ----------
// Corresponds to nginx's ngx_http_upstream_hash_peer_data_t:
//
//	typedef struct {
//	    ngx_http_upstream_rr_peer_data_t    rrp;          // ← embedded RR
//	    ngx_http_upstream_hash_conf_t      *conf;
//	    ngx_str_t                           key;
//	    ngx_uint_t                          hash;
//	    ngx_uint_t                          tries;
//	    ngx_event_get_peer_pt               get_rr_peer;  // ← saved RR fallback
//	} ngx_http_upstream_hash_peer_data_t;

type hashPicker struct {
	*RRPicker          // embedded RR (nginx: rrp first field)
	key       string   // hash key (evaluated from request)
	hashTries int      // number of hash attempts
}

// Pick selects a peer by hashing the key.
// Corresponds to nginx's ngx_http_upstream_get_hash_peer() (lines 168-233).
func (p *hashPicker) Pick() *Peer {
	group := p.PrimaryGroup()

	// Empty key or no peers → fall back to round-robin
	if p.key == "" || len(group.Peers) == 0 {
		return p.RRPicker.Pick()
	}

	group.mu.Lock()
	defer group.mu.Unlock()

	// nginx: hash = crc32(key)
	hash := crc32.ChecksumIEEE([]byte(p.key))

	for p.hashTries < maxHashTries {
		p.hashTries++

		// nginx: w = hash % total_weight, then walk peers
		w := int(hash % uint32(group.TotalWeight))

		var selected *Peer
		for _, peer := range group.Peers {
			w -= peer.Weight
			if w < 0 {
				selected = peer
				break
			}
		}

		if selected == nil {
			break
		}

		if !selected.Down && !p.Tried(selected) && peerAvailable(selected) {
			selected.conns++
			p.SetTried(selected)
			return selected
		}

		// nginx: hash = (hash + 113) rehash
		hash += 113
	}

	group.mu.Unlock()

	// Fallback to round-robin (nginx: hp->get_rr_peer)
	result := p.RRPicker.Pick()

	group.mu.Lock()
	return result
}

// Done delegates to the embedded RR picker.
func (p *hashPicker) Done(peer *Peer, failed bool) {
	p.RRPicker.Done(peer, failed)
}
