package cslb_test

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	cslb "github.com/fovecifer/cslb/v3"
)

// These assignments intentionally spell out the exported API from a consumer
// package. Removing a symbol or changing an incompatible signature fails the
// build before a release can silently break downstream users.
var (
	_ http.RoundTripper          = (*cslb.Transport)(nil)
	_ cslb.NextUpstreamCondition = func(*http.Response, error) bool { return false }
	_ cslb.TransportOption       = func(*cslb.Transport) {}
	_ cslb.ServerOption          = func(*cslb.Peer) {}

	_ cslb.Balancer = (*cslb.RoundRobin)(nil)
	_ cslb.Balancer = (*cslb.LeastConn)(nil)
	_ cslb.Balancer = (*cslb.IPHash)(nil)
	_ cslb.Balancer = (*cslb.Hash)(nil)
	_ cslb.Balancer = (*cslb.Random)(nil)
	_ cslb.Balancer = (*cslb.HashConsistent)(nil)

	_ func([]*cslb.Peer) (*cslb.PeerGroup, *cslb.PeerGroup)       = cslb.BuildGroups
	_ func(cslb.Balancer) *cslb.PickResult                        = cslb.PickOne
	_ func(cslb.Balancer, func(string) error) (*cslb.Peer, error) = cslb.PickWithRetry

	_ func([]*cslb.Peer) *cslb.RoundRobin                   = cslb.NewRoundRobin
	_ func([]*cslb.Peer) *cslb.LeastConn                    = cslb.NewLeastConn
	_ func([]*cslb.Peer) *cslb.IPHash                       = cslb.NewIPHash
	_ func([]*cslb.Peer) *cslb.Hash                         = cslb.NewHash
	_ func([]*cslb.Peer) *cslb.Random                       = cslb.NewRandom
	_ func([]*cslb.Peer) *cslb.Random                       = cslb.NewRandomTwo
	_ func([]*cslb.Peer) *cslb.HashConsistent               = cslb.NewHashConsistent
	_ func(*cslb.PeerGroup, *cslb.PeerGroup) *cslb.RRPicker = cslb.NewRRPicker
	_ func(*cslb.PickResult, bool)                          = (*cslb.PickResult).Done
	_ func(*cslb.RoundRobin) cslb.Picker                    = (*cslb.RoundRobin).NewPicker
	_ func(*cslb.RoundRobin) *cslb.PeerGroup                = (*cslb.RoundRobin).Primary
	_ func(*cslb.RoundRobin) *cslb.PeerGroup                = (*cslb.RoundRobin).Backup
	_ func(*cslb.RRPicker) *cslb.Peer                       = (*cslb.RRPicker).Pick
	_ func(*cslb.RRPicker, *cslb.Peer, bool)                = (*cslb.RRPicker).Done
	_ func(*cslb.RRPicker, *cslb.Peer) bool                 = (*cslb.RRPicker).Tried
	_ func(*cslb.RRPicker, *cslb.Peer)                      = (*cslb.RRPicker).SetTried
	_ func(*cslb.RRPicker) *cslb.PeerGroup                  = (*cslb.RRPicker).PrimaryGroup
	_ func(*cslb.RRPicker) *cslb.PeerGroup                  = (*cslb.RRPicker).BackupGroup
	_ func(*cslb.IPHash, net.IP) cslb.Picker                = (*cslb.IPHash).NewPickerForIP
	_ func(*cslb.Hash, string) cslb.Picker                  = (*cslb.Hash).NewPickerForKey
	_ func(*cslb.HashConsistent, string) cslb.Picker        = (*cslb.HashConsistent).NewPickerForKey
	_ func(*cslb.HashConsistent) *cslb.PeerGroup            = (*cslb.HashConsistent).Primary
	_ func(*cslb.HashConsistent) *cslb.PeerGroup            = (*cslb.HashConsistent).Backup

	_ func(string, ...cslb.ServerConfig) cslb.UpstreamConfig = cslb.Upstream
	_ func(string, ...cslb.ServerOption) cslb.ServerConfig   = cslb.Server
	_ func(...cslb.TransportOption) *cslb.Transport          = cslb.NewTransport
	_ func(...cslb.TransportOption) (*cslb.Transport, error) = cslb.NewTransportE

	_ func(http.RoundTripper) cslb.TransportOption          = cslb.WithRoundTripper
	_ func(time.Duration) cslb.TransportOption              = cslb.WithTimeout
	_ func(time.Duration) cslb.TransportOption              = cslb.WithConnectTimeout
	_ func(int64) cslb.TransportOption                      = cslb.WithMaxBodyBuffer
	_ func(cslb.NextUpstreamCondition) cslb.TransportOption = cslb.WithNextUpstream
	_ func(...int) cslb.TransportOption                     = cslb.WithNextUpstreamCodes
	_ func() cslb.TransportOption                           = cslb.WithNonIdempotentRetries
	_ func(...cslb.UpstreamConfig) cslb.TransportOption     = cslb.WithUpstreams

	_ func(int) cslb.ServerOption           = cslb.Weight
	_ func(int) cslb.ServerOption           = cslb.MaxFails
	_ func(time.Duration) cslb.ServerOption = cslb.FailTimeout
	_ func(int) cslb.ServerOption           = cslb.MaxConns
	_ func() cslb.ServerOption              = cslb.Backup
	_ func() cslb.ServerOption              = cslb.Down

	_ func(cslb.UpstreamConfig) cslb.UpstreamConfig                             = cslb.UpstreamConfig.RoundRobin
	_ func(cslb.UpstreamConfig) cslb.UpstreamConfig                             = cslb.UpstreamConfig.LeastConn
	_ func(cslb.UpstreamConfig) cslb.UpstreamConfig                             = cslb.UpstreamConfig.IPHash
	_ func(cslb.UpstreamConfig, func(*http.Request) string) cslb.UpstreamConfig = cslb.UpstreamConfig.Hash
	_ func(cslb.UpstreamConfig, func(*http.Request) string) cslb.UpstreamConfig = cslb.UpstreamConfig.HashConsistent
	_ func(cslb.UpstreamConfig) cslb.UpstreamConfig                             = cslb.UpstreamConfig.Random
	_ func(cslb.UpstreamConfig) cslb.UpstreamConfig                             = cslb.UpstreamConfig.RandomTwo
	_ func(cslb.UpstreamConfig, string) cslb.UpstreamConfig                     = cslb.UpstreamConfig.ProxySSLName

	_ func(*cslb.Transport) error                                  = (*cslb.Transport).Err
	_ func(*cslb.Transport, *http.Request) (*http.Response, error) = (*cslb.Transport).RoundTrip
)

func TestExternalAPIStructFieldsAndMethods(t *testing.T) {
	peer := &cslb.Peer{
		Addr:        "127.0.0.1:8080",
		Weight:      3,
		Down:        false,
		MaxConns:    4,
		MaxFails:    2,
		FailTimeout: time.Second,
		Backup:      true,
	}
	group := &cslb.PeerGroup{
		Peers:       []*cslb.Peer{peer},
		TotalWeight: peer.Weight,
	}
	server := cslb.ServerConfig{
		Address:     peer.Addr,
		Weight:      peer.Weight,
		Down:        peer.Down,
		MaxConns:    peer.MaxConns,
		MaxFails:    peer.MaxFails,
		FailTimeout: peer.FailTimeout,
		Backup:      peer.Backup,
	}
	keyFunc := func(*http.Request) string { return "key" }
	upstream := cslb.UpstreamConfig{
		Pattern:   "http://service.local",
		Servers:   []cslb.ServerConfig{server},
		Algorithm: cslb.AlgoHash,
		HashFunc:  keyFunc,
		SSLName:   "service.local",
	}

	var (
		peerAddr        string        = peer.Addr
		peerWeight      int           = peer.Weight
		peerDown        bool          = peer.Down
		peerMaxConns    int           = peer.MaxConns
		peerMaxFails    int           = peer.MaxFails
		peerFailTimeout time.Duration = peer.FailTimeout
		peerBackup      bool          = peer.Backup

		groupPeers       []*cslb.Peer = group.Peers
		groupTotalWeight int          = group.TotalWeight

		serverAddress     string        = server.Address
		serverWeight      int           = server.Weight
		serverDown        bool          = server.Down
		serverMaxConns    int           = server.MaxConns
		serverMaxFails    int           = server.MaxFails
		serverFailTimeout time.Duration = server.FailTimeout
		serverBackup      bool          = server.Backup

		upstreamPattern   string                     = upstream.Pattern
		upstreamServers   []cslb.ServerConfig        = upstream.Servers
		upstreamAlgorithm cslb.AlgorithmType         = upstream.Algorithm
		upstreamHashFunc  func(*http.Request) string = upstream.HashFunc
		upstreamSSLName   string                     = upstream.SSLName
	)
	_ = []any{
		peerAddr, peerWeight, peerDown, peerMaxConns, peerMaxFails,
		peerFailTimeout, peerBackup, groupPeers, groupTotalWeight,
		serverAddress, serverWeight, serverDown, serverMaxConns,
		serverMaxFails, serverFailTimeout, serverBackup, upstreamPattern,
		upstreamServers, upstreamAlgorithm, upstreamHashFunc, upstreamSSLName,
	}

	if group.Peers[0] != peer || upstream.HashFunc(nil) != "key" {
		t.Fatal("exported fields are not usable from an external package")
	}

	roundRobin := cslb.NewRoundRobin([]*cslb.Peer{peer})
	if roundRobin.Primary() == nil || roundRobin.Backup() == nil {
		t.Fatal("round-robin group accessors did not preserve the backup group")
	}
	consistent := cslb.NewHashConsistent([]*cslb.Peer{
		{Addr: "127.0.0.1:8080", Weight: 1},
		{Addr: "127.0.0.1:8081", Weight: 1, Backup: true},
	})
	if consistent.Primary() == nil || consistent.Backup() == nil {
		t.Fatal("consistent-hash group accessors did not preserve the backup group")
	}

	ipPicker := cslb.NewIPHash([]*cslb.Peer{
		{Addr: "a", Weight: 1},
		{Addr: "b", Weight: 1},
	}).NewPickerForIP(net.ParseIP("192.0.2.1"))
	selected := ipPicker.Pick()
	if selected == nil {
		t.Fatal("NewPickerForIP returned an unusable picker")
	}
	ipPicker.Done(selected, false)

	hashPicker := cslb.NewHash([]*cslb.Peer{
		{Addr: "a", Weight: 1},
		{Addr: "b", Weight: 1},
	}).NewPickerForKey("key")
	selected = hashPicker.Pick()
	if selected == nil {
		t.Fatal("NewPickerForKey returned an unusable picker")
	}
	hashPicker.Done(selected, false)

	result := cslb.PickOne(cslb.NewRoundRobin([]*cslb.Peer{
		{Addr: "a", Weight: 1},
	}))
	if result == nil || result.Peer == nil {
		t.Fatal("PickOne returned no result")
	}
	var resultPeer *cslb.Peer = result.Peer
	if resultPeer != result.Peer {
		t.Fatal("PickResult.Peer is not externally usable")
	}
	result.Done(false)
}

func TestExternalAPIErrorsAndFailFastConstruction(t *testing.T) {
	if _, err := cslb.PickWithRetry(nil, func(string) error { return nil }); !errors.Is(err, cslb.ErrNilBalancer) {
		t.Fatalf("PickWithRetry error = %v, want ErrNilBalancer", err)
	}
	if _, err := cslb.PickWithRetry(cslb.NewRoundRobin(nil), nil); !errors.Is(err, cslb.ErrNilTryFunc) {
		t.Fatalf("PickWithRetry error = %v, want ErrNilTryFunc", err)
	}

	transport, err := cslb.NewTransportE(cslb.WithRoundTripper(nil))
	if !errors.Is(err, cslb.ErrNilRoundTripper) {
		t.Fatalf("NewTransportE error = %v, want ErrNilRoundTripper", err)
	}
	if transport == nil || !errors.Is(transport.Err(), cslb.ErrNilRoundTripper) {
		t.Fatalf("Transport.Err = %v, want ErrNilRoundTripper", transport.Err())
	}

	var nilTransport *cslb.Transport
	if !errors.Is(nilTransport.Err(), cslb.ErrNilRoundTripper) {
		t.Fatalf("nil Transport.Err = %v, want ErrNilRoundTripper", nilTransport.Err())
	}

	publicErrors := []error{
		cslb.ErrNoPeerAvailable,
		cslb.ErrNilBalancer,
		cslb.ErrNilTryFunc,
		cslb.ErrNilRequest,
		cslb.ErrNilRequestURL,
		cslb.ErrNilRoundTripper,
		cslb.ErrInvalidRoundTrip,
		cslb.ErrNilResponseBody,
		cslb.ErrInvalidUpstreamConfig,
	}
	for _, publicErr := range publicErrors {
		if publicErr == nil || publicErr.Error() == "" {
			t.Fatalf("invalid exported error: %v", publicErr)
		}
	}
}

func TestExternalAPIAlgorithmConstants(t *testing.T) {
	got := []cslb.AlgorithmType{
		cslb.AlgoRoundRobin,
		cslb.AlgoLeastConn,
		cslb.AlgoIPHash,
		cslb.AlgoHash,
		cslb.AlgoRandom,
		cslb.AlgoRandomTwo,
		cslb.AlgoHashConsistent,
	}
	for index, algorithm := range got {
		if int(algorithm) != index {
			t.Fatalf("algorithm constant %d = %d", index, algorithm)
		}
	}
}
