package cslb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlgorithms_ConcurrentFailureRecoveryStateMachine(t *testing.T) {
	for name, newPickerFactory := range algorithmPickerFactories() {
		t.Run(name, func(t *testing.T) {
			peers := []*Peer{
				{
					Addr:     "A",
					Weight:   5,
					MaxFails: 0,
					explicit: serverFieldMaxFails,
				},
				{
					Addr:     "B",
					Weight:   3,
					MaxFails: 0,
					explicit: serverFieldMaxFails,
				},
				{
					Addr:     "C",
					Weight:   2,
					MaxFails: 0,
					explicit: serverFieldMaxFails,
				},
			}
			newPicker := newPickerFactory(peers)

			const goroutines = 12
			const eventsPerGoroutine = 200
			errCh := make(chan error, goroutines)
			var wg sync.WaitGroup
			for worker := 0; worker < goroutines; worker++ {
				worker := worker
				wg.Add(1)
				go func() {
					defer wg.Done()
					for event := 0; event < eventsPerGoroutine; event++ {
						key := worker*eventsPerGoroutine + event
						picker := newPicker(key)
						peer := picker.Pick()
						if peer == nil {
							errCh <- errors.New("initial Pick returned nil")
							return
						}

						switch key % 3 {
						case 0:
							picker.Done(peer, false)
						case 1:
							picker.Done(peer, true)
							retry := picker.Pick()
							if retry == nil {
								errCh <- errors.New("retry Pick returned nil")
								return
							}
							if retry == peer {
								errCh <- errors.New("retry selected an already-tried peer")
								return
							}
							picker.Done(retry, false)
						case 2:
							doneNeutral(picker, peer)
						}
					}
				}()
			}
			wg.Wait()
			close(errCh)

			for err := range errCh {
				t.Error(err)
			}
			for _, peer := range peers {
				if peer.conns != 0 {
					t.Errorf("%s conns = %d, want 0", peer.Addr, peer.conns)
				}
				if peer.fails < 0 {
					t.Errorf("%s failures = %d, want non-negative", peer.Addr, peer.fails)
				}
				if peer.effectiveWeight < 0 || peer.effectiveWeight > peer.Weight {
					t.Errorf("%s effective weight = %d, want [0,%d]",
						peer.Addr, peer.effectiveWeight, peer.Weight)
				}
			}
		})
	}
}

func TestTransport_RepeatedTimeoutsReleaseAttempts(t *testing.T) {
	var active atomic.Int64
	transport := NewTransport(
		WithTimeout(time.Millisecond),
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			active.Add(1)
			defer active.Add(-1)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})),
		WithUpstreams(Upstream("http://timeout-stress.local",
			Server("http://127.0.0.1:8080",
				MaxFails(0),
			),
		)),
	)

	for iteration := 0; iteration < 100; iteration++ {
		req, err := http.NewRequest(http.MethodGet, "http://timeout-stress.local/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: error = %v, want context deadline exceeded", iteration, err)
		}
		if got := active.Load(); got != 0 {
			t.Fatalf("iteration %d: active attempts = %d, want 0", iteration, got)
		}
	}

	rr := transport.upstreams[upstreamKey{
		scheme: "http",
		host:   "timeout-stress.local",
	}].balancer.(*RoundRobin)
	if got := rr.primary.Peers[0].conns; got != 0 {
		t.Fatalf("peer conns = %d, want 0", got)
	}
}

func TestTransport_RepeatedSpillRetriesLeaveNoResources(t *testing.T) {
	tempDir := t.TempDir()
	setProcessTempDir(t, tempDir)

	var calls atomic.Int64
	transport := NewTransport(
		WithMaxBodyBuffer(0),
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			_, readErr := io.ReadAll(req.Body)
			closeErr := req.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if calls.Add(1)%2 == 1 {
				return nil, errors.New("retryable transport failure")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})),
		WithUpstreams(Upstream("http://spill-stress.local",
			Server("http://127.0.0.1:8080", MaxFails(0)),
			Server("http://127.0.0.1:8081", MaxFails(0)),
		)),
	)

	for iteration := 0; iteration < 50; iteration++ {
		body := newTrackingBody("payload")
		req, err := http.NewRequest(http.MethodPut, "http://spill-stress.local/test", body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("iteration %d: RoundTrip: %v", iteration, err)
		}
		resp.Body.Close()
		if !body.closed {
			t.Fatalf("iteration %d: original body was not closed", iteration)
		}
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files after stress test = %v, want none", entries)
	}
	for _, peer := range transport.upstreams[upstreamKey{
		scheme: "http",
		host:   "spill-stress.local",
	}].balancer.(*RoundRobin).primary.Peers {
		if peer.conns != 0 {
			t.Errorf("%s conns = %d, want 0", peer.Addr, peer.conns)
		}
	}
}

func setProcessTempDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
}
