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
	st, err := store.New(cfg.Config)
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg, st, slog.New(slog.DiscardHandler))
	srv := httptest.NewUnstartedServer(a)
	srv.Config = a.server()
	srv.Start()
	t.Cleanup(srv.Close)
	return a, srv
}

type reply struct {
	status  int
	header  http.Header
	body    string
	closed  bool // the server closed the connection after the response
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

// raw sends a request head, then each body chunk after its delay, and reads the response, and
// the end of the connection when the response closes it. It stops sending once the server answers.
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
	closed := false
	if res.Close {
		_, err = io.Copy(io.Discard, r)
		closed = err == nil
	}
	return reply{status: res.StatusCode, header: res.Header, body: string(data), closed: closed, elapsed: time.Since(start)}
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

// A declared body that no handler reads must not hold the connection beyond the idle deadline.
func TestUnreadBodiesCloseTheConnection(t *testing.T) {
	a, srv := newServer(t, nil)
	for head, status := range map[string]int{
		"GET /health/live HTTP/1.1\nContent-Length: 100\n":                                 200,
		"GET /health/ready HTTP/1.1\nTransfer-Encoding: chunked\n":                         200,
		"POST /v2/transcript HTTP/1.1\nAuthorization: wrong\nContent-Length: 100\n":        401,
		"GET /metrics HTTP/1.1\nAuthorization: " + clientKey + "\nContent-Length: 10\n":    200,
		"PUT /v2/upload HTTP/1.1\nAuthorization: " + clientKey + "\nContent-Length: 10\n":  405,
		"POST /v2/upload HTTP/1.1\nAuthorization: " + clientKey + "\nContent-Length: 10\n": 408,
		// net/http answers OPTIONS * itself unless told not to, reading the body without a deadline.
		"OPTIONS * HTTP/1.1\nContent-Length: 100\n": 401,
	} {
		res := raw(t, srv, strings.Replace(head, "\n", "\nHost: x\n", 1))
		if res.status != status || !res.closed || res.elapsed > a.cfg.UploadIdle+time.Second {
			t.Errorf("%q: %d, closed %v after %v", head, res.status, res.closed, res.elapsed)
		}
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

// A client that stops reading a response must not hold the handler and its audio file.
func TestStalledResponseReaders(t *testing.T) {
	a, srv := newServer(t, func(s *Settings) { s.MaxUploadBytes, s.MaxStorageBytes = 32<<20, 64<<20 })
	audio := strings.Repeat("x", 32<<20)
	id, err := a.store.Save(strings.NewReader(audio))
	if err == nil {
		_, err = a.store.Submit(id, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	claim := a.store.Claim()
	conn, err := net.DialTCP("tcp", nil, srv.Listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadBuffer(4 << 10)
	io.WriteString(conn, "GET /internal/jobs/"+claim.ID+"/audio HTTP/1.1\r\nHost: x\r\nAuthorization: "+workerKey+
		"\r\nX-Lease-Token: "+claim.Token+"\r\n\r\n")
	time.Sleep(a.cfg.UploadIdle + time.Second)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	received, err := io.Copy(io.Discard, conn)
	if err != nil || received >= int64(len(audio)) {
		t.Fatalf("received %d of %d bytes: %v", received, len(audio), err)
	}
}

func TestJSONFieldNamesAreExact(t *testing.T) {
	_, srv := newServer(t, nil)
	upload := do(t, srv, "POST", "/v2/upload", clientKey, strings.NewReader("audio"))
	url := strings.TrimSuffix(strings.TrimPrefix(upload.body, `{"upload_url":`), "}\n")
	for body, status := range map[string]int{
		`{"audio_url":` + url + `,"Language_Code":"en"}`: 422,
		`{"AUDIO_URL":` + url + `}`:                      422,
		`{"audio_url":` + url + `,"language_code":null}`: 200,
	} {
		if res := do(t, srv, "POST", "/v2/transcript", clientKey, strings.NewReader(body)); res.status != status {
			t.Errorf("%s: %d %s", body, res.status, res.body)
		}
	}
}
