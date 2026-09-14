package api

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adapt2move/parakeet-api/internal/store"
)

// Settings is the process configuration, read from the environment.
type Settings struct {
	store.Config
	APIKey        string
	WorkerKey     string
	PublicURL     string // without trailing slashes
	SyncTimeout   time.Duration
	UploadSlots   int
	UploadIdle    time.Duration // also bounds each response write
	UploadRate    int64         // minimum average bytes per second once UploadIdle has passed
	AudioURLHosts []string
	ListenAddr    string
}

// maxBytes caps the byte limits, so that sums of limits and sizes cannot overflow.
const maxBytes = 1 << 50

// LoadSettings reads and validates the settings. getenv is os.Getenv outside tests; empty values
// mean the default.
func LoadSettings(getenv func(string) string) (Settings, error) {
	p := parser{getenv: getenv}
	if mode := p.str("DB_MODE", ""); mode != "" && mode != "memory" {
		return Settings{}, errors.New("DB_MODE must be empty or memory: the queue lives in memory only")
	}
	s := Settings{
		DataDir:         p.str("DATA_DIR", "/data"),
		APIKey:          p.str("API_KEY", ""),
		WorkerKey:       p.str("WORKER_API_KEY", ""),
		PublicURL:       strings.TrimRight(p.str("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		MaxUploadBytes:  p.int("MAX_UPLOAD_BYTES", 128<<20, maxBytes),
		MaxStorageBytes: p.int("MAX_STORAGE_BYTES", 2<<30, maxBytes),
		MaxPendingJobs:  int(p.int("MAX_PENDING_JOBS", 32, math.MaxInt32)),
		Retention:       p.seconds("RETENTION_SECONDS", 3600),
		MaxJobAge:       p.seconds("MAX_JOB_AGE_SECONDS", 21600),
		Lease:           p.seconds("LEASE_SECONDS", 90),
		MaxAttempts:     int(p.int("MAX_ATTEMPTS", 3, math.MaxInt32)),
		SyncTimeout:     p.seconds("SYNC_TIMEOUT_SECONDS", 1800),
		UploadSlots:     int(p.int("MAX_CONCURRENT_UPLOADS", 2, math.MaxInt32)),
		UploadIdle:      p.floatSeconds("UPLOAD_IDLE_SECONDS", 15),
		UploadRate:      p.int("MIN_UPLOAD_BYTES_PER_SECOND", 64<<10, math.MaxInt64),
		ListenAddr:      p.str("LISTEN_ADDR", ":8080"),
	}
	for host := range strings.SplitSeq(p.str("AUDIO_URL_HOSTS", ""), ",") {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		if strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-_") != "" {
			p.fail("AUDIO_URL_HOSTS must list bare hostnames, without scheme, port or path")
		}
		s.AudioURLHosts = append(s.AudioURLHosts, host)
	}
	switch {
	case p.err != nil:
		return Settings{}, p.err
	case utf8.RuneCountInString(s.APIKey) < 32 || utf8.RuneCountInString(s.WorkerKey) < 32 || s.APIKey == s.WorkerKey:
		return Settings{}, errors.New("API_KEY and WORKER_API_KEY must be distinct and at least 32 characters long")
	case s.MaxStorageBytes < s.MaxUploadBytes:
		return Settings{}, errors.New("MAX_STORAGE_BYTES must be at least MAX_UPLOAD_BYTES")
	case s.Lease < 15*time.Second:
		return Settings{}, errors.New("LEASE_SECONDS must be at least 15")
	}
	return s, nil
}

// parser reads variables and keeps the first error. Messages name the variable, never its value.
type parser struct {
	getenv func(string) string
	err    error
}

func (p *parser) fail(format string, args ...any) {
	if p.err == nil {
		p.err = fmt.Errorf(format, args...)
	}
}

func (p *parser) str(name, fallback string) string {
	if value := strings.TrimSpace(p.getenv(name)); value != "" {
		return value
	}
	return fallback
}

// int reads a positive integer of at most limit.
func (p *parser) int(name string, fallback, limit int64) int64 {
	n, err := strconv.ParseInt(p.str(name, strconv.FormatInt(fallback, 10)), 10, 64)
	if err != nil || n <= 0 || n > limit {
		p.fail("%s must be a positive integer up to %d", name, limit)
	}
	return n
}

// seconds reads a positive number of whole seconds, up to about 68 years.
func (p *parser) seconds(name string, fallback int64) time.Duration {
	return time.Duration(p.int(name, fallback, math.MaxInt32)) * time.Second
}

func (p *parser) floatSeconds(name string, fallback float64) time.Duration {
	f, err := strconv.ParseFloat(p.str(name, strconv.FormatFloat(fallback, 'f', -1, 64)), 64)
	if err != nil || !(f > 0 && f <= math.MaxInt32) {
		p.fail("%s must be a positive number of seconds", name)
	}
	return max(time.Duration(f*float64(time.Second)), 1)
}
