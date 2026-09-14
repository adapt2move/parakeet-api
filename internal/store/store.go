// Package store keeps the job queue in process memory and audio blobs on disk.
//
// Nothing survives a restart: New wipes the audio directory. One mutex guards all queue state.
// Audio files are removed after the state change that releases them; a file left behind by a
// failed removal is deleted by the orphan sweep in Cleanup.
package store

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/adapt2move/parakeet-api/internal/formats"
)

// Status is the lifecycle state of a job.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusError      Status = "error"
)

// staleAge bounds how long unsubmitted uploads and unknown files live. It exceeds the one hour
// limit of an upload transfer.
const staleAge = 3700 * time.Second

// minUploadCharge is the least storage an upload is charged, capped at MaxUploadBytes, so that
// tiny uploads are not nearly free.
const minUploadCharge = 64 << 10

// Config holds the limits the queue enforces.
type Config struct {
	DataDir         string // audio blobs live in DataDir/audio
	MaxUploadBytes  int64  // reserved per upload while it is written
	MaxStorageBytes int64  // sum of reserved and stored audio
	MaxPendingJobs  int    // queued plus processing jobs
	Retention       time.Duration
	MaxJobAge       time.Duration
	Lease           time.Duration
	MaxAttempts     int
	Now             func() time.Time // defaults to time.Now
}

// Error is a client-safe failure with an HTTP status.
type Error struct {
	Status     int
	Message    string
	RetryAfter int // seconds; 0 means no Retry-After header
}

func (e *Error) Error() string { return e.Message }

var (
	ErrTooLarge     = &Error{Status: http.StatusRequestEntityTooLarge, Message: "Audio too large"}
	ErrEmpty        = &Error{Status: http.StatusBadRequest, Message: "Audio is empty"}
	errStorageFull  = &Error{Status: http.StatusTooManyRequests, Message: "Audio storage full", RetryAfter: 30}
	errQueueFull    = &Error{Status: http.StatusTooManyRequests, Message: "Queue full", RetryAfter: 10}
	errUploadGone   = &Error{Status: http.StatusBadRequest, Message: "Upload missing or expired"}
	errOptions      = &Error{Status: http.StatusConflict, Message: "Upload already submitted with different options"}
	errJobGone      = &Error{Status: http.StatusNotFound, Message: "Transcript missing or expired"}
	errLease        = &Error{Status: http.StatusConflict, Message: "Lease expired or job cancelled"}
	errAudioMissing = &Error{Status: http.StatusConflict, Message: "Audio no longer available"}
)

// Job is a snapshot of a job. Result is shared and must not be modified.
type Job struct {
	ID       string
	UploadID string
	Status   Status
	Language string          // empty when unset
	Result   *formats.Result // set once completed
	Error    string          // the failure message when Status is StatusError
}

// Claim is a leased job as the worker receives it.
type Claim struct {
	ID           string            `json:"id"`
	Token        string            `json:"token"`
	LeaseSeconds int               `json:"lease_seconds"`
	Bytes        int64             `json:"bytes"`
	Options      map[string]string `json:"options"`
}

type upload struct {
	id      string
	bytes   int64 // storage charged; 0 once the job finished and its audio is gone
	size    int64
	created time.Time
	job     *job
}

type job struct {
	Job
	seq      uint64 // claim order
	upload   *upload
	updated  time.Time
	created  time.Time
	token    string
	lease    time.Time
	attempts int
}

// Store is the in-memory queue. It is safe for concurrent use.
type Store struct {
	cfg   Config
	blobs string

	mu      sync.Mutex
	uploads map[string]*upload
	jobs    map[string]*job
	counts  map[Status]int
	used    int64
	seq     uint64
}

// New prepares DataDir/audio and deletes everything in it, since an in-memory queue cannot
// recover the uploads of a previous process. The caller must hold the DataDir lock.
func New(cfg Config) (*Store, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	blobs := filepath.Join(cfg.DataDir, "audio")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		return nil, fmt.Errorf("create audio directory: %w", err)
	}
	entries, err := os.ReadDir(blobs)
	for _, entry := range entries {
		if err == nil {
			err = os.RemoveAll(filepath.Join(blobs, entry.Name()))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("wipe audio directory: %w", err)
	}
	return &Store{cfg: cfg, blobs: blobs, uploads: map[string]*upload{}, jobs: map[string]*job{}, counts: map[Status]int{}}, nil
}

// Save stores src as a new upload and returns its ID. It reads at most one byte beyond
// MaxUploadBytes. Errors from src are returned unchanged; nothing of a failed upload remains.
func (s *Store) Save(src io.Reader) (string, error) {
	s.mu.Lock()
	if s.used+s.cfg.MaxUploadBytes > s.cfg.MaxStorageBytes {
		s.mu.Unlock()
		return "", errStorageFull
	}
	s.used += s.cfg.MaxUploadBytes
	s.mu.Unlock()

	id := newUUID()
	size, err := s.write(id, src)
	switch {
	case err != nil:
	case size == 0:
		err = ErrEmpty
	case size > s.cfg.MaxUploadBytes:
		err = ErrTooLarge
	}
	if err != nil {
		os.Remove(s.path(id))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used -= s.cfg.MaxUploadBytes
	if err != nil {
		return "", err
	}
	u := &upload{id: id, bytes: max(size, min(minUploadCharge, s.cfg.MaxUploadBytes)), size: size, created: s.cfg.Now()}
	s.used += u.bytes
	s.uploads[id] = u
	return id, nil
}

func (s *Store) write(id string, src io.Reader) (int64, error) {
	f, err := os.OpenFile(s.path(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	size, err := io.Copy(f, io.LimitReader(src, s.cfg.MaxUploadBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return size, err
}

// DiscardUpload drops an upload that no job references, with its file.
func (s *Store) DiscardUpload(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.uploads[id]; u != nil && u.job == nil {
		s.removeUpload(u)
		os.Remove(s.path(id))
	}
}

// Submit queues a job for an upload. Submitting the upload again with the same language returns
// the existing job.
func (s *Store) Submit(uploadID, language string) (Job, error) {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[uploadID]
	switch {
	case u == nil:
		return Job{}, errUploadGone
	case u.job != nil && u.job.Language != language:
		return Job{}, errOptions
	case u.job != nil:
		return u.job.Job, nil
	case s.counts[StatusQueued]+s.counts[StatusProcessing] >= s.cfg.MaxPendingJobs:
		return Job{}, errQueueFull
	}
	s.seq++
	j := &job{Job: Job{ID: newUUID(), UploadID: u.id, Language: language}, seq: s.seq, upload: u, created: now}
	u.job = j
	s.jobs[j.ID] = j
	s.setStatus(j, StatusQueued)
	return j.Job, nil
}

// Get returns a snapshot of a job.
func (s *Store) Get(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobs[id]; j != nil {
		return j.Job, nil
	}
	return Job{}, errJobGone
}

// Claim expires stale leases and leases the oldest queued job, or returns nil.
func (s *Store) Claim() *Claim {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var next *job
	for _, j := range s.jobs {
		if j.Status == StatusProcessing && !j.lease.After(now) {
			if j.attempts >= s.cfg.MaxAttempts {
				s.fail(j, "Worker lease expired", now)
				continue
			}
			s.setStatus(j, StatusQueued)
		}
		if j.Status == StatusQueued && (next == nil || j.seq < next.seq) {
			next = j
		}
	}
	if next == nil {
		return nil
	}
	s.setStatus(next, StatusProcessing)
	next.token, next.lease = newUUID(), now.Add(s.cfg.Lease)
	next.attempts++
	options := map[string]string{}
	if next.Language != "" {
		options["language_code"] = next.Language
	}
	return &Claim{ID: next.ID, Token: next.token, LeaseSeconds: int(s.cfg.Lease / time.Second), Bytes: next.upload.size, Options: options}
}

// Heartbeat extends a valid lease.
func (s *Store) Heartbeat(id, token string) error {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, token, now)
	if err == nil {
		j.lease = now.Add(s.cfg.Lease)
	}
	return err
}

// Audio opens the audio of a leased job. The caller closes the file.
func (s *Store) Audio(id, token string) (*os.File, error) {
	s.mu.Lock()
	j, err := s.leased(id, token, s.cfg.Now())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(j.UploadID))
	if err != nil {
		return nil, errAudioMissing
	}
	return f, nil
}

// Finish ends a leased attempt. A result completes the job, even with retry set. Otherwise the
// job is queued again when retry is set and attempts remain, or it fails with message.
func (s *Store) Finish(id, token string, result *formats.Result, message string, retry bool) error {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, token, now)
	if err != nil {
		return err
	}
	switch {
	case result != nil:
		j.Result = result
		s.setStatus(j, StatusCompleted)
		s.release(j, now)
	case retry && j.attempts < s.cfg.MaxAttempts:
		s.setStatus(j, StatusQueued)
	default:
		s.fail(j, message, now)
	}
	return nil
}

// Delete removes a job, its upload and its audio. A worker holding the lease is fenced out.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return errJobGone
	}
	s.removeJob(j)
	os.Remove(s.path(j.UploadID))
	return nil
}

// Cleanup fails jobs older than MaxJobAge, drops finished jobs untouched for Retention and
// unsubmitted uploads older than max(Retention, 3700 s), and deletes audio files that no upload
// owns once they are 3700 s old.
func (s *Store) Cleanup() error {
	now := s.cfg.Now()
	s.mu.Lock()
	for _, j := range s.jobs {
		switch {
		case (j.Status == StatusQueued || j.Status == StatusProcessing) && now.Sub(j.created) > s.cfg.MaxJobAge:
			s.fail(j, "Job age limit exceeded", now)
		case (j.Status == StatusCompleted || j.Status == StatusError) && now.Sub(j.updated) > s.cfg.Retention:
			s.removeJob(j)
		}
	}
	known := map[string]bool{}
	for id, u := range s.uploads {
		if u.job == nil && now.Sub(u.created) > max(s.cfg.Retention, staleAge) {
			s.removeUpload(u)
			os.Remove(s.path(id))
		} else if u.bytes > 0 {
			known[id] = true
		}
	}
	s.mu.Unlock()

	entries, err := os.ReadDir(s.blobs)
	if err != nil {
		return fmt.Errorf("read audio directory: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		info, err := entry.Info()
		if known[entry.Name()] || err != nil || now.Sub(info.ModTime()) <= staleAge {
			continue
		}
		if err := os.Remove(filepath.Join(s.blobs, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, errors.New("remove orphaned audio file failed"))
		}
	}
	return errors.Join(errs...)
}

// Counts returns the number of jobs per status, omitting empty statuses.
func (s *Store) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for status, n := range s.counts {
		if n > 0 {
			counts[string(status)] = n
		}
	}
	return counts
}

func (s *Store) path(uploadID string) string { return filepath.Join(s.blobs, uploadID) }

func (s *Store) leased(id, token string, now time.Time) (*job, error) {
	j := s.jobs[id]
	if j == nil || j.Status != StatusProcessing || subtle.ConstantTimeCompare([]byte(j.token), []byte(token)) != 1 || !j.lease.After(now) {
		return nil, errLease
	}
	return j, nil
}

// setStatus moves a job to status and ends any lease it held.
func (s *Store) setStatus(j *job, status Status) {
	if j.Status != "" {
		s.counts[j.Status]--
	}
	s.counts[status]++
	j.Status, j.token, j.lease = status, "", time.Time{}
}

func (s *Store) fail(j *job, message string, now time.Time) {
	j.Error = message
	s.setStatus(j, StatusError)
	s.release(j, now)
}

// release deletes the audio of a finished job. The upload stays so that re-submitting it
// returns the job.
func (s *Store) release(j *job, now time.Time) {
	j.updated = now
	s.used -= j.upload.bytes
	j.upload.bytes = 0
	os.Remove(s.path(j.UploadID))
}

func (s *Store) removeJob(j *job) {
	s.counts[j.Status]--
	delete(s.jobs, j.ID)
	s.removeUpload(j.upload)
}

func (s *Store) removeUpload(u *upload) {
	s.used -= u.bytes
	delete(s.uploads, u.id)
}

// newUUID returns a random version 4 UUID in canonical lowercase form.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
