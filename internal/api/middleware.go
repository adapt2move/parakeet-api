package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	generalBodyLimit  = 65536
	completeBodyLimit = 16 * 1024 * 1024
	// uploadOverhead is the room for multipart framing and fields beyond MaxUploadBytes.
	uploadOverhead = 65536
	// transferLimit bounds one audio transfer, whatever its pace.
	transferLimit = time.Hour
)

type stateKey struct{}

// requestState is what the guard shares with the handlers of one request.
type requestState struct {
	body        *bodyReader // nil when the request has no body
	releaseOnce sync.Once
	release     func()
}

func state(r *http.Request) *requestState {
	s, _ := r.Context().Value(stateKey{}).(*requestState)
	if s == nil {
		return &requestState{}
	}
	return s
}

// releaseSlot frees the upload slot of the request early, once its audio is stored.
func releaseSlot(r *http.Request) {
	s := state(r)
	if s.release != nil {
		s.releaseOnce.Do(s.release)
	}
}

// bodyConsumed reports whether the request body was read to its end.
func bodyConsumed(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	s := state(r)
	return s.body != nil && s.body.eof
}

// fail writes an error response. When the body was not read to its end, the connection is
// closed instead of draining a body the client may never finish sending.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	e := asAPIError(err)
	if e == errInternal {
		a.log.Error("request_failed", "method", r.Method, "route", routeName(r), "error", err.Error())
	}
	if !bodyConsumed(r) {
		w.Header().Set("Connection", "close")
	}
	writeError(w, r, e)
}

// ServeHTTP authenticates and bounds every request before routing it.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/health/live" || path == "/health/ready" {
		a.route(w, r)
		return
	}
	expected := a.cfg.APIKey
	if strings.HasPrefix(path, "/internal/") {
		expected = a.cfg.WorkerKey
	}
	key := strings.TrimPrefix(lastHeader(r.Header, "Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(key), []byte(expected)) != 1 {
		a.fail(w, r, errUnauthorized)
		return
	}
	upload := r.Method == http.MethodPost && (path == "/v2/upload" || path == "/v1/audio/transcriptions")
	limit := int64(generalBodyLimit)
	switch {
	case upload:
		limit = a.cfg.MaxUploadBytes + uploadOverhead
	case strings.HasSuffix(path, "/complete"):
		limit = completeBodyLimit
	}
	if r.ContentLength > limit {
		a.fail(w, r, errRequestTooLarge)
		return
	}
	st := &requestState{}
	if upload {
		if !a.acquireSlot() {
			a.fail(w, r, errSlotsFull)
			return
		}
		st.release = a.releaseSlot
		defer st.releaseOnce.Do(st.release)
	}

	deadline := a.now().Add(a.cfg.SyncTimeout + transferLimit)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	rc := http.NewResponseController(w)
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		st.body = &bodyReader{
			src:      http.MaxBytesReader(w, r.Body, limit),
			rc:       rc,
			idle:     a.cfg.UploadIdle,
			deadline: deadline,
			pace:     pace{start: a.now(), idle: a.cfg.UploadIdle, rate: a.cfg.UploadRate},
			now:      a.now,
		}
	}
	inner := r.WithContext(context.WithValue(ctx, stateKey{}, st))
	if st.body != nil {
		inner.Body = st.body
	}
	gw := &guardWriter{ResponseWriter: w, deadline: deadline, now: a.now}
	a.route(gw, inner)
	if st.body != nil && !st.body.eof {
		// Never leave a deadline behind for the server's own reads of this connection.
		rc.SetReadDeadline(time.Time{})
	}
	if gw.timedOut() {
		h := w.Header()
		h.Del("Retry-After")
		a.fail(w, inner, errRequestTimeout)
	}
}

func lastHeader(h http.Header, name string) string {
	values := h.Values(name)
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func (a *API) acquireSlot() bool {
	a.slotsMu.Lock()
	defer a.slotsMu.Unlock()
	if a.uploads >= a.cfg.UploadSlots {
		return false
	}
	a.uploads++
	return true
}

func (a *API) releaseSlot() {
	a.slotsMu.Lock()
	defer a.slotsMu.Unlock()
	a.uploads--
}

// guardWriter drops everything a handler writes once the overall request deadline passed
// before a response started, so that the guard can answer 504 instead.
type guardWriter struct {
	http.ResponseWriter
	deadline time.Time
	now      func() time.Time

	started bool
	expired bool
}

func (g *guardWriter) WriteHeader(code int) {
	if g.started || g.expired {
		return
	}
	if !g.now().Before(g.deadline) {
		g.expired = true
		return
	}
	g.started = true
	g.ResponseWriter.WriteHeader(code)
}

func (g *guardWriter) Write(p []byte) (int, error) {
	if !g.started && !g.expired {
		g.WriteHeader(http.StatusOK)
	}
	if g.expired {
		return len(p), nil
	}
	return g.ResponseWriter.Write(p)
}

func (g *guardWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *guardWriter) timedOut() bool {
	return g.expired || (!g.started && !g.now().Before(g.deadline))
}

// pace rejects transfers whose average rate stays too low once the idle allowance is spent.
// An idle timeout alone lets a client hold an upload slot by trickling one byte at a time.
type pace struct {
	start time.Time
	idle  time.Duration
	rate  float64
	bytes int64
}

func (p *pace) add(n int, now time.Time) bool {
	p.bytes += int64(n)
	late := now.Sub(p.start) - p.idle
	return late <= 0 || float64(p.bytes) >= late.Seconds()*p.rate
}

// bodyReader enforces the body size limit, a per-read idle timeout and the minimum average
// rate. Its errors are *apiError values and stick.
type bodyReader struct {
	src      io.ReadCloser
	rc       *http.ResponseController
	idle     time.Duration
	deadline time.Time
	pace     pace
	now      func() time.Time

	err error
	eof bool
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.eof {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	readDeadline := b.now().Add(b.idle)
	if b.deadline.Before(readDeadline) {
		readDeadline = b.deadline
	}
	b.rc.SetReadDeadline(readDeadline)
	n, err := b.src.Read(p)
	var tooLarge *http.MaxBytesError
	var netErr net.Error
	switch {
	case err == nil || err == io.EOF:
	case errors.As(err, &tooLarge):
		b.err = errBodyTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		b.err = errStalled
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr):
		b.err = errDisconnected
	default:
		b.err = errBadBody
	}
	if b.err != nil {
		return 0, b.err
	}
	if !b.pace.add(n, b.now()) {
		b.err = errTooSlow
		return 0, b.err
	}
	if err == io.EOF {
		b.eof = true
		// The server starts watching the connection for a disconnect now; that read must not
		// inherit the idle deadline.
		b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (b *bodyReader) Close() error { return b.src.Close() }
