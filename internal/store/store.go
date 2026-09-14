// Package store keeps the job queue in process memory and audio blobs on disk.
//
// Nothing survives a restart: New wipes the audio directory. One mutex guards
// all queue state; files are only touched after the state change is done, so a
// crash in between leaves a file that the orphan sweep in Cleanup removes.
package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status is the lifecycle state of a job.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusError      Status = "error"
)

// staleAge bounds how long unsubmitted uploads and unknown files may live.
// It exceeds the one hour transfer limit of an upload.
const staleAge = 3700 * time.Second

// minUploadCharge is what a stored upload costs at least, capped at MaxUploadBytes. Every upload
// takes a file and an entry in memory for an hour, so tiny uploads must not be nearly free.
const minUploadCharge = 64 * 1024

// Config holds the limits the queue enforces.
type Config struct {
	DataDir         string // audio blobs live in DataDir/audio
	MaxUploadBytes  int64  // reserved per upload until its real size is known
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
	errStorageFull  = &Error{Status: http.StatusTooManyRequests, Message: "Audio storage full", RetryAfter: 30}
	errQueueFull    = &Error{Status: http.StatusTooManyRequests, Message: "Queue full", RetryAfter: 10}
	errUploadGone   = &Error{Status: http.StatusBadRequest, Message: "Upload missing or expired"}
	errOptions      = &Error{Status: http.StatusConflict, Message: "Upload already submitted with different options"}
	errJobGone      = &Error{Status: http.StatusNotFound, Message: "Transcript missing or expired"}
	errLease        = &Error{Status: http.StatusConflict, Message: "Lease expired or job cancelled"}
	errAudioMissing = &Error{Status: http.StatusConflict, Message: "Audio no longer available"}
)

// Job is a snapshot of a job. Result is shared with the store and must not be modified.
// The store never looks into a result; the caller decides what it holds.
type Job struct {
	ID       string
	UploadID string
	Status   Status
	Options  map[string]string
	Result   any     // set only when completed
	Error    *string // nil unless the job failed
	Created  time.Time
	Updated  time.Time
	Attempts int
}

// Claim is the worker's view of a leased job, serialized as the claim response.
type Claim struct {
	ID           string            `json:"id"`
	Token        string            `json:"token"`
	LeaseSeconds int               `json:"lease_seconds"`
	Bytes        int64             `json:"bytes"`
	Options      map[string]string `json:"options"`
}

type upload struct {
	id      string
	bytes   int64 // storage charged: the reservation, the charge of the stored file, or 0 once the job finished
	size    int64 // stored file size
	ready   bool
	created time.Time
	job     *job
}

type job struct {
	id       string
	seq      uint64 // breaks ties between jobs created at the same instant
	upload   *upload
	status   Status
	options  map[string]string
	result   any
	err      *string
	created  time.Time
	updated  time.Time
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
	active  map[string]*job // queued or processing
	counts  map[Status]int
	used    int64
	seq     uint64
}

// New prepares DataDir/audio and deletes every file in it, since an in-memory
// queue cannot recover uploads of a previous process. The caller must hold the
// DataDir lock first so a second process cannot wipe a running instance's audio.
func New(cfg Config) (*Store, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	blobs := filepath.Join(cfg.DataDir, "audio")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		return nil, fmt.Errorf("create audio directory: %w", err)
	}
	entries, err := os.ReadDir(blobs)
	if err != nil {
		return nil, fmt.Errorf("read audio directory: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(blobs, entry.Name())); err != nil {
			return nil, fmt.Errorf("wipe audio directory: %w", err)
		}
	}
	return &Store{
		cfg:     cfg,
		blobs:   blobs,
		uploads: map[string]*upload{},
		jobs:    map[string]*job{},
		active:  map[string]*job{},
		counts:  map[Status]int{},
	}, nil
}

// BlobPath returns the audio file path of an upload.
func (s *Store) BlobPath(uploadID string) string {
	return filepath.Join(s.blobs, uploadID)
}

// CreateBlob creates the audio file of a freshly reserved upload.
func (s *Store) CreateBlob(uploadID string) (*os.File, error) {
	return os.OpenFile(s.BlobPath(uploadID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// Reserve books MaxUploadBytes of storage for a new upload and returns its ID.
func (s *Store) Reserve() (string, error) {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+s.cfg.MaxUploadBytes > s.cfg.MaxStorageBytes {
		return "", errStorageFull
	}
	id := newUUID()
	for s.uploads[id] != nil {
		id = newUUID()
	}
	s.uploads[id] = &upload{id: id, bytes: s.cfg.MaxUploadBytes, created: now}
	s.used += s.cfg.MaxUploadBytes
	return id, nil
}

// UploadReady replaces the reservation with the charge for the stored size, at least
// min(64 KiB, MaxUploadBytes), and allows submission.
func (s *Store) UploadReady(uploadID string, size int64) error {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[uploadID]
	if u == nil || u.job != nil {
		return errUploadGone
	}
	charge := max(size, min(minUploadCharge, s.cfg.MaxUploadBytes))
	s.used += charge - u.bytes
	u.bytes, u.size, u.ready, u.created = charge, size, true, now
	return nil
}

// DiscardUpload drops an upload that no job references and deletes its file.
func (s *Store) DiscardUpload(uploadID string) {
	s.mu.Lock()
	u := s.uploads[uploadID]
	if u != nil && u.job != nil {
		s.mu.Unlock()
		return
	}
	if u != nil {
		s.removeUpload(u)
	}
	s.mu.Unlock()
	s.unlink(uploadID)
}

// Submit queues a job for a ready upload. Submitting the same upload again with
// equal options returns the existing job.
func (s *Store) Submit(uploadID string, options map[string]string) (Job, error) {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[uploadID]
	if u != nil && u.job != nil {
		if !maps.Equal(u.job.options, options) {
			return Job{}, errOptions
		}
		return u.job.snapshot(), nil
	}
	if u == nil || !u.ready {
		return Job{}, errUploadGone
	}
	if len(s.active) >= s.cfg.MaxPendingJobs {
		return Job{}, errQueueFull
	}
	opts := maps.Clone(options)
	if opts == nil {
		opts = map[string]string{}
	}
	s.seq++
	j := &job{id: newUUID(), seq: s.seq, upload: u, options: opts, created: now, updated: now}
	for s.jobs[j.id] != nil {
		j.id = newUUID()
	}
	u.job = j
	s.jobs[j.id] = j
	s.setStatus(j, StatusQueued)
	return j.snapshot(), nil
}

// Get returns a snapshot of a job.
func (s *Store) Get(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return Job{}, errJobGone
	}
	return j.snapshot(), nil
}

// Claim expires stale leases and leases the oldest queued job, or returns nil.
func (s *Store) Claim() *Claim {
	now := s.cfg.Now()
	var released []string
	s.mu.Lock()
	for _, j := range s.active {
		if j.status != StatusProcessing || j.lease.After(now) {
			continue
		}
		j.token, j.lease, j.updated = "", time.Time{}, now
		if j.attempts >= s.cfg.MaxAttempts {
			released = append(released, s.fail(j, "Worker lease expired"))
		} else {
			j.err = nil
			s.setStatus(j, StatusQueued)
		}
	}
	var next *job
	for _, j := range s.active {
		if j.status == StatusQueued && (next == nil || j.created.Before(next.created) ||
			j.created.Equal(next.created) && j.seq < next.seq) {
			next = j
		}
	}
	var claim *Claim
	if next != nil {
		next.token = newUUID()
		next.lease = now.Add(s.cfg.Lease)
		next.attempts++
		next.updated = now
		s.setStatus(next, StatusProcessing)
		claim = &Claim{
			ID:           next.id,
			Token:        next.token,
			LeaseSeconds: int(s.cfg.Lease / time.Second),
			Bytes:        next.upload.size,
			Options:      maps.Clone(next.options),
		}
	}
	s.mu.Unlock()
	s.unlink(released...)
	return claim
}

// Heartbeat extends a valid lease.
func (s *Store) Heartbeat(id, token string) error {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.fenced(id, token, now)
	if err != nil {
		return err
	}
	j.lease = now.Add(s.cfg.Lease)
	return nil
}

// Audio opens the audio of a leased job. The caller closes the file.
func (s *Store) Audio(id, token string) (*os.File, error) {
	s.mu.Lock()
	j, err := s.fenced(id, token, s.cfg.Now())
	var uploadID string
	if j != nil {
		uploadID = j.upload.id
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.BlobPath(uploadID))
	if err != nil {
		return nil, errAudioMissing
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errAudioMissing
	}
	return f, nil
}

// Finish ends a leased attempt. A non-nil result completes the job even when
// retry is set. Otherwise the job is queued again if retry is set and attempts
// remain, or it fails with message. The store keeps result as it is.
func (s *Store) Finish(id, token string, result any, message string, retry bool) error {
	now := s.cfg.Now()
	s.mu.Lock()
	j, err := s.fenced(id, token, now)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	j.token, j.lease, j.updated = "", time.Time{}, now
	var released string
	switch {
	case result != nil:
		j.result, j.err = result, nil
		s.setStatus(j, StatusCompleted)
		released = s.release(j)
	case retry && j.attempts < s.cfg.MaxAttempts:
		j.err = nil
		s.setStatus(j, StatusQueued)
	default:
		released = s.fail(j, message)
	}
	s.mu.Unlock()
	s.unlink(released)
	return nil
}

// Delete removes a job, its upload and its audio. A worker holding its lease gets 409.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	j := s.jobs[id]
	if j == nil {
		s.mu.Unlock()
		return errJobGone
	}
	uploadID := j.upload.id
	s.removeJob(j)
	s.mu.Unlock()
	s.unlink(uploadID)
	return nil
}

// Cleanup fails jobs past MaxJobAge, drops finished jobs past Retention and
// unsubmitted uploads past max(Retention, 3700 s), and deletes audio files that
// no live upload owns and that are older than 3700 s.
func (s *Store) Cleanup() error {
	now := s.cfg.Now()
	var remove []string
	s.mu.Lock()
	ageLimit := now.Add(-s.cfg.MaxJobAge)
	for _, j := range s.active {
		if j.created.Before(ageLimit) {
			j.token, j.lease, j.updated = "", time.Time{}, now
			remove = append(remove, s.fail(j, "Job age limit exceeded"))
		}
	}
	retained := now.Add(-s.cfg.Retention)
	for _, j := range s.jobs {
		if (j.status == StatusCompleted || j.status == StatusError) && j.updated.Before(retained) {
			remove = append(remove, j.upload.id)
			s.removeJob(j)
		}
	}
	unused := now.Add(-max(s.cfg.Retention, staleAge))
	for _, u := range s.uploads {
		if u.job == nil && u.created.Before(unused) {
			remove = append(remove, u.id)
			s.removeUpload(u)
		}
	}
	known := make(map[string]struct{}, len(s.uploads))
	for id, u := range s.uploads {
		if u.bytes > 0 {
			known[id] = struct{}{}
		}
	}
	s.mu.Unlock()
	s.unlink(remove...)

	entries, err := os.ReadDir(s.blobs)
	if err != nil {
		return fmt.Errorf("read audio directory: %w", err)
	}
	var errs []error
	orphan := now.Add(-staleAge)
	for _, entry := range entries {
		if _, ok := known[entry.Name()]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(orphan) {
			continue
		}
		if err := os.Remove(filepath.Join(s.blobs, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, errors.New("remove orphaned audio file"))
		}
	}
	return errors.Join(errs...)
}

// Counts returns the number of jobs per status, omitting empty statuses.
func (s *Store) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.counts))
	for status, n := range s.counts {
		if n > 0 {
			counts[string(status)] = n
		}
	}
	return counts
}

func (s *Store) fenced(id, token string, now time.Time) (*job, error) {
	j := s.jobs[id]
	if j == nil || j.status != StatusProcessing || token == "" ||
		subtle.ConstantTimeCompare([]byte(j.token), []byte(token)) != 1 || !j.lease.After(now) {
		return nil, errLease
	}
	return j, nil
}

func (s *Store) setStatus(j *job, status Status) {
	if j.status != "" {
		s.counts[j.status]--
	}
	j.status = status
	s.counts[status]++
	if status == StatusQueued || status == StatusProcessing {
		s.active[j.id] = j
	} else {
		delete(s.active, j.id)
	}
}

// fail marks a job as failed and returns the upload whose audio is released.
func (s *Store) fail(j *job, message string) string {
	j.result, j.err = nil, &message
	s.setStatus(j, StatusError)
	return s.release(j)
}

// release frees the storage of a finished job. The upload stays so that
// re-submitting it stays idempotent; the caller unlinks the returned file.
func (s *Store) release(j *job) string {
	s.used -= j.upload.bytes
	j.upload.bytes = 0
	return j.upload.id
}

func (s *Store) removeJob(j *job) {
	s.counts[j.status]--
	delete(s.active, j.id)
	delete(s.jobs, j.id)
	s.removeUpload(j.upload)
}

func (s *Store) removeUpload(u *upload) {
	s.used -= u.bytes
	delete(s.uploads, u.id)
}

func (s *Store) unlink(uploadIDs ...string) {
	for _, id := range uploadIDs {
		if id != "" {
			os.Remove(s.BlobPath(id))
		}
	}
}

func (j *job) snapshot() Job {
	var err *string
	if j.err != nil {
		message := *j.err
		err = &message
	}
	return Job{
		ID:       j.id,
		UploadID: j.upload.id,
		Status:   j.status,
		Options:  maps.Clone(j.options),
		Result:   j.result,
		Error:    err,
		Created:  j.created,
		Updated:  j.updated,
		Attempts: j.attempts,
	}
}

// newUUID returns a random RFC 4122 version 4 UUID in canonical form.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
