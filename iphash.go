package cslb

import (
	"net"
	"time"
)

// ---------- IPHash Balancer ----------
// Corresponds to nginx's ngx_http_upstream_ip_hash_module.c
//
// Uses client IP to consistently route to the same backend. If no client IP is
// supplied, it falls back to round-robin selection.

const maxIPHashTries = 20 // nginx: 20 tries before fallback

// IPHash implements IP-hash based load balancing.
type IPHash struct {
	rr *RoundRobin
}

// NewIPHash creates an IP-hash balancer.
// Corresponds to nginx's ngx_http_upstream_init_ip_hash().
func NewIPHash(peers []*Peer) *IPHash {
	return &IPHash{rr: NewRoundRobin(peers)}
}

func (h *IPHash) NewPicker() Picker {
	return &ipHashPicker{
		RRPicker: NewRRPicker(h.rr.primary, h.rr.backup),
		hash:     89,
	}
}

// NewPickerForIP creates a picker for a specific client IP.
func (h *IPHash) NewPickerForIP(clientIP net.IP) Picker {
	return &ipHashPicker{
		RRPicker: NewRRPicker(h.rr.primary, h.rr.backup),
		clientIP: clientIP,
		hash:     89,
	}
}

// ---------- ipHashPicker: per-request state ----------
// Corresponds to nginx's ngx_http_upstream_ip_hash_peer_data_t:
//
//	typedef struct {
//	    ngx_http_upstream_rr_peer_data_t  rrp;           // ← embedded RR (first field)
//	    ngx_uint_t                        hash;
//	    u_char                           *addr;
//	    u_char                            addrlen;
//	    u_char                            tries;
//	    ngx_event_get_peer_pt             get_rr_peer;   // ← saved RR fallback
//	} ngx_http_upstream_ip_hash_peer_data_t;

type ipHashPicker struct {
	*RRPicker        // embedded RR picker (nginx: rrp as first field)
	clientIP  net.IP // parsed client IP
	hash      uint32 // rolling nginx hash, initialized to 89
	tries     int    // number of rejected hash candidates
}

// Pick selects a peer based on client IP hash.
// Corresponds to nginx's ngx_http_upstream_get_ip_hash_peer() (lines 149-226).
func (p *ipHashPicker) Pick() *Peer {
	group := p.PrimaryGroup()

	if len(group.Peers) < 2 {
		return p.RRPicker.Pick()
	}

	// If no client IP, fall back to round-robin
	if len(p.clientIP) == 0 {
		return p.RRPicker.Pick()
	}

	// Use first 3 bytes for IPv4 (nginx behavior: /24 subnet)
	// Full bytes for IPv6
	addr := p.clientIP.To4()
	if addr == nil {
		addr = p.clientIP.To16()
	}
	if addr == nil {
		return p.RRPicker.Pick()
	}
	addrLen := 3
	if len(addr) > 4 {
		addrLen = len(addr)
	}
	if addrLen > len(addr) {
		addrLen = len(addr)
	}

	group.mu.Lock()
	now := time.Now()
	hash := p.hash

	for p.tries <= maxIPHashTries {
		// nginx folds the address into the rolling hash once per candidate.
		for i := 0; i < addrLen; i++ {
			hash = (hash*113 + uint32(addr[i])) % 6271
		}
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

		// Check availability (same checks as RR: down, tried, max_fails, max_conns)
		if !selected.Down && !p.Tried(selected) && peerAvailable(selected) {
			p.commitPeer(selected, now)
			p.hash = hash
			group.mu.Unlock()
			return selected
		}

		p.tries++
	}

	group.mu.Unlock()
	return p.RRPicker.Pick()
}

// Done delegates to the embedded RR picker.
// nginx's ip_hash does NOT override free — it inherits from round-robin.
func (p *ipHashPicker) Done(peer *Peer, failed bool) {
	p.RRPicker.Done(peer, failed)
}
