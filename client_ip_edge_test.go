package cslb

import (
	"net"
	"net/http"
	"testing"
)

func TestClientIP_SkipsMalformedHigherPrioritySources(t *testing.T) {
	tests := []struct {
		name       string
		realIP     string
		forwarded  string
		remoteAddr string
		want       string
	}{
		{
			name:      "trim real IP",
			realIP:    " 192.0.2.10 ",
			forwarded: "198.51.100.20",
			want:      "192.0.2.10",
		},
		{
			name:      "invalid real IP falls through to forwarded",
			realIP:    "invalid",
			forwarded: "198.51.100.20, 203.0.113.30",
			want:      "198.51.100.20",
		},
		{
			name:       "invalid headers fall through to remote address",
			realIP:     "invalid",
			forwarded:  "also-invalid",
			remoteAddr: "[2001:db8::5]:443",
			want:       "2001:db8::5",
		},
		{
			name:       "bare remote address",
			remoteAddr: "203.0.113.40",
			want:       "203.0.113.40",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := &http.Request{
				Header:     http.Header{},
				RemoteAddr: test.remoteAddr,
			}
			req.Header.Set("X-Real-IP", test.realIP)
			req.Header.Set("X-Forwarded-For", test.forwarded)

			got := clientIP(req)
			want := net.ParseIP(test.want)
			if !got.Equal(want) {
				t.Fatalf("clientIP = %v, want %v", got, want)
			}
		})
	}
}

func TestClientIP_InvalidSourcesReturnNil(t *testing.T) {
	req := &http.Request{
		Header: http.Header{
			"X-Real-Ip":       []string{"invalid"},
			"X-Forwarded-For": []string{"also-invalid"},
		},
		RemoteAddr: "not-an-address",
	}

	if got := clientIP(req); got != nil {
		t.Fatalf("clientIP = %v, want nil", got)
	}
}

func TestIPHash_InvalidIPFallsBackToRoundRobin(t *testing.T) {
	balancer := NewIPHash([]*Peer{
		{Addr: "A", Weight: 1},
		{Addr: "B", Weight: 1},
	})

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		picker := balancer.NewPickerForIP(net.IP{1, 2, 3})
		peer := picker.Pick()
		if peer == nil {
			t.Fatal("Pick returned nil")
		}
		counts[peer.Addr]++
		picker.Done(peer, false)
	}

	if counts["A"] != 2 || counts["B"] != 2 {
		t.Fatalf("round-robin fallback distribution = %v, want A=2 B=2", counts)
	}
}
