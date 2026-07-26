package cslb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func FuzzNewTransportEConfiguration(f *testing.F) {
	f.Add("http://service.local", "http://127.0.0.1:8080", int8(1), uint8(0))
	f.Add("https://service.local", "127.0.0.1:8443", int8(5), uint8(4))
	f.Add("invalid", "://bad", int8(-1), uint8(6))
	f.Add("", "", int8(0), uint8(2))

	f.Fuzz(func(t *testing.T, pattern, backend string, rawWeight int8, algorithm uint8) {
		if len(pattern) > 256 || len(backend) > 256 {
			t.Skip()
		}

		weight := int(rawWeight % 9)
		upstream := Upstream(pattern, Server(backend, Weight(weight)))
		switch algorithm % 7 {
		case 0:
			upstream = upstream.RoundRobin()
		case 1:
			upstream = upstream.LeastConn()
		case 2:
			upstream = upstream.IPHash()
		case 3:
			upstream = upstream.Hash(func(*http.Request) string { return "key" })
		case 4:
			upstream = upstream.Random()
		case 5:
			upstream = upstream.RandomTwo()
		case 6:
			upstream = upstream.HashConsistent(func(*http.Request) string { return "key" })
		}

		transport, err := NewTransportE(WithUpstreams(upstream))
		if transport == nil {
			t.Fatal("NewTransportE returned a nil transport")
		}
		if (err == nil) != (transport.Err() == nil) {
			t.Fatalf("constructor error = %v, Transport.Err = %v", err, transport.Err())
		}
	})
}

func FuzzTransportBodyReplay(f *testing.F) {
	f.Add([]byte("payload"), uint8(0))
	f.Add([]byte{}, uint8(32))
	f.Add(bytes.Repeat([]byte("x"), 65), uint8(64))

	f.Fuzz(func(t *testing.T, payload []byte, rawLimit uint8) {
		if len(payload) > 1024 {
			t.Skip()
		}

		var attempts [][]byte
		transport := NewTransport(
			WithMaxBodyBuffer(int64(rawLimit%65)),
			WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				closeErr := req.Body.Close()
				if err != nil {
					return nil, err
				}
				if closeErr != nil {
					return nil, closeErr
				}
				attempts = append(attempts, append([]byte(nil), body...))
				if len(attempts) == 1 {
					return nil, errors.New("retryable transport failure")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
					Request:    req,
				}, nil
			})),
			WithUpstreams(Upstream("http://body-fuzz.local",
				Server("http://127.0.0.1:8080"),
				Server("http://127.0.0.1:8081"),
			)),
		)

		original := newTrackingBody(string(payload))
		req, err := http.NewRequest(http.MethodPut, "http://body-fuzz.local/test", original)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		resp.Body.Close()

		if len(attempts) != 2 {
			t.Fatalf("attempts = %d, want 2", len(attempts))
		}
		for index, body := range attempts {
			if !bytes.Equal(body, payload) {
				t.Fatalf("attempt %d body = %q, want %q", index, body, payload)
			}
		}
		if !original.closed {
			t.Fatal("original body was not closed")
		}
	})
}

func FuzzBalancerStateMachine(f *testing.F) {
	f.Add(uint8(0), []byte{1, 3, 5}, []byte{0, 1, 2, 1})
	f.Add(uint8(4), []byte{5, 1, 2}, []byte{2, 2, 0, 1})
	f.Add(uint8(6), []byte{2, 2, 2}, []byte{1, 0, 1, 2})

	f.Fuzz(func(t *testing.T, algorithm uint8, rawWeights, events []byte) {
		if len(rawWeights) > 16 || len(events) > 128 {
			t.Skip()
		}

		peers := make([]*Peer, 3)
		for index := range peers {
			weight := 1
			if index < len(rawWeights) {
				weight += int(rawWeights[index] % 8)
			}
			peers[index] = &Peer{
				Addr:     fmt.Sprintf("peer-%d", index),
				Weight:   weight,
				MaxFails: 0,
				explicit: serverFieldMaxFails,
			}
		}
		newPicker := fuzzPickerFactory(algorithm%7, peers)

		for index, event := range events {
			picker := newPicker(index)
			peer := picker.Pick()
			if peer == nil {
				t.Fatalf("event %d: Pick returned nil", index)
			}
			if !containsPeer(peers, peer) {
				t.Fatalf("event %d: selected unknown peer %p", index, peer)
			}
			if peer.conns != 1 {
				t.Fatalf("event %d: selected peer conns = %d, want 1", index, peer.conns)
			}

			switch event % 3 {
			case 0:
				picker.Done(peer, false)
			case 1:
				picker.Done(peer, true)
				retry := picker.Pick()
				if retry == nil {
					t.Fatalf("event %d: retry Pick returned nil", index)
				}
				if retry == peer {
					t.Fatalf("event %d: retry selected the already-tried peer", index)
				}
				picker.Done(retry, false)
			case 2:
				doneNeutral(picker, peer)
			}
		}

		for _, peer := range peers {
			if peer.conns != 0 {
				t.Fatalf("%s conns = %d, want 0", peer.Addr, peer.conns)
			}
			if peer.effectiveWeight < 0 || peer.effectiveWeight > peer.Weight {
				t.Fatalf("%s effective weight = %d, want [0,%d]", peer.Addr, peer.effectiveWeight, peer.Weight)
			}
		}
	})
}

func FuzzClientIP(f *testing.F) {
	f.Add("192.0.2.1", "198.51.100.2, 203.0.113.3", "[2001:db8::1]:443")
	f.Add("invalid", " 2001:db8::2 ", "203.0.113.4:80")
	f.Add("", "", "not-an-address")

	f.Fuzz(func(t *testing.T, realIP, forwarded, remoteAddr string) {
		if len(realIP) > 128 || len(forwarded) > 256 || len(remoteAddr) > 256 {
			t.Skip()
		}

		req := &http.Request{
			Header:     make(http.Header),
			RemoteAddr: remoteAddr,
		}
		req.Header.Set("X-Real-IP", realIP)
		req.Header.Set("X-Forwarded-For", forwarded)

		got := clientIP(req)
		want := referenceClientIP(realIP, forwarded, remoteAddr)
		if (got == nil) != (want == nil) || got != nil && !got.Equal(want) {
			t.Fatalf("clientIP = %v, want %v", got, want)
		}
	})
}

func fuzzPickerFactory(algorithm uint8, peers []*Peer) func(int) Picker {
	switch algorithm {
	case 0:
		balancer := NewRoundRobin(peers)
		return func(int) Picker { return balancer.NewPicker() }
	case 1:
		balancer := NewLeastConn(peers)
		return func(int) Picker { return balancer.NewPicker() }
	case 2:
		balancer := NewIPHash(peers)
		return func(key int) Picker {
			return balancer.NewPickerForIP(net.IPv4(10, 0, byte(key), 1))
		}
	case 3:
		balancer := NewHash(peers)
		return func(key int) Picker {
			return balancer.NewPickerForKey(fmt.Sprintf("key-%d", key))
		}
	case 4:
		balancer := NewRandom(peers)
		return func(int) Picker { return balancer.NewPicker() }
	case 5:
		balancer := NewRandomTwo(peers)
		return func(int) Picker { return balancer.NewPicker() }
	default:
		balancer := NewHashConsistent(peers)
		return func(key int) Picker {
			return balancer.NewPickerForKey(fmt.Sprintf("key-%d", key))
		}
	}
}

func containsPeer(peers []*Peer, target *Peer) bool {
	for _, peer := range peers {
		if peer == target {
			return true
		}
	}
	return false
}

func referenceClientIP(realIP, forwarded, remoteAddr string) net.IP {
	if ip := net.ParseIP(strings.TrimSpace(realIP)); ip != nil {
		return ip
	}
	if separator := strings.IndexByte(forwarded, ','); separator >= 0 {
		forwarded = forwarded[:separator]
	}
	if ip := net.ParseIP(strings.TrimSpace(forwarded)); ip != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(strings.TrimSpace(host))
}
