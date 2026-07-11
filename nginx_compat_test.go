package cslb

import (
	"hash/crc32"
	"net"
	"strconv"
	"testing"
)

func TestHash_NginxCompatibleMapping(t *testing.T) {
	weights := []int{5, 3, 2}
	peers := []*Peer{
		{Addr: "A", Weight: weights[0]},
		{Addr: "B", Weight: weights[1]},
		{Addr: "C", Weight: weights[2]},
	}
	balancer := NewHash(peers)

	for i := 0; i < 100; i++ {
		key := "key-" + strconv.Itoa(i)
		picker := balancer.NewPickerForKey(key)
		peer := picker.Pick()
		if peer == nil {
			t.Fatalf("key %q: Pick returned nil", key)
		}
		want := peers[nginxHashPeerIndex(key, weights, nil)]
		if peer != want {
			picker.Done(peer, false)
			t.Fatalf("key %q mapped to %q, want %q", key, peer.Addr, want.Addr)
		}
		picker.Done(peer, false)
	}
}

func TestHash_NginxCompatibleRetryMapping(t *testing.T) {
	weights := []int{5, 3, 2}
	peers := []*Peer{
		{Addr: "A", Weight: weights[0]},
		{Addr: "B", Weight: weights[1]},
		{Addr: "C", Weight: weights[2]},
	}
	picker := NewHash(peers).NewPickerForKey("retry-key")

	first := picker.Pick()
	if first == nil {
		t.Fatal("first Pick returned nil")
	}
	firstIndex := peerIndex(peers, first)
	picker.Done(first, true)

	second := picker.Pick()
	if second == nil {
		t.Fatal("second Pick returned nil")
	}
	wantIndex := nginxHashPeerIndex("retry-key", weights, map[int]bool{firstIndex: true})
	if second != peers[wantIndex] {
		picker.Done(second, false)
		t.Fatalf("retry mapped to %q, want %q", second.Addr, peers[wantIndex].Addr)
	}
	picker.Done(second, false)
}

func TestIPHash_NginxCompatibleMapping(t *testing.T) {
	weights := []int{5, 3, 2}
	peers := []*Peer{
		{Addr: "A", Weight: weights[0]},
		{Addr: "B", Weight: weights[1]},
		{Addr: "C", Weight: weights[2]},
	}
	balancer := NewIPHash(peers)

	for i := 0; i < 100; i++ {
		ip := net.IPv4(192, 0, byte(i), 123)
		picker := balancer.NewPickerForIP(ip)
		peer := picker.Pick()
		if peer == nil {
			t.Fatalf("IP %s: Pick returned nil", ip)
		}
		want := peers[nginxIPHashPeerIndex(ip, weights, nil)]
		if peer != want {
			picker.Done(peer, false)
			t.Fatalf("IP %s mapped to %q, want %q", ip, peer.Addr, want.Addr)
		}
		picker.Done(peer, false)
	}
}

func TestIPHash_NginxCompatibleIPv6Mapping(t *testing.T) {
	weights := []int{5, 3, 2}
	peers := []*Peer{
		{Addr: "A", Weight: weights[0]},
		{Addr: "B", Weight: weights[1]},
		{Addr: "C", Weight: weights[2]},
	}
	balancer := NewIPHash(peers)

	for _, address := range []string{
		"2001:db8::1",
		"2001:db8:1::42",
		"2001:db8:abcd:1234::dead:beef",
		"fd00::ffff",
	} {
		ip := net.ParseIP(address)
		picker := balancer.NewPickerForIP(ip)
		peer := picker.Pick()
		if peer == nil {
			t.Fatalf("IP %s: Pick returned nil", ip)
		}
		want := peers[nginxIPHashPeerIndex(ip, weights, nil)]
		if peer != want {
			picker.Done(peer, false)
			t.Fatalf("IP %s mapped to %q, want %q", ip, peer.Addr, want.Addr)
		}
		picker.Done(peer, false)
	}
}

func TestIPHash_NginxCompatibleRetryMapping(t *testing.T) {
	weights := []int{5, 3, 2}
	peers := []*Peer{
		{Addr: "A", Weight: weights[0]},
		{Addr: "B", Weight: weights[1]},
		{Addr: "C", Weight: weights[2]},
	}
	ip := net.ParseIP("198.51.100.42")
	picker := NewIPHash(peers).NewPickerForIP(ip)

	first := picker.Pick()
	if first == nil {
		t.Fatal("first Pick returned nil")
	}
	firstIndex := peerIndex(peers, first)
	picker.Done(first, true)

	second := picker.Pick()
	if second == nil {
		t.Fatal("second Pick returned nil")
	}
	wantIndex := nginxIPHashPeerIndex(ip, weights, map[int]bool{firstIndex: true})
	if second != peers[wantIndex] {
		picker.Done(second, false)
		t.Fatalf("retry mapped to %q, want %q", second.Addr, peers[wantIndex].Addr)
	}
	picker.Done(second, false)
}

func TestLeastConn_WeightedTieMatchesSmoothRoundRobin(t *testing.T) {
	balancer := NewLeastConn([]*Peer{
		{Addr: "A", Weight: 5},
		{Addr: "B", Weight: 1},
	})

	counts := pickN(t, balancer, 60)
	if counts["A"] != 50 || counts["B"] != 10 {
		t.Fatalf("weighted tie distribution = %v, want A=50 B=10", counts)
	}
}

func TestLeastConn_WeightedTieRemainsSmooth(t *testing.T) {
	balancer := NewLeastConn([]*Peer{
		{Addr: "A", Weight: 5},
		{Addr: "B", Weight: 1},
	})

	sequence := pickSequence(t, balancer, 6)
	if got := maxConsecutive(sequence); got > 3 {
		t.Fatalf("weighted tie sequence = %v, max consecutive = %d, want <= 3", sequence, got)
	}
}

// nginxHashPeerIndex mirrors ngx_http_upstream_get_hash_peer. In particular,
// nginx folds the high 15 bits of CRC32 into a cumulative hash and prefixes
// retries with their decimal rehash number.
func nginxHashPeerIndex(key string, weights []int, unavailable map[int]bool) int {
	totalWeight := sumWeights(weights)
	var cumulative uint32
	for rehash := 0; ; rehash++ {
		input := key
		if rehash > 0 {
			input = strconv.Itoa(rehash) + key
		}
		hash := crc32.ChecksumIEEE([]byte(input))
		cumulative += (hash >> 16) & 0x7fff
		index := weightedPeerIndex(int(cumulative%uint32(totalWeight)), weights)
		if !unavailable[index] {
			return index
		}
	}
}

// nginxIPHashPeerIndex mirrors ngx_http_upstream_get_ip_hash_peer. Each retry
// folds the address bytes into the previous hash once; IPv4 uses the first
// three bytes and IPv6 uses all sixteen.
func nginxIPHashPeerIndex(ip net.IP, weights []int, unavailable map[int]bool) int {
	addr := ip.To4()
	if addr == nil {
		addr = ip.To16()
	}
	addrLen := len(addr)
	if len(addr) == net.IPv4len {
		addrLen = 3
	}

	totalWeight := sumWeights(weights)
	hash := uint32(89)
	for {
		for _, octet := range addr[:addrLen] {
			hash = (hash*113 + uint32(octet)) % 6271
		}
		index := weightedPeerIndex(int(hash%uint32(totalWeight)), weights)
		if !unavailable[index] {
			return index
		}
	}
}

func weightedPeerIndex(weight int, weights []int) int {
	for index, peerWeight := range weights {
		weight -= peerWeight
		if weight < 0 {
			return index
		}
	}
	panic("weight outside configured range")
}

func sumWeights(weights []int) int {
	total := 0
	for _, weight := range weights {
		total += weight
	}
	return total
}

func peerIndex(peers []*Peer, target *Peer) int {
	for index, peer := range peers {
		if peer == target {
			return index
		}
	}
	return -1
}
