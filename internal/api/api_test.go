package api

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adapt2move/parakeet-api/internal/store"
)

func newServer(t *testing.T, change func(*Settings)) (*API, *httptest.Server) {
	t.Helper()
	cfg := Settings{DataDir: t.TempDir(), APIKey: clientKey, WorkerKey: workerKey, PublicURL: "http://api.test",
		MaxUploadBytes: 4096, MaxStorageBytes: 1 << 20, MaxPendingJobs: 8, Retention: time.Hour, MaxJobAge: time.Hour,
		Lease: 15 * time.Second, MaxAttempts: 3, SyncTimeout: 5 * time.Second, UploadSlots: 1,
		UploadIdle: 300 * time.Millisecond, UploadRate: 1000, AudioURLHosts: []string{"audio.example"}}
	if change != nil {
		change(&cfg)
	}
	st, err := store.New(cfg.StoreConfig())
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg, st, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, srv
}

type reply struct {
	status  int
	header  http.Header
	body    string
	elapsed time.Duration
}

func (r reply) message(t *testing.T) string {
	t.Helper()
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	var message string
	if json.Unmarshal([]byte(r.body), &body) != nil || json.Unmarshal(body.Error, &message) != nil {
		t.Fatalf("not a plain JSON error: %d %q", r.status, r.body)
	}
	return message
}

func do(t *testing.T, srv *httptest.Server, method, path, key string, body io.Reader) reply {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, body)
	if key != "" {
		req.Header.Set("Authorization", key)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return reply{status: res.StatusCode, header: res.Header, body: string(data)}
}

// raw sends a request head, then each body chunk after its delay, and reads the response. It
// stops sending once the server answers.
func raw(t *testing.T, srv *httptest.Server, head string, chunks ...any) reply {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	io.WriteString(conn, strings.ReplaceAll(head, "\n", "\r\n")+"\r\n")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		for _, chunk := range chunks {
			if delay, ok := chunk.(time.Duration); ok {
				select {
				case <-answered:
				case <-time.After(delay):
				}
				continue
			}
			io.WriteString(conn, chunk.(string))
		}
	}()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	return reply{status: res.StatusCode, header: res.Header, body: string(data), elapsed: time.Since(start)}
}

func TestAuthentication(t *testing.T) {
	_, srv := newServer(t, nil)
	for _, tc := range []struct {
		method, path, key string
		status            int
	}{
		{"GET", "/health/live", "", 200},
		{"GET", "/health/ready", "wrong", 200},
		{"GET", "/metrics", clientKey, 200},
		{"GET", "/metrics", "Bearer " + clientKey, 200},
		{"GET", "/metrics", "", 401},
		{"GET", "/metrics", workerKey, 401},
		{"GET", "/metrics", "Bearer  " + clientKey, 401},
		{"GET", "/metrics", "Basic " + clientKey, 401},
		{"GET", "/health/live/", "", 401},
		{"POST", "/internal/jobs/claim", "Bearer " + workerKey, 204},
		{"POST", "/internal/jobs/claim", clientKey, 401},
		{"GET", "/nothing", "", 401},
		{"GET", "/internal/nothing", clientKey, 401},
	} {
		res := do(t, srv, tc.method, tc.path, tc.key, nil)
		if res.status != tc.status {
			t.Errorf("%s %s with %q: %d", tc.method, tc.path, tc.key, res.status)
		}
		if tc.status == 401 && res.message(t) != "Unauthorized" {
			t.Errorf("%s %s: %s", tc.method, tc.path, res.body)
		}
	}
}

func TestUnmatchedRequests(t *testing.T) {
	_, srv := newServer(t, nil)
	job := "/00000000-0000-4000-8000-000000000000"
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 404},
		{"GET", "/v2/upload/", 404},
		{"GET", "/v2/transcript" + job + "/txt", 404},
		{"GET", "/v2//transcript", 404},
		{"GET", "/v2/../metrics", 404},
		{"PUT", "/v2/upload", 405},
		{"GET", "/v2/transcript", 405},
		{"POST", "/v2/transcript" + job + "/srt", 405},
	} {
		res := do(t, srv, tc.method, tc.path, clientKey, nil)
		if res.status != tc.status || res.header.Get("Location") != "" || res.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: %d %v", tc.method, tc.path, res.status, res.header)
		}
		res.message(t)
	}
	res := do(t, srv, "GET", "/v1/models", clientKey, nil)
	if res.status != 404 || res.body != `{"error":{"code":"404","message":"Not Found","type":"invalid_request_error"}}`+"\n" {
		t.Errorf("v1: %d %s", res.status, res.body)
	}
}

func TestDeclaredBodyLimits(t *testing.T) {
	a, srv := newServer(t, nil)
	for path, limit := range map[string]int{
		"/v2/upload":               4096 + 65536,
		"/v1/audio/transcriptions": 4096 + 65536,
		"/v2/transcript":           65536,
		"/internal/jobs/claim":     65536,
		"/internal/jobs/00000000-0000-4000-8000-000000000000/complete": 16 << 20,
	} {
		key := clientKey
		if strings.HasPrefix(path, "/internal/") {
			key = workerKey
		}
		head := "POST " + path + " HTTP/1.1\nHost: x\nAuthorization: " + key + "\nContent-Length: "
		// The body is never sent: the answer must not wait for it.
		if res := raw(t, srv, head+strconv.Itoa(limit+1)+"\n"); res.status != 413 || res.elapsed > a.cfg.UploadIdle {
			t.Errorf("%s: %d after %v", path, res.status, res.elapsed)
		}
	}
	res := raw(t, srv, "POST /v2/upload HTTP/1.1\nHost: x\nContent-Length: 1000000000\n")
	if res.status != 401 {
		t.Errorf("authentication must come first: %d", res.status)
	}
}

func TestStreamedBodyLimits(t *testing.T) {
	_, srv := newServer(t, nil)
	chunked := func(n int) io.Reader { return io.MultiReader(strings.NewReader(strings.Repeat(" ", n))) }
	for _, tc := range []struct {
		path    string
		size    int
		status  int
		message string
	}{
		{"/v2/transcript", 65537, 413, "Request too large"},
		{"/v2/upload", 4097, 413, "Audio too large"},
		{"/v2/upload", 4096, 200, ""},
	} {
		res := do(t, srv, "POST", tc.path, clientKey, chunked(tc.size))
		if res.status != tc.status || tc.message != "" && res.message(t) != tc.message {
			t.Errorf("%s with %d bytes: %d %s", tc.path, tc.size, res.status, res.body)
		}
	}
}

func TestSlowBodies(t *testing.T) {
	a, srv := newServer(t, func(s *Settings) { s.UploadIdle, s.UploadRate = 200*time.Millisecond, 1000 })
	form := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\r\n\r\n" + strings.Repeat("x", 900) + "\r\n--b--\r\n"
	trickle := []any{}
	for i := range 40 {
		trickle = append(trickle, form[i:i+1], 50*time.Millisecond)
	}
	for _, tc := range []struct {
		name, head string
		chunks     []any
		message    string
	}{
		{"stalled upload", "POST /v2/upload HTTP/1.1\nContent-Length: 1000\n", []any{"x"}, "Upload stalled"},
		{"trickled upload", "POST /v2/upload HTTP/1.1\nContent-Length: 1000\n", trickle, "Upload too slow"},
		{"stalled JSON", "POST /v2/transcript HTTP/1.1\nContent-Length: 100\n", []any{"{"}, "Upload stalled"},
		{"trickled form", "POST /v1/audio/transcriptions HTTP/1.1\nContent-Type: multipart/form-data; boundary=b\nContent-Length: " +
			strconv.Itoa(len(form)) + "\n", trickle, "Upload too slow"},
	} {
		res := raw(t, srv, strings.Replace(tc.head, "\n", "\nHost: x\nAuthorization: "+clientKey+"\n", 1), tc.chunks...)
		if res.status != 408 || !strings.Contains(res.body, tc.message) || res.elapsed > 3*time.Second {
			t.Errorf("%s: %d %s after %v", tc.name, res.status, res.body, res.elapsed)
		}
	}
	if files, _ := os.ReadDir(filepath.Join(a.cfg.DataDir, "audio")); len(files) != 0 {
		t.Errorf("audio left: %d files", len(files))
	}
	// The slot is free again.
	if res := do(t, srv, "POST", "/v2/upload", clientKey, strings.NewReader("audio")); res.status != 200 {
		t.Errorf("upload after slow bodies: %d %s", res.status, res.body)
	}
}

func TestUploadSlots(t *testing.T) {
	_, srv := newServer(t, func(s *Settings) { s.UploadIdle = 2 * time.Second })
	holder := make(chan reply)
	go func() {
		holder <- raw(t, srv, "POST /v2/upload HTTP/1.1\nHost: x\nAuthorization: "+clientKey+"\nContent-Length: 10\n",
			"12345", 300*time.Millisecond, "67890")
	}()
	time.Sleep(100 * time.Millisecond)
	for _, path := range []string{"/v2/upload", "/v1/audio/transcriptions"} {
		res := do(t, srv, "POST", path, clientKey, strings.NewReader("audio"))
		if res.status != 429 || res.header.Get("Retry-After") != "5" || !strings.Contains(res.body, "Upload slots full") {
			t.Errorf("%s: %d %s", path, res.status, res.body)
		}
	}
	if res := do(t, srv, "GET", "/metrics", clientKey, nil); res.status != 200 {
		t.Errorf("metrics need no slot: %d", res.status)
	}
	if res := <-holder; res.status != 200 {
		t.Fatalf("holder: %d %s", res.status, res.body)
	}
	if res := do(t, srv, "POST", "/v2/upload", clientKey, strings.NewReader("audio")); res.status != 200 {
		t.Errorf("slot not released: %d %s", res.status, res.body)
	}
}

// A declared body that no handler reads must not hold the connection beyond the idle deadline.
func TestUnreadBodiesCloseTheConnection(t *testing.T) {
	a, srv := newServer(t, nil)
	for _, tc := range []struct {
		head   string
		status int
	}{
		{"GET /health/live HTTP/1.1\nContent-Length: 100\n", 200},
		{"GET /health/ready HTTP/1.1\nTransfer-Encoding: chunked\n", 200},
		{"POST /v2/transcript HTTP/1.1\nAuthorization: wrong\nContent-Length: 100\n", 401},
		{"GET /metrics HTTP/1.1\nAuthorization: " + clientKey + "\nContent-Length: 10\n", 200},
		{"PUT /v2/upload HTTP/1.1\nAuthorization: " + clientKey + "\nContent-Length: 10\n", 405},
		{"POST /v2/upload HTTP/1.1\nAuthorization: " + clientKey + "\nContent-Length: 10\n", 408},
	} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		io.WriteString(conn, strings.ReplaceAll(strings.Replace(tc.head, "\n", "\nHost: x\n", 1), "\n", "\r\n")+"\r\n")
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		res, err := http.ReadResponse(r, nil)
		if err != nil || res.StatusCode != tc.status {
			t.Fatalf("%q: %v %v", tc.head, res, err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil || time.Since(start) > a.cfg.UploadIdle+time.Second {
			t.Errorf("%q: connection held for %v: %v", tc.head, time.Since(start), err)
		}
		conn.Close()
	}
}

func TestKeepAliveAfterABody(t *testing.T) {
	_, srv := newServer(t, func(s *Settings) { s.UploadIdle = 100 * time.Millisecond })
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	io.WriteString(conn, "POST /v2/upload HTTP/1.1\r\nHost: x\r\nAuthorization: "+clientKey+"\r\nContent-Length: 5\r\n\r\naudio")
	if res, err := http.ReadResponse(r, nil); err != nil || res.StatusCode != 200 || res.Close {
		t.Fatalf("first: %v %v", res, err)
	} else {
		io.Copy(io.Discard, res.Body)
	}
	time.Sleep(300 * time.Millisecond)
	io.WriteString(conn, "GET /metrics HTTP/1.1\r\nHost: x\r\nAuthorization: "+clientKey+"\r\n\r\n")
	if res, err := http.ReadResponse(r, nil); err != nil || res.StatusCode != 200 {
		t.Fatalf("second: %v %v", res, err)
	}
}

func TestJSONRequests(t *testing.T) {
	_, srv := newServer(t, nil)
	upload := do(t, srv, "POST", "/v2/upload", clientKey, strings.NewReader("audio"))
	var uploaded struct {
		UploadURL string `json:"upload_url"`
	}
	json.Unmarshal([]byte(upload.body), &uploaded)
	url := `"` + uploaded.UploadURL + `"`
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"audio_url":` + url + `,"language_code":"en"}`, 200},
		{`{"audio_url":` + url + `,"language_code":"en"}` + "\n", 200},
		{`{"audio_url":` + url + `,"language_code":null}`, 409},
		{`{"audio_url":` + url + `,"language_code":"xx"}`, 400},
		{`{"audio_url":` + url + `} {}`, 422},
		{`{"audio_url":` + url + `}x`, 422},
		{`{"audio_url":` + url + `,"speaker_labels":true}`, 422},
		{`{"audio_url":null}`, 422},
		{`{"audio_url":5}`, 422},
		{`{}`, 422},
		{`null`, 422},
		{``, 422},
	} {
		res := do(t, srv, "POST", "/v2/transcript", clientKey, strings.NewReader(tc.body))
		if res.status != tc.status {
			t.Errorf("%s: %d %s", tc.body, res.status, res.body)
		}
		if tc.status == 422 && res.message(t) != "Invalid or unsupported request fields" {
			t.Errorf("%s: %s", tc.body, res.body)
		}
	}
}
