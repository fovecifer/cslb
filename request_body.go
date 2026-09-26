package cslb

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

// errRequestBodyReleased is returned when an attempt asks for the request body
// after its resources have been released.
var errRequestBodyReleased = errors.New("cslb: request body already released")

// requestBody hands each attempt its own view of a request body and releases
// the body's resources once nothing uses them any more.
//
// A RoundTripper may keep reading a request body after RoundTrip returns, for
// example to finish a full-duplex upload after the response headers arrived,
// and closes the body when it is done. The resources therefore live until
// RoundTrip has finished and every attempt body has been closed, not merely
// until RoundTrip returns.
type requestBody struct {
	open   func() (io.ReadCloser, error) // yields the complete body; nil when it cannot be replayed
	stream io.Reader                     // unread body shared by attempts when open is nil

	mu       sync.Mutex
	refs     int // RoundTrip's own reference plus one per open attempt body
	released bool
	cleanup  func()
}

func newRequestBody(open func() (io.ReadCloser, error), stream io.Reader, cleanup func()) *requestBody {
	return &requestBody{open: open, stream: stream, refs: 1, cleanup: cleanup}
}

// empty reports whether there is no body to hand to attempts.
func (b *requestBody) empty() bool {
	return b.open == nil && b.stream == nil
}

// getBody returns a function suitable for Request.GetBody, or nil when the body
// cannot be replayed.
func (b *requestBody) getBody() func() (io.ReadCloser, error) {
	if b.open == nil {
		return nil
	}
	return b.next
}

// next returns the body for one attempt. Whoever receives it, normally the
// underlying RoundTripper, must close it.
func (b *requestBody) next() (io.ReadCloser, error) {
	if !b.acquire() {
		return nil, errRequestBodyReleased
	}
	if b.open == nil {
		// Not replayable: attempts share the unread stream. A later attempt only
		// reads it after an earlier one failed before sending any bytes. The
		// stream itself is closed by cleanup, not by an attempt.
		return &attemptBody{ReadCloser: io.NopCloser(b.stream), owner: b}, nil
	}
	rc, err := b.open()
	if err != nil {
		b.release()
		return nil, err
	}
	if rc == nil || rc == http.NoBody {
		// Nothing to track, and net/http recognizes http.NoBody only by
		// identity when it frames an empty body.
		b.release()
		return rc, nil
	}
	return &attemptBody{ReadCloser: rc, owner: b}, nil
}

func (b *requestBody) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released {
		return false
	}
	b.refs++
	return true
}

// release drops one reference and runs cleanup when the last one is gone.
func (b *requestBody) release() {
	b.mu.Lock()
	b.refs--
	run := b.refs == 0 && !b.released
	if run {
		b.released = true
	}
	b.mu.Unlock()
	if run {
		b.runCleanup()
	}
}

// releaseNow runs cleanup immediately, even while attempt bodies are still
// open. RoundTrip uses it when it returns no response: every attempt has then
// failed, so an upload still in progress can only belong to a failed attempt.
func (b *requestBody) releaseNow() {
	b.mu.Lock()
	run := !b.released
	b.released = true
	b.mu.Unlock()
	if run {
		b.runCleanup()
	}
}

func (b *requestBody) runCleanup() {
	if b.cleanup != nil {
		b.cleanup()
	}
}

// attemptBody is the body of one attempt. Closing it releases that attempt's
// reference on the shared request body.
type attemptBody struct {
	io.ReadCloser
	owner *requestBody
	once  sync.Once
	err   error
}

func (a *attemptBody) Close() error {
	a.once.Do(func() {
		a.err = a.ReadCloser.Close()
		a.owner.release()
	})
	return a.err
}

// replaySource reads a seekable body at explicit offsets, so every reader of it
// keeps its own position. A RoundTripper may still be reading a superseded
// attempt's body while the next attempt reads from the start; sharing a single
// file offset would interleave the two readers and corrupt both bodies.
type replaySource struct {
	mu sync.Mutex
	rs io.ReadSeeker
}

// reader returns a reader over the byte range [start, end); a negative end
// reads to EOF.
func (s *replaySource) reader(start, end int64) io.ReadCloser {
	return io.NopCloser(&offsetReader{src: s, off: start, end: end})
}

func (s *replaySource) readAt(p []byte, off int64) (int, error) {
	if ra, ok := s.rs.(io.ReaderAt); ok {
		return ra.ReadAt(p, off)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return s.rs.Read(p)
}

type offsetReader struct {
	src *replaySource
	off int64
	end int64 // negative: read to EOF
}

func (r *offsetReader) Read(p []byte) (int, error) {
	if r.end >= 0 {
		if r.off >= r.end {
			return 0, io.EOF
		}
		p = p[:min(int64(len(p)), r.end-r.off)]
	}
	n, err := r.src.readAt(p, r.off)
	r.off += int64(n)
	return n, err
}
