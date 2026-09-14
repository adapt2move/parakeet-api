package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/adapt2move/parakeet-api/internal/formats"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newStore(t *testing.T, change func(*Config)) (*Store, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := Config{DataDir: t.TempDir(), MaxUploadBytes: 1 << 20, MaxStorageBytes: 8 << 20, MaxPendingJobs: 8,
		Retention: time.Hour, MaxJobAge: 6 * time.Hour, Lease: time.Minute, MaxAttempts: 2, Now: c.Now}
	if change != nil {
		change(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func mustSave(t *testing.T, s *Store, data string) string {
	t.Helper()
	id, err := s.Save(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func queue(t *testing.T, s *Store, language string) Job {
	t.Helper()
	job, err := s.Submit(mustSave(t, s, "audio"), language)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func files(t *testing.T, s *Store) int {
	t.Helper()
	entries, err := os.ReadDir(s.blobs)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func status(t *testing.T, s *Store, id string) Status {
	t.Helper()
	job, err := s.Get(id)
	if err != nil {
		return ""
	}
	return job.Status
}

var result = &formats.Result{Text: "", Words: []formats.Word{}, AudioDurationMS: 1, Chunks: 1}

func TestNewWipesAudio(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "audio", "nested"), 0o700)
	os.WriteFile(filepath.Join(dir, "audio", "leftover"), []byte("x"), 0o600)
	s, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if n := files(t, s); n != 0 {
		t.Fatalf("%d files left", n)
	}
}

func TestSave(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxUploadBytes, c.MaxStorageBytes = 100<<10, 250<<10 })
	if _, err := s.Save(strings.NewReader("")); err != ErrEmpty {
		t.Fatalf("empty: %v", err)
	}
	if _, err := s.Save(strings.NewReader(strings.Repeat("x", 100<<10+1))); err != ErrTooLarge {
		t.Fatalf("too large: %v", err)
	}
	readErr := errors.New("client went away")
	if _, err := s.Save(io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(readErr))); err != readErr {
		t.Fatalf("source error: %v", err)
	}
	if n := files(t, s); n != 0 || s.used != 0 {
		t.Fatalf("failed uploads left %d files and %d bytes", n, s.used)
	}

	// Tiny uploads are charged 64 KiB; a reservation needs MaxUploadBytes free.
	for range 2 {
		mustSave(t, s, "a")
	}
	big := mustSave(t, s, strings.Repeat("x", 100<<10))
	if s.used != 2*minUploadCharge+100<<10 {
		t.Fatalf("used %d", s.used)
	}
	if _, err := s.Save(strings.NewReader("a")); err != errStorageFull {
		t.Fatalf("full: %v", err)
	}
	s.DiscardUpload(big)
	mustSave(t, s, "a")
	if n := files(t, s); n != 3 {
		t.Fatalf("%d files", n)
	}
}

func TestSubmit(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxPendingJobs = 1 })
	id := mustSave(t, s, "audio")
	job, err := s.Submit(id, "de")
	if err != nil || job.Status != StatusQueued || job.Language != "de" || job.UploadID != id {
		t.Fatalf("%+v %v", job, err)
	}
	if again, err := s.Submit(id, "de"); err != nil || again.ID != job.ID {
		t.Fatalf("resubmit: %+v %v", again, err)
	}
	if _, err := s.Submit(id, ""); err != errOptions {
		t.Fatalf("other language: %v", err)
	}
	waiting := mustSave(t, s, "audio")
	if _, err := s.Submit(waiting, ""); err != errQueueFull {
		t.Fatalf("queue full: %v", err)
	}
	claim := s.Claim()
	if _, err := s.Submit(waiting, ""); err != errQueueFull {
		t.Fatalf("processing jobs are pending: %v", err)
	}
	// A finished job released its audio but stays idempotent.
	s.Finish(claim.ID, claim.Token, result, "", false)
	if again, err := s.Submit(id, "de"); err != nil || again.Status != StatusCompleted {
		t.Fatalf("finished resubmit: %+v %v", again, err)
	}
	if _, err := s.Submit(waiting, ""); err != nil {
		t.Fatal(err)
	}
	s.Delete(job.ID)
	if _, err := s.Submit(id, "de"); err != errUploadGone {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := s.Submit("unknown", ""); err != errUploadGone {
		t.Fatalf("unknown: %v", err)
	}
}

func TestClaimOrderAndPayload(t *testing.T) {
	s, _ := newStore(t, nil)
	first, second := queue(t, s, "fr"), queue(t, s, "")
	a, b := s.Claim(), s.Claim()
	if a.ID != first.ID || b.ID != second.ID || s.Claim() != nil {
		t.Fatalf("order: %+v %+v", a, b)
	}
	if a.LeaseSeconds != 60 || a.Bytes != 5 || a.Options["language_code"] != "fr" || len(b.Options) != 0 || a.Token == b.Token {
		t.Fatalf("payload: %+v %+v", a, b)
	}
}

func TestLeases(t *testing.T) {
	s, c := newStore(t, nil)
	job := queue(t, s, "")
	claim := s.Claim()
	for _, token := range []string{"", "wrong"} {
		if s.Heartbeat(job.ID, token) != errLease || s.Finish(job.ID, token, result, "", false) != errLease {
			t.Fatalf("token %q was accepted", token)
		}
		if _, err := s.Audio(job.ID, token); err != errLease {
			t.Fatalf("audio with token %q: %v", token, err)
		}
	}
	f, err := s.Audio(job.ID, claim.Token)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	// A heartbeat extends the lease; it ends exactly at its deadline.
	c.Advance(59 * time.Second)
	if err := s.Heartbeat(job.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	c.Advance(59 * time.Second)
	if s.Claim() != nil || status(t, s, job.ID) != StatusProcessing {
		t.Fatal("lease expired early")
	}
	c.Advance(time.Second)
	if s.Heartbeat(job.ID, claim.Token) != errLease {
		t.Fatal("expired lease accepted")
	}
	retry := s.Claim()
	if retry == nil || retry.ID != job.ID || retry.Token == claim.Token {
		t.Fatalf("expired lease was not reassigned: %+v", retry)
	}
	if s.Finish(job.ID, claim.Token, result, "", false) != errLease {
		t.Fatal("old token accepted")
	}

	// The second expiry exhausts MaxAttempts.
	c.Advance(time.Minute)
	if s.Claim() != nil {
		t.Fatal("job claimed beyond MaxAttempts")
	}
	if got, _ := s.Get(job.ID); got.Status != StatusError || got.Error != "Worker lease expired" || files(t, s) != 0 {
		t.Fatalf("%+v, %d files", got, files(t, s))
	}
}

func TestFinish(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int // attempts that end with retry before the last call
		result   *formats.Result
		message  string
		retry    bool
		want     Status
	}{
		{"result", 0, result, "", false, StatusCompleted},
		{"result with retry", 0, result, "", true, StatusCompleted},
		{"error", 0, nil, "Audio could not be decoded", false, StatusError},
		{"retry", 0, nil, "Transient", true, StatusQueued},
		{"retry exhausted", 1, nil, "Transient", true, StatusError},
	} {
		s, _ := newStore(t, nil)
		job := queue(t, s, "")
		for range tc.attempts {
			claim := s.Claim()
			s.Finish(claim.ID, claim.Token, nil, "Transient", true)
		}
		claim := s.Claim()
		if err := s.Finish(claim.ID, claim.Token, tc.result, tc.message, tc.retry); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, _ := s.Get(job.ID)
		audioKept := tc.want == StatusQueued
		if got.Status != tc.want || (files(t, s) == 1) != audioKept || (s.used > 0) != audioKept {
			t.Errorf("%s: %+v, %d files, %d bytes", tc.name, got, files(t, s), s.used)
		}
		if tc.want == StatusError && got.Error != tc.message || tc.want == StatusCompleted && got.Result != result {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}

func TestDelete(t *testing.T) {
	s, _ := newStore(t, nil)
	job := queue(t, s, "")
	claim := s.Claim()
	if err := s.Delete(job.ID); err != nil || files(t, s) != 0 || s.used != 0 || s.Counts()["processing"] != 0 {
		t.Fatalf("delete: %v", err)
	}
	if s.Heartbeat(job.ID, claim.Token) != errLease || s.Delete(job.ID) != errJobGone {
		t.Fatal("deleted job still reachable")
	}
}

func TestCleanup(t *testing.T) {
	s, c := newStore(t, func(c *Config) { c.MaxJobAge, c.Lease = 2*time.Hour, 3*time.Hour })
	old := queue(t, s, "")
	s.Claim()
	c.Advance(90 * time.Minute)
	done := queue(t, s, "")
	claim := s.Claim()
	s.Finish(claim.ID, claim.Token, result, "", false)
	unsubmitted := mustSave(t, s, "audio")

	c.Advance(31 * time.Minute) // old is 2 h 1 min old
	s.Cleanup()
	if got, _ := s.Get(old.ID); got.Status != StatusError || got.Error != "Job age limit exceeded" {
		t.Fatalf("age limit: %+v", got)
	}
	if status(t, s, done.ID) != StatusCompleted {
		t.Fatal("completed job removed before its retention")
	}

	c.Advance(30 * time.Minute) // done finished 61 min ago; the upload waits for 3700 s
	s.Cleanup()
	if status(t, s, done.ID) != "" || status(t, s, old.ID) != StatusError || s.uploads[unsubmitted] == nil {
		t.Fatal("retention")
	}
	c.Advance(2 * time.Minute)
	s.Cleanup()
	if s.uploads[unsubmitted] != nil || files(t, s) != 0 || s.used != 0 {
		t.Fatalf("unsubmitted upload kept: %d files, %d bytes", files(t, s), s.used)
	}
}

func TestLongRetentionKeepsUnsubmittedUploads(t *testing.T) {
	s, c := newStore(t, func(c *Config) { c.Retention = 3 * time.Hour })
	id := mustSave(t, s, "audio")
	c.Advance(3 * time.Hour)
	s.Cleanup()
	if _, err := s.Submit(id, ""); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanSweep(t *testing.T) {
	s, c := newStore(t, nil)
	c.now = time.Now()
	kept := mustSave(t, s, "audio")
	for name, age := range map[string]time.Duration{"old-orphan": staleAge + time.Second, "new-orphan": staleAge, kept: 2 * staleAge} {
		path := s.path(name)
		os.WriteFile(path, []byte("x"), 0o600)
		os.Chtimes(path, c.now.Add(-age), c.now.Add(-age))
	}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"old-orphan": false, "new-orphan": true, kept: true} {
		if _, err := os.Stat(s.path(name)); (err == nil) != want {
			t.Errorf("%s: exists=%v", name, err == nil)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	s, _ := newStore(t, func(c *Config) { c.MaxPendingJobs, c.MaxStorageBytes = 100, 64<<20 })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				id, err := s.Save(strings.NewReader("audio"))
				if err == nil {
					_, err = s.Submit(id, "")
				}
				if err != nil {
					t.Error(err)
				}
				s.Cleanup()
			}
		})
	}
	claimed := make(chan string, 80)
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if c := s.Claim(); c != nil {
					claimed <- c.ID
					s.Finish(c.ID, c.Token, result, "", false)
				}
				s.Counts()
			}
		})
	}
	wg.Wait()
	close(claimed)
	seen := map[string]bool{}
	for id := range claimed {
		if seen[id] {
			t.Fatalf("job %s claimed twice", id)
		}
		seen[id] = true
	}
}
