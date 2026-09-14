// Package api serves the AssemblyAI and OpenAI compatible endpoints and the internal worker queue.
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adapt2move/parakeet-api/internal/store"
)

const (
	generalBodyLimit  = 64 << 10
	completeBodyLimit = 16 << 20
	uploadOverhead    = 64 << 10 // multipart framing and fields beyond MaxUploadBytes
	maxTransfer       = time.Hour
)

// API is the HTTP handler.
type API struct {
	cfg    Settings
	store  *store.Store
	log    *slog.Logger
	mux    *http.ServeMux
	client *http.Client
	slots  chan struct{}
	poll   time.Duration // how often a synchronous /v1 request checks its job
}

// New returns the handler for a store.
func New(cfg Settings, st *store.Store, log *slog.Logger) *API {
	a := &API{cfg: cfg, store: st, log: log, mux: http.NewServeMux(), client: downloadClient(cfg.UploadIdle),
		slots: make(chan struct{}, cfg.UploadSlots), poll: 200 * time.Millisecond}
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /health/live":                   a.live,
		"GET /health/ready":                  a.ready,
		"GET /metrics":                       a.metrics,
		"POST /v2/upload":                    a.upload,
		"POST /v2/transcript":                a.submit,
		"GET /v2/transcript/{id}":            a.get,
		"DELETE /v2/transcript/{id}":         a.delete,
		"GET /v2/transcript/{id}/srt":        a.captions,
		"GET /v2/transcript/{id}/vtt":        a.captions,
		"POST /v1/audio/transcriptions":      a.transcriptions,
		"POST /internal/jobs/claim":          a.claim,
		"POST /internal/jobs/{id}/heartbeat": a.heartbeat,
		"GET /internal/jobs/{id}/audio":      a.audio,
		"POST /internal/jobs/{id}/complete":  a.complete,
	} {
		a.mux.HandleFunc(pattern, handler)
	}
	return a
}

// apiError is a client-safe failure. Its message never contains request data.
type apiError struct {
	status     int
	message    string
	retryAfter int // seconds; 0 omits the header
}

func (e *apiError) Error() string { return e.message }

func newError(status int, message string) *apiError {
	return &apiError{status: status, message: message}
}

var (
	errUnauthorized  = newError(http.StatusUnauthorized, "Unauthorized")
	errTooLarge      = newError(http.StatusRequestEntityTooLarge, "Request too large")
	errSlotsFull     = &apiError{status: http.StatusTooManyRequests, message: "Upload slots full", retryAfter: 5}
	errStalled       = newError(http.StatusRequestTimeout, "Upload stalled")
	errTooSlow       = newError(http.StatusRequestTimeout, "Upload too slow")
	errBadBody       = newError(http.StatusBadRequest, "Invalid request body")
	errNotFound      = newError(http.StatusNotFound, "Not Found")
	errMethod        = newError(http.StatusMethodNotAllowed, "Method Not Allowed")
	errInternal      = newError(http.StatusInternalServerError, "Internal Server Error")
	errInvalidFields = newError(http.StatusUnprocessableEntity, "Invalid or unsupported request fields")
)

// ServeHTTP authenticates and bounds every request before routing it.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if hasBody(r) {
		// Bound every body read, including the discard net/http runs for a body left unread.
		rc.SetReadDeadline(time.Now().Add(a.cfg.UploadIdle))
	}
	internal := strings.HasPrefix(r.URL.Path, "/internal/")
	if r.URL.Path != "/health/live" && r.URL.Path != "/health/ready" {
		key := a.cfg.APIKey
		if internal {
			key = a.cfg.WorkerKey
		}
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(given), []byte(key)) != 1 {
			a.fail(w, r, errUnauthorized)
			return
		}
	}
	limit := int64(generalBodyLimit)
	if isUpload(r) {
		limit = a.cfg.MaxUploadBytes + uploadOverhead
	} else if internal && strings.HasSuffix(r.URL.Path, "/complete") {
		limit = completeBodyLimit
	}
	if r.ContentLength > limit {
		a.fail(w, r, errTooLarge)
		return
	}
	if hasBody(r) {
		r.Body = &bodyReader{src: http.MaxBytesReader(w, r.Body, limit), rc: rc, idle: a.cfg.UploadIdle,
			pace: pace{start: time.Now(), idle: a.cfg.UploadIdle, rate: a.cfg.UploadRate}}
	}
	// The mux answers unknown paths with text and non-canonical paths with redirects.
	if _, pattern := a.mux.Handler(r); pattern == "" || path.Clean(r.URL.Path) != r.URL.Path {
		w = &unmatched{ResponseWriter: w, a: a, r: r}
	}
	a.mux.ServeHTTP(w, r)
}

func isUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/v2/upload" || r.URL.Path == "/v1/audio/transcriptions")
}

func hasBody(r *http.Request) bool {
	return r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0
}

// acquireSlot takes an upload slot. The returned release may be called more than once.
func (a *API) acquireSlot() (release func(), ok bool) {
	select {
	case a.slots <- struct{}{}:
		return sync.OnceFunc(func() { <-a.slots }), true
	default:
		return nil, false
	}
}

// unmatched turns the plain text 404 and 405 answers of the mux into JSON errors, and its
// redirects into 404 errors.
type unmatched struct {
	http.ResponseWriter
	a     *API
	r     *http.Request
	wrote bool
}

func (u *unmatched) WriteHeader(code int) {
	if u.wrote {
		return
	}
	u.wrote = true
	u.Header().Del("Location")
	if code == http.StatusMethodNotAllowed {
		u.a.fail(u.ResponseWriter, u.r, errMethod)
	} else {
		u.a.fail(u.ResponseWriter, u.r, errNotFound)
	}
}

func (u *unmatched) Write(p []byte) (int, error) {
	u.WriteHeader(http.StatusNotFound)
	return len(p), nil
}

// fail writes the error response. When the body was not read to its end, the connection is
// closed instead of waiting for a body the client may never finish.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	e := asAPIError(err)
	if e == errInternal {
		a.log.Error("request_failed", "method", r.Method, "route", r.Pattern, "error", logCause(err))
	}
	if b, ok := r.Body.(*bodyReader); hasBody(r) && !(ok && b.eof) {
		w.Header().Set("Connection", "close")
		http.NewResponseController(w).SetReadDeadline(time.Now())
	}
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	var body any = map[string]string{"error": e.message}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		body = map[string]any{"error": map[string]string{
			"message": e.message, "type": "invalid_request_error", "code": strconv.Itoa(e.status)}}
	}
	writeJSON(w, e.status, body)
}

func asAPIError(err error) *apiError {
	var ae *apiError
	var se *store.Error
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &se):
		return &apiError{status: se.Status, message: se.Message, retryAfter: se.RetryAfter}
	}
	return errInternal
}

// logCause describes an internal error without the paths and URLs error strings carry.
func logCause(err error) string {
	var pathErr *fs.PathError
	var urlErr *url.Error
	switch {
	case errors.As(err, &pathErr):
		return pathErr.Op + ": " + pathErr.Err.Error()
	case errors.As(err, &urlErr):
		return urlErr.Op + ": request failed"
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeText(w http.ResponseWriter, contentType, text string) {
	w.Header().Set("Content-Type", contentType)
	io.WriteString(w, text)
}

// decodeJSON reads exactly one JSON value into v, rejecting unknown fields.
func decodeJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return errInvalidFields
	}
	return nil
}

// pace fails transfers whose average rate stays below rate once the idle allowance is spent,
// so a client cannot hold a slot by trickling bytes, and transfers longer than an hour.
type pace struct {
	start time.Time
	idle  time.Duration
	rate  int64
	bytes int64
}

func (p *pace) add(n int) bool {
	p.bytes += int64(n)
	elapsed := time.Since(p.start)
	late := elapsed - p.idle
	return elapsed <= maxTransfer && (late <= 0 || float64(p.bytes) >= late.Seconds()*float64(p.rate))
}

// bodyReader enforces the body size limit, the per-read idle deadline and the pace. Its errors
// are *apiError values and stick.
type bodyReader struct {
	src  io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	pace pace
	err  error
	eof  bool
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.eof {
		return 0, io.EOF
	}
	b.rc.SetReadDeadline(time.Now().Add(b.idle))
	n, err := b.src.Read(p)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		b.err = errTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		b.err = errStalled
	case err != nil && err != io.EOF:
		b.err = errBadBody
	case !b.pace.add(n):
		b.err = errTooSlow
	case err == io.EOF:
		b.eof = true
		// The server now watches the connection for a disconnect; that read must not time out.
		b.rc.SetReadDeadline(time.Time{})
	}
	if b.err != nil {
		return 0, b.err
	}
	return n, err
}

func (b *bodyReader) Close() error { return b.src.Close() }
