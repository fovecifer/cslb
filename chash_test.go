package cslb

import (
	"hash/crc32"
	"sort"
	"strconv"
	"testing"
)

func TestSplitKetamaHostPort(t *testing.T) {
	tests := []struct {
		addr     string
		wantHost string
		wantPort string
	}{
		{"10.0.0.1:8080", "10.0.0.1", "8080"},
		{"10.0.0.1", "10.0.0.1", ""},
		{"myhost", "myhost", ""},
		{"myhost:80", "myhost", "80"},
		{"http://10.0.0.1:8080", "10.0.0.1", "8080"},
		{"http://10.0.0.1:8080/some/path", "10.0.0.1", "8080"},
		{"https://api.example.com:443/v1?x=y", "api.example.com", "443"},
		{"http://10.0.0.1", "10.0.0.1", ""},
		{"[::1]:8080", "[::1]", "8080"},
		{"[::1]", "[::1]", ""},
		{"http://[::1]:8080/api", "[::1]", "8080"},
	}
	for _, tt := range tests {
		gotHost, gotPort := splitKetamaHostPort(tt.addr)
		if gotHost != tt.wantHost || gotPort != tt.wantPort {
			t.Errorf("splitKetamaHostPort(%q) = (%q, %q), want (%q, %q)",
				tt.addr, gotHost, gotPort, tt.wantHost, tt.wantPort)
		}
	}
}

// TestBuildChashRing_PointCount verifies each peer occupies weight*160 ring
// points before deduplication, matching nginx's npoints = total_weight * 160.
func TestBuildChashRing_PointCount(t *testing.T) {
	peers := []*Peer{
		{Addr: "10.0.0.1:8080", Weight: 1},
		{Addr: "10.0.0.2:8080", Weight: 5},
	}
	for _, p := range peers {
		p.init()
	}
	ring := buildChashRing(peers)

	// CRC32 collisions for ~1000 points are vanishingly rare; expect exact count.
	expected := (1 + 5) * consistentReplicas
	if len(ring) != expected {
		t.Fatalf("ring length = %d, want %d", len(ring), expected)
	}

	// Sorted ascending by hash.
	if !sort.SliceIsSorted(ring, func(i, j int) bool { return ring[i].hash < ring[j].hash }) {
		t.Fatalf("ring is not sorted by hash")
	}

	// No duplicate hashes.
	for i := 1; i < len(ring); i++ {
		if ring[i].hash == ring[i-1].hash {
			t.Fatalf("duplicate hash at index %d: %x", i, ring[i].hash)
		}
	}

	// Per-peer point counts proportional to weight.
	counts := map[*Peer]int{}
	for _, pt := range ring {
		counts[pt.peer]++
	}
	if counts[peers[0]] != 1*consistentReplicas {
		t.Errorf("peer0 points = %d, want %d", counts[peers[0]], 1*consistentReplicas)
	}
	if counts[peers[1]] != 5*consistentReplicas {
		t.Errorf("peer1 points = %d, want %d", counts[peers[1]], 5*consistentReplicas)
	}
}

// TestBuildChashRing_KetamaCompatibility verifies that the first ring point
// for a peer matches what Cache::Memcached::Fast / nginx produce, i.e.
// crc32(host \0 port + 4 zero bytes). This pins down byte-level wire
// compatibility — change it and existing keys will silently rehash.
func TestBuildChashRing_KetamaCompatibility(t *testing.T) {
	peer := &Peer{Addr: "10.0.0.1:8080", Weight: 1}
	peer.init()

	ring := buildChashRing([]*Peer{peer})
	if len(ring) == 0 {
		t.Fatalf("empty ring")
	}

	// First point's hash, by spec: crc32("10.0.0.1\08080" + 0x00 0x00 0x00 0x00)
	seed := []byte("10.0.0.1\x008080\x00\x00\x00\x00")
	expected := crc32.ChecksumIEEE(seed)

	// The ring is sorted, so we cannot just take ring[0]. Look it up.
	found := false
	for _, pt := range ring {
		if pt.hash == expected {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ketama point %x not present in ring", expected)
	}
}

func TestBuildChashRing_EmptyPeers(t *testing.T) {
	if got := buildChashRing(nil); got != nil {
		t.Errorf("nil peers should produce nil ring, got %v", got)
	}
	if got := buildChashRing([]*Peer{}); got != nil {
		t.Errorf("empty peers should produce nil ring, got %v", got)
	}
}

// TestBuildChashRing_ReproducibleAcrossCalls ensures the ring layout is a
// pure function of the input peers. A previous unstable sort could let
// CRC32 collisions resolve differently across builds, breaking key routing
// when the same peer config was loaded twice.
func TestBuildChashRing_ReproducibleAcrossCalls(t *testing.T) {
	mkPeers := func() []*Peer {
		ps := []*Peer{
			{Addr: "10.0.0.1:8080", Weight: 3},
			{Addr: "10.0.0.2:8080", Weight: 5},
			{Addr: "10.0.0.3:8080", Weight: 2},
		}
		for _, p := range ps {
			p.init()
		}
		return ps
	}

	a := buildChashRing(mkPeers())
	b := buildChashRing(mkPeers())
	if len(a) != len(b) {
		t.Fatalf("ring sizes differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].hash != b[i].hash {
			t.Errorf("hash at index %d differs: %x vs %x", i, a[i].hash, b[i].hash)
		}
		// The peer pointers necessarily differ (fresh slices), but the peer's
		// Addr should match position-by-position.
		if a[i].peer.Addr != b[i].peer.Addr {
			t.Errorf("peer Addr at index %d differs: %s vs %s", i, a[i].peer.Addr, b[i].peer.Addr)
		}
	}
}

func TestFindChashStart(t *testing.T) {
	points := []chashPoint{
		{hash: 100}, {hash: 200}, {hash: 300}, {hash: 400},
	}
	tests := []struct {
		name string
		key  uint32
		want int
	}{
		{"below all", 0, 0},
		{"exactly first", 100, 0},
		{"between", 250, 2},
		{"exactly last", 400, 3},
		{"above all wraps", 500, 0},
	}
	for _, tt := range tests {
		if got := findChashStart(points, tt.key); got != tt.want {
			t.Errorf("%s: findChashStart(%d) = %d, want %d", tt.name, tt.key, got, tt.want)
		}
	}
}

func TestChashPicker_DeterministicForSameKey(t *testing.T) {
	peers := []*Peer{
		{Addr: "10.0.0.1:8080", Weight: 1},
		{Addr: "10.0.0.2:8080", Weight: 1},
		{Addr: "10.0.0.3:8080", Weight: 1},
	}
	for _, p := range peers {
		p.init()
	}
	h := NewHashConsistent(peers)

	// Run twice with the same key, verify both times pick the same peer.
	first := h.NewPickerForKey("user-42").Pick()
	second := h.NewPickerForKey("user-42").Pick()
	if first == nil || second == nil {
		t.Fatalf("got nil peer (first=%v second=%v)", first, second)
	}
	if first != second {
		t.Errorf("same key routed to different peers: %s vs %s", first.Addr, second.Addr)
	}
}

func TestChashPicker_FallsThroughDownPeer(t *testing.T) {
	peers := []*Peer{
		{Addr: "10.0.0.1:8080", Weight: 1},
		{Addr: "10.0.0.2:8080", Weight: 1},
		{Addr: "10.0.0.3:8080", Weight: 1},
	}
	for _, p := range peers {
		p.init()
	}

	// First learn which peer "abc" naturally maps to.
	h := NewHashConsistent(peers)
	target := h.NewPickerForKey("abc").Pick()
	if target == nil {
		t.Fatalf("nil peer on first pick")
	}

	// Mark it Down and rebuild — picker should walk to the next ring peer.
	target.Down = true
	h2 := NewHashConsistent(peers)
	got := h2.NewPickerForKey("abc").Pick()
	if got == nil {
		t.Fatalf("nil peer after marking primary down")
	}
	if got == target {
		t.Errorf("picker still returned down peer %s", target.Addr)
	}
}

func TestChashPicker_EmptyKeyFallsBackToRR(t *testing.T) {
	peers := []*Peer{
		{Addr: "10.0.0.1:8080", Weight: 1},
		{Addr: "10.0.0.2:8080", Weight: 1},
	}
	for _, p := range peers {
		p.init()
	}
	h := NewHashConsistent(peers)
	if got := h.NewPickerForKey("").Pick(); got == nil {
		t.Errorf("empty key should fall back to RR and return a peer, got nil")
	}
}

// TestChashPicker_StabilityOnPeerRemoval is the load-bearing test for this
// feature. With plain modulo hashing, removing one peer remaps ~75% of keys
// for a 4-peer setup. Ketama keeps it within ~1/N + variance.
func TestChashPicker_StabilityOnPeerRemoval(t *testing.T) {
	const numKeys = 10000

	peers4 := []*Peer{
		{Addr: "10.0.0.1:8080", Weight: 1},
		{Addr: "10.0.0.2:8080", Weight: 1},
		{Addr: "10.0.0.3:8080", Weight: 1},
		{Addr: "10.0.0.4:8080", Weight: 1},
	}
	for _, p := range peers4 {
		p.init()
	}
	h4 := NewHashConsistent(peers4)

	beforeAddr := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		k := keyOf(i)
		p := h4.NewPickerForKey(k).Pick()
		if p == nil {
			t.Fatalf("nil peer for key %q (4-peer)", k)
		}
		beforeAddr[i] = p.Addr
	}

	// Drop the third peer.
	peers3 := []*Peer{
		{Addr: "10.0.0.1:8080", Weight: 1},
		{Addr: "10.0.0.2:8080", Weight: 1},
		{Addr: "10.0.0.4:8080", Weight: 1},
	}
	for _, p := range peers3 {
		p.init()
	}
	h3 := NewHashConsistent(peers3)

	moved := 0
	for i := 0; i < numKeys; i++ {
		k := keyOf(i)
		p := h3.NewPickerForKey(k).Pick()
		if p == nil {
			t.Fatalf("nil peer for key %q (3-peer)", k)
		}
		if p.Addr != beforeAddr[i] {
			moved++
		}
	}

	// Expected fraction ~1/N = 25%. Allow up to 35% to absorb 160-replica variance.
	pct := float64(moved) / float64(numKeys) * 100
	t.Logf("ketama remap on 1/4 peer removal: %d/%d (%.2f%%)", moved, numKeys, pct)
	if pct > 35 {
		t.Errorf("remap rate %.2f%% exceeds 35%% — ketama failed to localize disturbance", pct)
	}
}

func keyOf(i int) string {
	// Deterministic, varied keyspace using a Knuth multiplicative hash to
	// spread the integer suffix across the input space.
	return "key-" + strconv.Itoa(i) + "-" + strconv.Itoa(i*2654435761)
}
