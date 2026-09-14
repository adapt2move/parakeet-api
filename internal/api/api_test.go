package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adapt2move/parakeet-api/internal/store"
)

var (
	clientKey = "client-" + strings.Repeat("x", 32)
	workerKey = "worker-" + strings.Repeat("y", 32)
)

func testSettings(t *testing.T) Settings {
	return Settings{
		DataDir:         t.TempDir(),
		APIKey:          clientKey,
		WorkerKey:       workerKey,
		PublicURL:       "http://api.test",
		MaxUploadBytes:  4096,
		MaxStorageBytes: 1 << 20,
		MaxPendingJobs:  8,
		Retention:       time.Hour,
		MaxJobAge:       time.Hour,
		Lease:           15 * time.Second,
		MaxAttempts:     3,
		SyncTimeout:     5 * time.Second,
		UploadSlots:     1,
		UploadIdle:      300 * time.Millisecond,
		UploadRate:      1000,
		ListenAddr:      "127.0.0.1:0",
	}
}

func newAPI(t *testing.T, change func(*Settings)) *API {
	t.Helper()
	cfg := testSettings(t)
	if change != nil {
		change(&cfg)
	}
	st, err := store.New(cfg.StoreConfig())
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, st, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func serve(t *testing.T, a *API) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, key string, body io.Reader, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(data)
}

func audioFiles(t *testing.T, a *API) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(a.cfg.DataDir, "audio"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// rawConn sends a request head and returns the connection for byte-level control of the body.
func rawConn(t *testing.T, srv *httptest.Server, head string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, strings.ReplaceAll(head, "\n", "\r\n")+"\r\n"); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}

func readResponse(t *testing.T, r *bufio.Reader) (*http.Response, string) {
	t.Helper()
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

func TestAuthentication(t *testing.T) {
	a := newAPI(t, nil)
	srv := serve(t, a)
	for _, path := range []string{"/health/live", "/health/ready"} {
		if res, body := do(t, srv, "GET", path, "", nil); res.StatusCode != 200 || body != `{"status":"ok"}` {
			t.Fatalf("%s: %d %s", path, res.StatusCode, body)
		}
	}
	for _, tc := range []struct{ path, key string }{
		{"/metrics", ""},
		{"/metrics", workerKey},
		{"/metrics", "Bearer  " + clientKey},
		{"/metrics", "bearer " + clientKey},
		{"/v1/audio/transcriptions", "wrong"},
		{"/internal/jobs/claim", clientKey},
		{"/internal/jobs/claim", "Bearer " + clientKey},
	} {
		res, body := do(t, srv, "POST", tc.path, tc.key, nil)
		if res.StatusCode != 401 || body != `{"error":"Unauthorized"}` {
			t.Fatalf("%s with %q: %d %s", tc.path, tc.key, res.StatusCode, body)
		}
	}
	if res, _ := do(t, srv, "GET", "/metrics", "Bearer "+clientKey, nil); res.StatusCode != 200 {
		t.Fatalf("bearer client key: %d", res.StatusCode)
	}
	if res, _ := do(t, srv, "POST", "/internal/jobs/claim", workerKey, nil); res.StatusCode != 204 {
		t.Fatalf("raw worker key: %d", res.StatusCode)
	}

	// Like the Python middleware's dict of headers, the last Authorization header counts.
	_, r := rawConn(t, srv, "GET /metrics HTTP/1.1\nHost: x\nAuthorization: "+clientKey+"\nAuthorization: wrong\n")
	if res, _ := readResponse(t, r); res.StatusCode != 401 {
		t.Fatalf("last header wrong: %d", res.StatusCode)
	}
	_, r = rawConn(t, srv, "GET /metrics HTTP/1.1\nHost: x\nAuthorization: wrong\nAuthorization: "+clientKey+"\n")
	if res, _ := readResponse(t, r); res.StatusCode != 200 {
		t.Fatalf("last header right: %d", res.StatusCode)
	}
}

func TestDeclaredContentLengthIsRejectedWithoutReadingTheBody(t *testing.T) {
	a := newAPI(t, nil)
	srv := serve(t, a)
	for _, tc := range []struct {
		path  string
		key   string
		limit int64
	}{
		{"/v2/upload", clientKey, 4096 + 65536},
		{"/v1/audio/transcriptions", clientKey, 4096 + 65536},
		{"/v2/transcript", clientKey, 65536},
		{"/v2/anything/complete", clientKey, 16 << 20},
		{"/internal/jobs/x/complete", workerKey, 16 << 20},
		{"/internal/jobs/x/heartbeat", workerKey, 65536},
	} {
		head := fmt.Sprintf("POST %s HTTP/1.1\nHost: x\nAuthorization: %s\nContent-Length: %d\n", tc.path, tc.key, tc.limit+1)
		_, r := rawConn(t, srv, head)
		res, body := readResponse(t, r)
		if res.StatusCode != 413 || body != `{"error":"Request too large"}` || !res.Close {
			t.Fatalf("%s: %d %s close=%v", tc.path, res.StatusCode, body, res.Close)
		}
	}
}

func TestUploadSlots(t *testing.T) {
	a := newAPI(t, func(s *Settings) { s.UploadIdle = 2 * time.Second })
	srv := serve(t, a)
	// A failed upload gives its slot back.
	if res, body := do(t, srv, "POST", "/v2/upload", clientKey, nil); res.StatusCode != 400 || body != `{"error":"Audio is empty"}` {
		t.Fatalf("empty upload: %d %s", res.StatusCode, body)
	}
	conn, r := rawConn(t, srv, "POST /v2/upload HTTP/1.1\nHost: x\nAuthorization: "+clientKey+"\nContent-Length: 10\n")
	io.WriteString(conn, "12345")
	time.Sleep(100 * time.Millisecond)
	res, body := do(t, srv, "POST", "/v1/audio/transcriptions", clientKey, strings.NewReader("x"))
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "5" || body != `{"error":"Upload slots full"}` {
		t.Fatalf("second upload: %d %q %s", res.StatusCode, res.Header.Get("Retry-After"), body)
	}
	// Other requests do not compete for upload slots.
	if res, _ := do(t, srv, "GET", "/metrics", clientKey, nil); res.StatusCode != 200 {
		t.Fatalf("metrics: %d", res.StatusCode)
	}
	io.WriteString(conn, "67890")
	if res, body := readResponse(t, r); res.StatusCode != 200 {
		t.Fatalf("holder: %d %s", res.StatusCode, body)
	}
	if res, body := do(t, srv, "POST", "/v2/upload", clientKey, strings.NewReader("audio")); res.StatusCode != 200 {
		t.Fatalf("after release: %d %s", res.StatusCode, body)
	}
	// The slot is released after the response is written.
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.slotsMu.Lock()
		held := a.uploads
		a.slotsMu.Unlock()
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still held", held)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStalledAndTricklingBodies(t *testing.T) {
	a := newAPI(t, func(s *Settings) { s.UploadSlots = 4 })
	srv := serve(t, a)
	for _, tc := range []struct {
		name, path, contentType, prefix, message string
		interval                                 time.Duration // 0 stalls after the prefix
	}{
		{"stalled upload", "/v2/upload", "", "-", `{"error":"Upload stalled"}`, 0},
		{"stalled json", "/v2/transcript", "application/json", "{", `{"error":"Upload stalled"}`, 0},
		{"trickling upload", "/v2/upload", "", "-", `{"error":"Upload too slow"}`, 20 * time.Millisecond},
		{"trickling v1", "/v1/audio/transcriptions", "multipart/form-data; boundary=b",
			"--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\r\n\r\n",
			`{"error":{"message":"Upload too slow","type":"invalid_request_error","code":"408"}}`, 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := "POST " + tc.path + " HTTP/1.1\nHost: x\nAuthorization: " + clientKey + "\nContent-Length: 2000\n"
			if tc.contentType != "" {
				head += "Content-Type: " + tc.contentType + "\n"
			}
			started := time.Now()
			conn, r := rawConn(t, srv, head)
			done := make(chan struct{})
			go func() {
				defer close(done)
				io.WriteString(conn, tc.prefix)
				for tc.interval > 0 {
					time.Sleep(tc.interval)
					if _, err := io.WriteString(conn, "-"); err != nil {
						return
					}
				}
			}()
			res, body := readResponse(t, r)
			conn.Close()
			<-done
			if res.StatusCode != 408 || body != tc.message {
				t.Fatalf("%d %s", res.StatusCode, body)
			}
			if elapsed := time.Since(started); elapsed < 250*time.Millisecond || elapsed > 3*time.Second {
				t.Fatalf("answered after %v", elapsed)
			}
			if files := audioFiles(t, a); len(files) != 0 {
				t.Fatalf("left audio files %v", files)
			}
		})
	}
}

func TestStreamedBodyLimit(t *testing.T) {
	a := newAPI(t, nil)
	srv := serve(t, a)
	// io.MultiReader hides the length, so the client sends a chunked body.
	res, body := do(t, srv, "POST", "/v2/transcript", clientKey,
		io.MultiReader(bytes.NewReader(bytes.Repeat([]byte(" "), 70000))), "Content-Type", "application/json")
	if res.StatusCode != 413 || body != `{"error":"Request too large"}` {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = do(t, srv, "POST", "/v2/upload", clientKey, io.MultiReader(bytes.NewReader(make([]byte, 4097))))
	if res.StatusCode != 413 || body != `{"error":"Audio too large"}` {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if files := audioFiles(t, a); len(files) != 0 {
		t.Fatalf("left audio files %v", files)
	}
}

func TestKeepAliveAfterABodyOutlivesTheIdleDeadline(t *testing.T) {
	a := newAPI(t, func(s *Settings) { s.UploadIdle = 100 * time.Millisecond })
	srv := serve(t, a)
	conn, r := rawConn(t, srv, "POST /v2/upload HTTP/1.1\nHost: x\nAuthorization: "+clientKey+"\nContent-Length: 5\n")
	io.WriteString(conn, "audio")
	if res, body := readResponse(t, r); res.StatusCode != 200 || res.Close {
		t.Fatalf("first: %d %s close=%v", res.StatusCode, body, res.Close)
	}
	time.Sleep(400 * time.Millisecond)
	io.WriteString(conn, "GET /metrics HTTP/1.1\r\nHost: x\r\nAuthorization: "+clientKey+"\r\n\r\n")
	if res, body := readResponse(t, r); res.StatusCode != 200 {
		t.Fatalf("second: %d %s", res.StatusCode, body)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// advancingReader moves the clock when the body is fully read.
type advancingReader struct {
	r     io.Reader
	clock *fakeClock
	by    time.Duration
}

func (a *advancingReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if err == io.EOF {
		a.clock.Advance(a.by)
	}
	return n, err
}

func TestOverallDeadlineAnswers504(t *testing.T) {
	a := newAPI(t, nil)
	clock := &fakeClock{now: time.Now()}
	a.now = clock.Now
	form := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\r\n\r\naudio\r\n--b--\r\n"
	for _, path := range []string{"/v2/upload", "/v1/audio/transcriptions"} {
		body := &advancingReader{r: strings.NewReader(form), clock: clock, by: a.cfg.SyncTimeout + transferLimit}
		req := httptest.NewRequest("POST", path, body)
		req.Header.Set("Authorization", clientKey)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != 504 || rec.Body.String() != `{"error":"Request timed out"}` || rec.Header().Get("Retry-After") != "" {
			t.Fatalf("%s: %d %q %s", path, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
		}
	}
	if files := audioFiles(t, a); len(files) != 0 {
		t.Fatalf("left audio files %v", files)
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	a := newAPI(t, nil)
	srv := serve(t, a)
	for _, tc := range []struct{ method, path, key, body, allow string }{
		{"GET", "/v2/nothing", clientKey, `{"error":"Not Found"}`, ""},
		{"GET", "/v2/transcript/a/b/c", clientKey, `{"error":"Not Found"}`, ""},
		{"GET", "/v2/transcript//srt", clientKey, `{"error":"Not Found"}`, ""},
		{"GET", "/v1/models", clientKey, `{"error":{"message":"Not Found","type":"invalid_request_error","code":"404"}}`, ""},
		{"PUT", "/v2/upload", clientKey, `{"error":"Method Not Allowed"}`, "POST"},
		{"PUT", "/v2/transcript/abc", clientKey, `{"error":"Method Not Allowed"}`, "GET, DELETE"},
		{"GET", "/v1/audio/transcriptions", clientKey,
			`{"error":{"message":"Method Not Allowed","type":"invalid_request_error","code":"405"}}`, "POST"},
		{"HEAD", "/health/live", "", "", "GET"},
		{"GET", "/internal/jobs/claim", workerKey, `{"error":"Method Not Allowed"}`, "POST"},
	} {
		res, body := do(t, srv, tc.method, tc.path, tc.key, nil)
		want := 404
		if tc.allow != "" {
			want = 405
		}
		if res.StatusCode != want || body != tc.body || res.Header.Get("Allow") != tc.allow {
			t.Fatalf("%s %s: %d %q %s", tc.method, tc.path, res.StatusCode, res.Header.Get("Allow"), body)
		}
	}
}

func TestWorkerCycleAndResponseBytes(t *testing.T) {
	a := newAPI(t, nil)
	srv := serve(t, a)
	_, body := do(t, srv, "POST", "/v2/upload", clientKey, strings.NewReader("audio"))
	var upload struct {
		UploadURL string `json:"upload_url"`
	}
	json.Unmarshal([]byte(body), &upload)
	res, body := do(t, srv, "POST", "/v2/transcript", clientKey,
		strings.NewReader(`{"audio_url":"`+upload.UploadURL+`","language_code":"de"}`), "Content-Type", "application/json")
	if res.StatusCode != 200 {
		t.Fatalf("submit: %d %s", res.StatusCode, body)
	}
	var job struct{ ID string }
	json.Unmarshal([]byte(body), &job)

	res, body = do(t, srv, "POST", "/internal/jobs/claim", workerKey, nil)
	var claim struct{ ID, Token string }
	json.Unmarshal([]byte(body), &claim)
	want := `{"id":"` + job.ID + `","token":"` + claim.Token + `","lease_seconds":15,"bytes":5,"options":{"language_code":"de"}}`
	if res.StatusCode != 200 || body != want {
		t.Fatalf("claim: %d %s", res.StatusCode, body)
	}
	res, body = do(t, srv, "GET", "/internal/jobs/"+job.ID+"/audio", workerKey, nil, "X-Lease-Token", claim.Token)
	if res.StatusCode != 200 || body != "audio" || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("audio: %d %s %v", res.StatusCode, body, res.Header)
	}
	result := `{"result":{"text":"a<b","words":[{"text":"a<b","start":0,"end":10,"confidence":1}],"audio_duration_ms":10,"chunks":1,"seam_fallbacks":0}}`
	res, body = do(t, srv, "POST", "/internal/jobs/"+job.ID+"/complete", workerKey, strings.NewReader(result),
		"Content-Type", "application/json")
	if res.StatusCode != 409 || body != `{"error":"Missing lease token"}` {
		t.Fatalf("complete without token: %d %s", res.StatusCode, body)
	}
	res, body = do(t, srv, "POST", "/internal/jobs/"+job.ID+"/complete", workerKey, strings.NewReader(result),
		"Content-Type", "application/json", "X-Lease-Token", claim.Token)
	if res.StatusCode != 200 || body != `{"ok":true}` {
		t.Fatalf("complete: %d %s", res.StatusCode, body)
	}
	res, body = do(t, srv, "GET", "/v2/transcript/"+job.ID, clientKey, nil)
	want = `{"id":"` + job.ID + `","status":"completed","error":null,"audio_url":"` + upload.UploadURL +
		`","text":"a<b","words":[{"text":"a<b","start":0,"end":10,"confidence":1.0,"speaker":null,"channel":null}],` +
		`"confidence":1.0,"audio_duration":1,"language_code":"de","language_confidence":null,"utterances":null}`
	if res.StatusCode != 200 || body != want {
		t.Fatalf("transcript:\n got %s\nwant %s", body, want)
	}
	res, body = do(t, srv, "GET", "/v2/transcript/"+job.ID+"/srt?chars_per_caption=+2_0.00", clientKey, nil)
	if res.StatusCode != 200 || body != "1\n00:00:00,000 --> 00:00:00,010\na&lt;b\n" ||
		res.Header.Get("Content-Type") != "application/x-subrip" {
		t.Fatalf("srt: %d %q %s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	res, body = do(t, srv, "GET", "/metrics", clientKey, nil)
	if body != "parakeet_jobs{status=\"completed\"} 1\n" || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("metrics: %q %s", res.Header.Get("Content-Type"), body)
	}
}

func TestSyncTranscriptionWaitsAndCleansUp(t *testing.T) {
	a := newAPI(t, func(s *Settings) { s.SyncTimeout = 300 * time.Millisecond })
	a.poll = 10 * time.Millisecond
	srv := serve(t, a)
	form := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\r\n\r\naudio\r\n--b--\r\n"
	res, body := do(t, srv, "POST", "/v1/audio/transcriptions", clientKey, strings.NewReader(form),
		"Content-Type", "multipart/form-data; boundary=b")
	want := `{"error":{"message":"Transcription wait timed out; use the asynchronous /v2 API","type":"invalid_request_error","code":"504"}}`
	if res.StatusCode != 504 || body != want {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if files := audioFiles(t, a); len(files) != 0 || len(a.store.Counts()) != 0 {
		t.Fatalf("left files %v and jobs %v", files, a.store.Counts())
	}
}

func TestRunLocksDataDirAndShutsDown(t *testing.T) {
	cfg := testSettings(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddr = probe.Addr().String()
	probe.Close()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log) }()

	lockPath := filepath.Join(cfg.DataDir, "api.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if res, err := http.Get("http://" + cfg.ListenAddr + "/health/live"); err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	marker := filepath.Join(cfg.DataDir, "audio", "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second := cfg
	if err := Run(context.Background(), second, log); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("second run touched the audio directory: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestHealthcheck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/ready" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(status)
	})}}
	srv.Start()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if code := Healthcheck(":" + port); code != 0 {
		t.Fatalf("healthy: %d", code)
	}
	if code := Healthcheck("0.0.0.0:" + port); code != 0 {
		t.Fatalf("healthy with host: %d", code)
	}
	status = http.StatusServiceUnavailable
	if code := Healthcheck(":" + port); code != 1 {
		t.Fatalf("unhealthy: %d", code)
	}
	if code := Healthcheck("no-port"); code != 1 {
		t.Fatalf("invalid address: %d", code)
	}
}
