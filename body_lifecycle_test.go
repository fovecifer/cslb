package cslb

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestTransport_GetBodyClosesOriginalBody(t *testing.T) {
	original := newTrackingBody("original")
	getBodyCalls := 0

	req, err := http.NewRequest(http.MethodPost, "http://getbody.local/upload", original)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len("payload"))
	req.GetBody = func() (io.ReadCloser, error) {
		getBodyCalls++
		return io.NopCloser(strings.NewReader("payload")), nil
	}

	var gotBody string
	transport := NewTransport(
		WithRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			gotBody = string(body)
			if err := req.Body.Close(); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       http.NoBody,
				Request:    req,
			}, nil
		})),
		WithUpstreams(
			Upstream("http://getbody.local", Server("http://127.0.0.1:8080")),
		),
	)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	resp.Body.Close()

	if gotBody != "payload" {
		t.Fatalf("attempt body = %q, want payload", gotBody)
	}
	if getBodyCalls != 1 {
		t.Fatalf("GetBody calls = %d, want 1", getBodyCalls)
	}
	if !original.closed {
		t.Fatal("original request body was not closed")
	}
}

func TestTransport_EarlyErrorsCloseRequestBody(t *testing.T) {
	initErr := errors.New("initialization failed")

	tests := []struct {
		name    string
		run     func(*http.Request) (*http.Response, error)
		wantErr error
	}{
		{
			name: "nil transport receiver",
			run: func(req *http.Request) (*http.Response, error) {
				var transport *Transport
				return transport.RoundTrip(req)
			},
			wantErr: ErrNilRoundTripper,
		},
		{
			name: "retained initialization error",
			run: func(req *http.Request) (*http.Response, error) {
				return (&Transport{initErr: initErr}).RoundTrip(req)
			},
			wantErr: initErr,
		},
		{
			name: "nil request URL",
			run: func(req *http.Request) (*http.Response, error) {
				req.URL = nil
				return NewTransport().RoundTrip(req)
			},
			wantErr: ErrNilRequestURL,
		},
		{
			name: "nil underlying transport",
			run: func(req *http.Request) (*http.Response, error) {
				return (&Transport{}).RoundTrip(req)
			},
			wantErr: ErrNilRoundTripper,
		},
		{
			name: "invalid upstream picker",
			run: func(req *http.Request) (*http.Response, error) {
				transport := NewTransport()
				transport.upstreams[upstreamKey{scheme: "http", host: "early-error.local"}] = &upstream{}
				return transport.RoundTrip(req)
			},
			wantErr: ErrInvalidUpstreamConfig,
		},
		{
			name: "invalid underlying RoundTripper result",
			run: func(req *http.Request) (*http.Response, error) {
				transport := NewTransport(
					WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
						return nil, nil
					})),
				)
				return transport.RoundTrip(req)
			},
			wantErr: ErrInvalidRoundTrip,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := newTrackingBody("payload")
			req, err := http.NewRequest(http.MethodPost, "http://early-error.local/upload", body)
			if err != nil {
				t.Fatal(err)
			}

			_, err = test.run(req)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("RoundTrip error = %v, want %v", err, test.wantErr)
			}
			if !body.closed {
				t.Fatal("request body was not closed")
			}
		})
	}
}

func TestPrepareBody_BuffersInMemoryAndReplays(t *testing.T) {
	original := newTrackingBody("payload")
	req := &http.Request{Body: original, ContentLength: -1}
	transport := NewTransport(WithMaxBodyBuffer(64))

	getBody, cleanup, err := transport.prepareBody(req)
	if err != nil {
		t.Fatalf("prepare body: %v", err)
	}
	if cleanup != nil {
		t.Fatal("in-memory buffering unexpectedly returned cleanup")
	}
	if !original.closed {
		t.Fatal("original request body was not closed")
	}
	if req.ContentLength != int64(len("payload")) {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, len("payload"))
	}

	assertBodyReplay(t, getBody, "payload")
	assertBodyReplay(t, getBody, "payload")
}

func TestPrepareBody_SpillsToFileAndCleansUp(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	original := newTrackingBody("payload larger than limit")
	req := &http.Request{Body: original, ContentLength: -1}
	transport := NewTransport(WithMaxBodyBuffer(4))

	getBody, cleanup, err := transport.prepareBody(req)
	if err != nil {
		t.Fatalf("prepare body: %v", err)
	}
	if cleanup == nil {
		t.Fatal("file-backed buffering did not return cleanup")
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			cleanup()
		}
	})

	if !original.closed {
		t.Fatal("original request body was not closed")
	}
	if req.ContentLength != int64(len("payload larger than limit")) {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, len("payload larger than limit"))
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temp directory: %v", err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "cslb-body-") {
		t.Fatalf("temporary files = %v, want one cslb-body-* file", entries)
	}

	assertBodyReplay(t, getBody, "payload larger than limit")
	assertBodyReplay(t, getBody, "payload larger than limit")

	cleanup()
	cleaned = true
	entries, err = os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temp directory after cleanup: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files after cleanup = %v, want none", entries)
	}
}

func TestPrepareBody_ReadErrorClosesOriginalBody(t *testing.T) {
	wantErr := errors.New("read failed")
	original := &failingBody{err: wantErr}
	req := &http.Request{Body: original}

	_, _, err := NewTransport().prepareBody(req)
	if !errors.Is(err, wantErr) {
		t.Fatalf("prepare body error = %v, want %v", err, wantErr)
	}
	if !original.closed {
		t.Fatal("original request body was not closed after read failure")
	}
}

type failingBody struct {
	err    error
	closed bool
}

func (b *failingBody) Read([]byte) (int, error) {
	return 0, b.err
}

func (b *failingBody) Close() error {
	b.closed = true
	return nil
}

func assertBodyReplay(t *testing.T, getBody func() (io.ReadCloser, error), want string) {
	t.Helper()
	body, err := getBody()
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		body.Close()
		t.Fatalf("read replayed body: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close replayed body: %v", err)
	}
	if string(got) != want {
		t.Fatalf("replayed body = %q, want %q", got, want)
	}
}
