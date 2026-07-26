package cslb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
)

func BenchmarkBalancerPickDone(b *testing.B) {
	for _, peerCount := range []int{3, 32} {
		peerCount := peerCount
		b.Run(fmt.Sprintf("peers_%d", peerCount), func(b *testing.B) {
			for name, newPickerFactory := range algorithmPickerFactories() {
				newPickerFactory := newPickerFactory
				b.Run(name, func(b *testing.B) {
					peers := benchmarkPeers(peerCount)
					newPicker := newPickerFactory(peers)
					b.ReportAllocs()
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						picker := newPicker(iteration)
						peer := picker.Pick()
						if peer == nil {
							b.Fatal("Pick returned nil")
						}
						picker.Done(peer, false)
					}
				})
			}
		})
	}
}

func BenchmarkHashConsistentBuild(b *testing.B) {
	for _, peerCount := range []int{3, 32} {
		peerCount := peerCount
		b.Run(strconv.Itoa(peerCount), func(b *testing.B) {
			peers := benchmarkPeers(peerCount)
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				if balancer := NewHashConsistent(peers); len(balancer.points) == 0 {
					b.Fatal("consistent-hash ring is empty")
				}
			}
		})
	}
}

func BenchmarkTransportRoundTrip(b *testing.B) {
	success := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})
	b.Run("single_peer_success", func(b *testing.B) {
		transport := NewTransport(
			WithRoundTripper(success),
			WithUpstreams(Upstream("http://benchmark.local",
				Server("http://127.0.0.1:8080"),
			)),
		)
		req, err := http.NewRequest(http.MethodGet, "http://benchmark.local/path", nil)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			resp, err := transport.RoundTrip(req)
			if err != nil {
				b.Fatal(err)
			}
			resp.Body.Close()
		}
	})

	b.Run("two_peer_retry", func(b *testing.B) {
		calls := 0
		retryThenSucceed := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls%2 == 1 {
				return nil, errors.New("retryable failure")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})
		transport := NewTransport(
			WithRoundTripper(retryThenSucceed),
			WithUpstreams(Upstream("http://benchmark.local",
				Server("http://127.0.0.1:8080", MaxFails(0)),
				Server("http://127.0.0.1:8081", MaxFails(0)),
			)),
		)
		req, err := http.NewRequest(http.MethodGet, "http://benchmark.local/path", nil)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			resp, err := transport.RoundTrip(req)
			if err != nil {
				b.Fatal(err)
			}
			resp.Body.Close()
		}
	})
}

func BenchmarkPrepareBodyReplay(b *testing.B) {
	const payloadSize = 64 << 10
	payload := bytes.Repeat([]byte("x"), payloadSize)

	b.Run("memory", func(b *testing.B) {
		transport := NewTransport(WithMaxBodyBuffer(payloadSize))
		benchmarkPrepareBody(b, transport, payload)
	})
	b.Run("spill", func(b *testing.B) {
		tempDir := b.TempDir()
		b.Setenv("TMPDIR", tempDir)
		b.Setenv("TMP", tempDir)
		b.Setenv("TEMP", tempDir)
		transport := NewTransport(WithMaxBodyBuffer(1024))
		benchmarkPrepareBody(b, transport, payload)
	})
}

func benchmarkPrepareBody(b *testing.B, transport *Transport, payload []byte) {
	b.Helper()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		req := &http.Request{
			Body:          io.NopCloser(bytes.NewReader(payload)),
			ContentLength: int64(len(payload)),
		}
		getBody, cleanup, err := transport.prepareBody(req, true)
		if err != nil {
			b.Fatal(err)
		}
		body, err := getBody()
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			b.Fatal(err)
		}
		read, err := io.Copy(io.Discard, body)
		body.Close()
		if cleanup != nil {
			cleanup()
		}
		if err != nil || read != int64(len(payload)) {
			b.Fatalf("body replay = (%d, %v), want (%d, nil)", read, err, len(payload))
		}
	}
}

func benchmarkPeers(count int) []*Peer {
	peers := make([]*Peer, count)
	for index := range peers {
		peers[index] = &Peer{
			Addr:     fmt.Sprintf("10.0.0.%d:8080", index+1),
			Weight:   index%5 + 1,
			MaxFails: 0,
			explicit: serverFieldMaxFails,
		}
	}
	return peers
}
