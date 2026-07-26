package cslb

import (
	"encoding/json"
	"net"
	"os"
	"testing"
)

type nginxGoldenFixture struct {
	Source         string               `json:"source"`
	Peers          []nginxGoldenPeer    `json:"peers"`
	Hash           []nginxGoldenMapping `json:"hash"`
	IPHash         []nginxGoldenMapping `json:"ip_hash"`
	HashConsistent []nginxGoldenMapping `json:"hash_consistent"`
}

type nginxGoldenPeer struct {
	Address string `json:"address"`
	Weight  int    `json:"weight"`
}

type nginxGoldenMapping struct {
	Input string `json:"input"`
	Peer  string `json:"peer"`
}

func TestNginxCompatibilityGoldenVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/nginx_compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture nginxGoldenFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Source == "" || len(fixture.Peers) == 0 {
		t.Fatal("nginx compatibility fixture is missing provenance or peers")
	}

	t.Run("hash", func(t *testing.T) {
		balancer := NewHash(goldenPeers(fixture.Peers))
		assertGoldenMappings(t, fixture.Hash, func(input string) Picker {
			return balancer.NewPickerForKey(input)
		})
	})
	t.Run("ip_hash", func(t *testing.T) {
		balancer := NewIPHash(goldenPeers(fixture.Peers))
		assertGoldenMappings(t, fixture.IPHash, func(input string) Picker {
			return balancer.NewPickerForIP(net.ParseIP(input))
		})
	})
	t.Run("hash_consistent", func(t *testing.T) {
		balancer := NewHashConsistent(goldenPeers(fixture.Peers))
		assertGoldenMappings(t, fixture.HashConsistent, func(input string) Picker {
			return balancer.NewPickerForKey(input)
		})
	})
}

func goldenPeers(configs []nginxGoldenPeer) []*Peer {
	peers := make([]*Peer, len(configs))
	for index, config := range configs {
		peers[index] = &Peer{Addr: config.Address, Weight: config.Weight}
	}
	return peers
}

func assertGoldenMappings(
	t *testing.T,
	mappings []nginxGoldenMapping,
	newPicker func(string) Picker,
) {
	t.Helper()
	if len(mappings) == 0 {
		t.Fatal("golden mapping list is empty")
	}
	for _, mapping := range mappings {
		picker := newPicker(mapping.Input)
		peer := picker.Pick()
		if peer == nil {
			t.Fatalf("input %q: Pick returned nil", mapping.Input)
		}
		picker.Done(peer, false)
		if peer.Addr != mapping.Peer {
			t.Fatalf("input %q mapped to %q, want frozen nginx peer %q",
				mapping.Input, peer.Addr, mapping.Peer)
		}
	}
}
