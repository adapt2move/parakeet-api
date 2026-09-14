package store

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var result = json.RawMessage(`{"text":"Hello world.","audio_duration_ms":2100,"chunks":1,"seam_fallbacks":0,` +
	`"words":[{"text":"Hello","start":120,"end":610,"confidence":0.8},{"text":"world.","start":900,"end":1700,"confidence":0.7}]}`)

func config(dir string, c *clock) Config {
	return Config{
		DataDir:         dir,
		MaxUploadBytes:  1024,
		MaxStorageBytes: 4096,
		MaxPendingJobs:  32,
		Retention:       60 * time.Second,
		MaxJobAge:       21600 * time.Second,
		Lease:           15 * time.Second,
		MaxAttempts:     3,
		Now:             c.Now,
	}
}

func newStore(t *testing.T, change func(*Config)) (*Store, *clock) {
	t.Helper()
	c := &clock{now: time.Now()}
	cfg := config(t.TempDir(), c)
	if change != nil {
		change(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func submit(t *testing.T, s *Store) Job {
	t.Helper()
	uid, err := s.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.BlobPath(uid), []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.UploadReady(uid, 5); err != nil {
		t.Fatal(err)
	}
	job, err := s.Submit(uid, nil)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func mustClaim(t *testing.T, s *Store) *Claim {
	t.Helper()
	c := s.Claim()
	if c == nil {
		t.Fatal("expected a claim")
	}
	return c
}

func status(t *testing.T, s *Store, id string) Status {
	t.Helper()
	job, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return job.Status
}

func wantError(t *testing.T, err error, code int) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error with status %d, got %v", code, err)
	}
	if e.Status != code {
		t.Fatalf("status = %d (%s), want %d", e.Status, e.Message, code)
	}
	return e
}

func blobs(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.blobs)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func exists(s *Store, uploadID string) bool {
	_, err := os.Stat(s.BlobPath(uploadID))
	return err == nil
}

func used(s *Store) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, u := range s.uploads {
		sum += u.bytes
	}
	if sum != s.used {
		panic("storage counter out of sync")
	}
	return s.used
}

func TestClaimIsExclusiveAndLeaseExpiryReassigns(t *testing.T) {
	s, c := newStore(t, nil)
	job := submit(t, s)
	claims := make([]*Claim, 8)
	var wg sync.WaitGroup
	for i := range claims {
		wg.Go(func() { claims[i] = s.Claim() })
	}
	wg.Wait()
	var first *Claim
	for _, claim := range claims {
		if claim != nil {
			if first != nil {
				t.Fatal("job claimed twice")
			}
			first = claim
		}
	}
	if first == nil || first.ID != job.ID || first.Bytes != 5 || first.LeaseSeconds != 15 {
		t.Fatalf("unexpected claim %+v", first)
	}
	c.Advance(15 * time.Second)
	second := mustClaim(t, s)
	if second.ID != first.ID || second.Token == first.Token {
		t.Fatalf("expected a new lease on the same job, got %+v", second)
	}
	wantError(t, s.Finish(first.ID, first.Token, result, "", false), http.StatusConflict)
	if err := s.Finish(second.ID, second.Token, result, "", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCompleted || string(got.Result) != string(result) || got.Error != nil || got.Attempts != 2 {
		t.Fatalf("unexpected job %+v", got)
	}
}

func TestClaimPayloadSerializesLikeWorkerExpects(t *testing.T) {
	s, _ := newStore(t, nil)
	submit(t, s)
	body, err := json.Marshal(mustClaim(t, s))
	if err != nil {
		t.Fatal(err)
	}
	pattern := `^\{"id":"[0-9a-f-]{36}","token":"[0-9a-f-]{36}","lease_seconds":15,"bytes":5,"options":\{\}\}$`
	if !regexp.MustCompile(pattern).Match(body) {
		t.Fatalf("unexpected claim JSON %s", body)
	}
}

func TestClaimOrderIsOldestFirst(t *testing.T) {
	s, c := newStore(t, nil)
	a := submit(t, s)
	b := submit(t, s) // same instant, later insertion
	c.Advance(time.Second)
	d := submit(t, s)
	for _, want := range []string{a.ID, b.ID, d.ID} {
		if got := mustClaim(t, s); got.ID != want {
			t.Fatalf("claimed %s, want %s", got.ID, want)
		}
	}
	if s.Claim() != nil {
		t.Fatal("queue should be empty")
	}
}

func TestLeaseExpiryIsInclusiveAndFencesAllOperations(t *testing.T) {
	s, c := newStore(t, nil)
	job := submit(t, s)
	claim := mustClaim(t, s)
	c.Advance(14 * time.Second)
	if err := s.Heartbeat(job.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	c.Advance(14 * time.Second) // heartbeat extended the lease to 29 s
	f, err := s.Audio(job.ID, claim.Token)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "audio" {
		t.Fatalf("audio = %q", data)
	}
	wantError(t, s.Heartbeat(job.ID, "wrong"), http.StatusConflict)
	wantError(t, s.Heartbeat(job.ID, ""), http.StatusConflict)
	wantError(t, s.Heartbeat("missing", claim.Token), http.StatusConflict)
	c.Advance(time.Second) // exactly at the lease deadline
	wantError(t, s.Heartbeat(job.ID, claim.Token), http.StatusConflict)
	_, err = s.Audio(job.ID, claim.Token)
	wantError(t, err, http.StatusConflict)
	wantError(t, s.Finish(job.ID, claim.Token, result, "", false), http.StatusConflict)
	if status(t, s, job.ID) != StatusProcessing {
		t.Fatal("an expired lease is only recovered by the next claim")
	}
}

func TestRetryExhaustion(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxAttempts = 2 })
	job := submit(t, s)
	for range 2 {
		claim := mustClaim(t, s)
		if err := s.Finish(claim.ID, claim.Token, nil, "Transient", true); err != nil {
			t.Fatal(err)
		}
	}
	if s.Claim() != nil {
		t.Fatal("exhausted job was claimed again")
	}
	got, _ := s.Get(job.ID)
	if got.Status != StatusError || got.Error == nil || *got.Error != "Transient" || got.Attempts != 2 {
		t.Fatalf("unexpected job %+v", got)
	}
}

func TestRetryRequeuesWithoutError(t *testing.T) {
	s, _ := newStore(t, nil)
	job := submit(t, s)
	claim := mustClaim(t, s)
	if err := s.Finish(claim.ID, claim.Token, nil, "Transient", true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(job.ID)
	if got.Status != StatusQueued || got.Error != nil || !exists(s, job.UploadID) || used(s) != 5 {
		t.Fatalf("unexpected job %+v", got)
	}
	// Requeueing revokes the old token.
	wantError(t, s.Heartbeat(claim.ID, claim.Token), http.StatusConflict)
}

func TestLeaseExpiryExhaustsAttempts(t *testing.T) {
	s, c := newStore(t, func(c *Config) { c.MaxAttempts = 2 })
	job := submit(t, s)
	for range 2 {
		mustClaim(t, s)
		c.Advance(15 * time.Second)
	}
	if s.Claim() != nil {
		t.Fatal("exhausted job was claimed again")
	}
	got, _ := s.Get(job.ID)
	if got.Status != StatusError || *got.Error != "Worker lease expired" || exists(s, job.UploadID) {
		t.Fatalf("unexpected job %+v", got)
	}
}

func TestQueueAndStorageAreBounded(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxPendingJobs = 1 })
	job := submit(t, s)
	again, err := s.Submit(job.UploadID, map[string]string{})
	if err != nil || again.ID != job.ID {
		t.Fatalf("resubmit = %+v, %v", again, err)
	}
	uid, err := s.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UploadReady(uid, 5); err != nil {
		t.Fatal(err)
	}
	_, err = s.Submit(uid, nil)
	if e := wantError(t, err, http.StatusTooManyRequests); e.RetryAfter != 10 || e.Message != "Queue full" {
		t.Fatalf("unexpected error %+v", e)
	}
	for range 3 {
		if _, err := s.Reserve(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Reserve()
	if e := wantError(t, err, http.StatusTooManyRequests); e.RetryAfter != 30 || e.Message != "Audio storage full" {
		t.Fatalf("unexpected error %+v", e)
	}
	if used(s) != 5+5+3*1024 {
		t.Fatalf("used = %d", used(s))
	}
}

func TestFinishedJobsLeaveTheQueueBound(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxPendingJobs = 1 })
	job := submit(t, s)
	claim := mustClaim(t, s)
	if err := s.Finish(claim.ID, claim.Token, result, "", false); err != nil {
		t.Fatal(err)
	}
	if next := submit(t, s); next.ID == job.ID {
		t.Fatal("expected a new job")
	}
}

func TestCancelFencesWorkerAndCleansAudio(t *testing.T) {
	s, _ := newStore(t, nil)
	job := submit(t, s)
	claim := mustClaim(t, s)
	if err := s.Delete(job.ID); err != nil {
		t.Fatal(err)
	}
	wantError(t, s.Heartbeat(job.ID, claim.Token), http.StatusConflict)
	wantError(t, s.Finish(job.ID, claim.Token, result, "", false), http.StatusConflict)
	_, err := s.Get(job.ID)
	wantError(t, err, http.StatusNotFound)
	wantError(t, s.Delete(job.ID), http.StatusNotFound)
	_, err = s.Submit(job.UploadID, nil)
	wantError(t, err, http.StatusBadRequest)
	if names := blobs(t, s); len(names) != 0 || used(s) != 0 || len(s.Counts()) != 0 {
		t.Fatalf("leftovers: files %v, used %d, counts %v", names, used(s), s.Counts())
	}
}

func TestRetentionAndJobAge(t *testing.T) {
	s, c := newStore(t, nil)
	job := submit(t, s)
	claim := mustClaim(t, s)
	if err := s.Finish(job.ID, claim.Token, result, "", false); err != nil {
		t.Fatal(err)
	}
	c.Advance(60 * time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if status(t, s, job.ID) != StatusCompleted {
		t.Fatal("job removed before its retention elapsed")
	}
	c.Advance(time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(job.ID)
	wantError(t, err, http.StatusNotFound)
	if names := blobs(t, s); len(names) != 0 {
		t.Fatalf("files left: %v", names)
	}

	queued := submit(t, s)
	processing := submit(t, s)
	mustClaim(t, s)
	c.Advance(21600*time.Second + time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{queued.ID, processing.ID} {
		got, _ := s.Get(id)
		if got.Status != StatusError || *got.Error != "Job age limit exceeded" {
			t.Fatalf("unexpected job %+v", got)
		}
	}
	if s.Claim() != nil || len(blobs(t, s)) != 0 || used(s) != 0 {
		t.Fatal("aged jobs kept their audio or stayed claimable")
	}
}

func TestCleanupRemovesCrashOrphansButPreservesRecentFiles(t *testing.T) {
	s, _ := newStore(t, nil)
	stale := filepath.Join(s.blobs, "crash-orphan")
	recent := filepath.Join(s.blobs, "recent-upload")
	for _, path := range []string{stale, recent} {
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stale, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	// Old files that belong to a live upload stay.
	job := submit(t, s)
	if err := os.Chtimes(s.BlobPath(job.UploadID), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale orphan kept")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("recent file removed")
	}
	if !exists(s, job.UploadID) {
		t.Fatal("live upload removed")
	}
}

func TestUnsubmittedUploadsExpire(t *testing.T) {
	s, c := newStore(t, nil)
	uid, err := s.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.BlobPath(uid), []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.UploadReady(uid, 5); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	// Retention is 60 s, so the 3700 s floor applies.
	c.Advance(3700 * time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if !exists(s, uid) || used(s) != 5+1024 {
		t.Fatal("upload expired too early")
	}
	c.Advance(time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if exists(s, uid) || used(s) != 0 {
		t.Fatal("unsubmitted uploads kept")
	}
	_, err = s.Submit(uid, nil)
	wantError(t, err, http.StatusBadRequest)
	wantError(t, s.UploadReady(pending, 5), http.StatusBadRequest)
}

func TestLongRetentionKeepsUnsubmittedUploads(t *testing.T) {
	s, c := newStore(t, func(c *Config) { c.Retention = 2 * time.Hour })
	if _, err := s.Reserve(); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour + 101*time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if used(s) != 1024 {
		t.Fatal("upload expired before retention")
	}
	c.Advance(time.Hour)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if used(s) != 0 {
		t.Fatal("upload kept past retention")
	}
}

func TestRestartWipesAudioAndQueue(t *testing.T) {
	c := &clock{now: time.Now()}
	cfg := config(t.TempDir(), c)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	job := submit(t, s)
	if err := os.Mkdir(filepath.Join(s.blobs, "leftover-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Counts()) != 0 || len(blobs(t, restarted)) != 0 {
		t.Fatal("restart kept state")
	}
	_, err = restarted.Get(job.ID)
	wantError(t, err, http.StatusNotFound)
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "jobs.sqlite3")); !os.IsNotExist(err) {
		t.Fatal("unexpected database file")
	}
}

func TestFinishedJobsReleaseAudioButStayIdempotent(t *testing.T) {
	s, _ := newStore(t, nil)
	job := submit(t, s)
	claim := mustClaim(t, s)
	if claim.Bytes != 5 {
		t.Fatalf("bytes = %d", claim.Bytes)
	}
	// A result always completes the job, even if a worker also asks for a retry.
	if err := s.Finish(claim.ID, claim.Token, result, "", true); err != nil {
		t.Fatal(err)
	}
	if status(t, s, job.ID) != StatusCompleted {
		t.Fatal("job not completed")
	}
	if exists(s, job.UploadID) || used(s) != 0 {
		t.Fatal("completed job kept its audio")
	}
	again, err := s.Submit(job.UploadID, map[string]string{})
	if err != nil || again.ID != job.ID || again.Status != StatusCompleted {
		t.Fatalf("resubmit = %+v, %v", again, err)
	}
	wantError(t, s.Finish(claim.ID, claim.Token, result, "", false), http.StatusConflict)
}

func TestFailedJobsReleaseAudio(t *testing.T) {
	s, c := newStore(t, func(c *Config) { c.MaxAttempts = 1 })
	failed, expired, aged := submit(t, s), submit(t, s), submit(t, s)
	claim := mustClaim(t, s)
	if claim.ID != failed.ID {
		t.Fatal("unexpected claim order")
	}
	if err := s.Finish(claim.ID, claim.Token, nil, "Audio could not be decoded", false); err != nil {
		t.Fatal(err)
	}
	if claim = mustClaim(t, s); claim.ID != expired.ID {
		t.Fatal("unexpected claim order")
	}
	s.mu.Lock()
	s.jobs[aged.ID].created = c.Now().Add(-21601 * time.Second)
	s.mu.Unlock()
	c.Advance(15 * time.Second)
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if s.Claim() != nil {
		t.Fatal("no job should be claimable")
	}
	messages := map[string]string{
		failed.ID:  "Audio could not be decoded",
		expired.ID: "Worker lease expired",
		aged.ID:    "Job age limit exceeded",
	}
	for _, job := range []Job{failed, expired, aged} {
		got, _ := s.Get(job.ID)
		if got.Status != StatusError || got.Error == nil || *got.Error != messages[job.ID] {
			t.Fatalf("unexpected job %+v", got)
		}
		if exists(s, job.UploadID) {
			t.Fatal("failed job kept its audio")
		}
	}
	if used(s) != 0 {
		t.Fatalf("used = %d", used(s))
	}
	if counts := s.Counts(); len(counts) != 1 || counts["error"] != 3 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestIdempotentSubmit(t *testing.T) {
	s, _ := newStore(t, nil)
	uid, _ := s.Reserve()
	_, err := s.Submit(uid, nil)
	wantError(t, err, http.StatusBadRequest) // not ready yet
	if err := s.UploadReady(uid, 5); err != nil {
		t.Fatal(err)
	}
	options := map[string]string{"language_code": "de"}
	job, err := s.Submit(uid, options)
	if err != nil {
		t.Fatal(err)
	}
	options["language_code"] = "en" // callers cannot mutate stored options
	again, err := s.Submit(uid, map[string]string{"language_code": "de"})
	if err != nil || again.ID != job.ID || again.Options["language_code"] != "de" {
		t.Fatalf("resubmit = %+v, %v", again, err)
	}
	_, err = s.Submit(uid, nil)
	if e := wantError(t, err, http.StatusConflict); e.Message != "Upload already submitted with different options" {
		t.Fatalf("unexpected message %q", e.Message)
	}
	_, err = s.Submit(uid, options)
	wantError(t, err, http.StatusConflict)
	if counts := s.Counts(); counts["queued"] != 1 || len(counts) != 1 {
		t.Fatalf("counts = %v", counts)
	}
	_, err = s.Submit("missing", nil)
	wantError(t, err, http.StatusBadRequest)
}

func TestDiscardUpload(t *testing.T) {
	s, _ := newStore(t, nil)
	uid, _ := s.Reserve()
	f, err := s.CreateBlob(uid)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := s.CreateBlob(uid); err == nil {
		t.Fatal("CreateBlob overwrote an existing file")
	}
	s.DiscardUpload(uid)
	if exists(s, uid) || used(s) != 0 {
		t.Fatal("discarded upload kept")
	}
	// A submitted upload is owned by its job.
	job := submit(t, s)
	s.DiscardUpload(job.UploadID)
	if !exists(s, job.UploadID) || status(t, s, job.ID) != StatusQueued {
		t.Fatal("discard removed a submitted upload")
	}
	s.DiscardUpload("never-reserved")
}

func TestAudioMissingFile(t *testing.T) {
	s, _ := newStore(t, nil)
	job := submit(t, s)
	claim := mustClaim(t, s)
	if err := os.Remove(s.BlobPath(job.UploadID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Audio(claim.ID, claim.Token)
	if e := wantError(t, err, http.StatusConflict); e.Message != "Audio no longer available" {
		t.Fatalf("unexpected message %q", e.Message)
	}
}

func TestConcurrentOperations(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxStorageBytes = 1 << 20; c.MaxPendingJobs = 1000 })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				uid, err := s.Reserve()
				if err == nil {
					err = os.WriteFile(s.BlobPath(uid), []byte("audio"), 0o600)
				}
				if err == nil {
					err = s.UploadReady(uid, 5)
				}
				if err == nil {
					_, err = s.Submit(uid, nil)
				}
				if err != nil {
					t.Error(err)
				}
			}
		})
		wg.Go(func() {
			for range 40 {
				if claim := s.Claim(); claim != nil {
					if f, err := s.Audio(claim.ID, claim.Token); err == nil {
						f.Close()
					}
					s.Finish(claim.ID, claim.Token, result, "", false)
				}
				s.Counts()
			}
		})
		wg.Go(func() {
			for range 5 {
				s.Cleanup()
			}
		})
	}
	wg.Wait()
	for claim := s.Claim(); claim != nil; claim = s.Claim() {
		if err := s.Finish(claim.ID, claim.Token, result, "", false); err != nil {
			t.Fatal(err)
		}
	}
	if counts := s.Counts(); counts["completed"] != 160 || len(counts) != 1 || used(s) != 0 || len(blobs(t, s)) != 0 {
		t.Fatalf("counts %v, used %d, files %v", counts, used(s), blobs(t, s))
	}
}

func TestNewUUID(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 1000 {
		id := newUUID()
		if !pattern.MatchString(id) || seen[id] {
			t.Fatalf("bad uuid %q", id)
		}
		seen[id] = true
	}
}
