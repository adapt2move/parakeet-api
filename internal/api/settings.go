package api

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/adapt2move/parakeet-api/internal/store"
)

// maxDuration caps configured durations so that arithmetic on them cannot overflow.
const maxDuration = time.Duration(math.MaxInt64 / 4)

// Settings is the process configuration, read from the environment.
type Settings struct {
	DataDir         string
	APIKey          string
	WorkerKey       string
	PublicURL       string // without trailing slashes
	MaxUploadBytes  int64
	MaxStorageBytes int64
	MaxPendingJobs  int
	Retention       time.Duration
	MaxJobAge       time.Duration
	Lease           time.Duration
	MaxAttempts     int
	SyncTimeout     time.Duration
	UploadSlots     int
	UploadIdle      time.Duration
	UploadRate      float64 // minimum average bytes per second once UploadIdle has passed
	AudioURLHosts   []string
	ListenAddr      string
}

// LoadSettings reads and validates the settings. lookup is os.LookupEnv outside tests.
func LoadSettings(lookup func(string) (string, bool)) (Settings, error) {
	get := func(name, fallback string) string {
		if value, ok := lookup(name); ok {
			return value
		}
		return fallback
	}
	if mode := get("DB_MODE", ""); mode != "" && mode != "memory" {
		return Settings{}, errors.New("DB_MODE file was removed: the queue lives in memory only; unset DB_MODE or set it to memory")
	}
	p := envParser{get: get}
	s := Settings{
		DataDir:         get("DATA_DIR", "/data"),
		APIKey:          get("API_KEY", ""),
		WorkerKey:       get("WORKER_API_KEY", ""),
		PublicURL:       strings.TrimRight(get("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		MaxUploadBytes:  p.int("MAX_UPLOAD_BYTES", 128*1024*1024),
		MaxStorageBytes: p.int("MAX_STORAGE_BYTES", 2*1024*1024*1024),
		MaxPendingJobs:  int(p.int("MAX_PENDING_JOBS", 32)),
		Retention:       p.seconds("RETENTION_SECONDS", 3600),
		MaxJobAge:       p.seconds("MAX_JOB_AGE_SECONDS", 21600),
		Lease:           p.seconds("LEASE_SECONDS", 90),
		MaxAttempts:     int(p.int("MAX_ATTEMPTS", 3)),
		SyncTimeout:     p.seconds("SYNC_TIMEOUT_SECONDS", 1800),
		UploadSlots:     int(p.int("MAX_CONCURRENT_UPLOADS", 2)),
		UploadIdle:      p.floatSeconds("UPLOAD_IDLE_SECONDS", 15),
		UploadRate:      float64(p.int("MIN_UPLOAD_BYTES_PER_SECOND", 64*1024)),
		ListenAddr:      get("LISTEN_ADDR", ""),
	}
	if p.err != nil {
		return Settings{}, p.err
	}
	if s.DataDir == "" {
		s.DataDir = "."
	}
	if s.ListenAddr == "" {
		s.ListenAddr = ":8080"
	}
	bareHosts := true
	for _, host := range strings.Split(get("AUDIO_URL_HOSTS", ""), ",") {
		// Hostnames compare case-insensitively; tolerate "a.example, b.example".
		host = strings.TrimFunc(host, pySpace)
		if host == "" {
			continue
		}
		bareHosts = bareHosts && isASCII(host) && !strings.ContainsAny(host, "/:@[]?#% ")
		s.AudioURLHosts = append(s.AudioURLHosts, strings.ToLower(host))
	}
	if min(utf8.RuneCountInString(s.APIKey), utf8.RuneCountInString(s.WorkerKey)) < 32 || s.APIKey == s.WorkerKey {
		return Settings{}, errors.New("Set distinct API_KEY and WORKER_API_KEY values of at least 32 characters")
	}
	if s.MaxUploadBytes <= 0 || s.MaxStorageBytes <= 0 || s.MaxPendingJobs <= 0 || s.Retention <= 0 ||
		s.MaxJobAge <= 0 || s.Lease <= 0 || s.MaxAttempts <= 0 || s.SyncTimeout <= 0 || s.UploadSlots <= 0 ||
		s.UploadIdle <= 0 || s.UploadRate <= 0 {
		return Settings{}, errors.New("Limits must be positive")
	}
	if s.MaxStorageBytes < s.MaxUploadBytes || s.Lease < 15*time.Second {
		return Settings{}, errors.New("Storage must fit an upload; lease must be at least 15 seconds")
	}
	if !bareHosts {
		return Settings{}, errors.New("AUDIO_URL_HOSTS must list bare hostnames, without scheme, port or path")
	}
	return s, nil
}

// StoreConfig returns the queue limits for store.New.
func (s Settings) StoreConfig() store.Config {
	return store.Config{
		DataDir:         s.DataDir,
		MaxUploadBytes:  s.MaxUploadBytes,
		MaxStorageBytes: s.MaxStorageBytes,
		MaxPendingJobs:  s.MaxPendingJobs,
		Retention:       s.Retention,
		MaxJobAge:       s.MaxJobAge,
		Lease:           s.Lease,
		MaxAttempts:     s.MaxAttempts,
	}
}

// envParser parses numbers the way Python's int() and float() accepted them and keeps the first
// error. Messages name the variable but never repeat its value.
type envParser struct {
	get func(name, fallback string) string
	err error
}

func (p *envParser) fail(name, kind string) {
	if p.err == nil {
		p.err = fmt.Errorf("%s must be %s", name, kind)
	}
}

func (p *envParser) int(name string, fallback int64) int64 {
	raw := p.get(name, strconv.FormatInt(fallback, 10))
	digits, ok := pyNumber(raw, false)
	if !ok {
		p.fail(name, "an integer")
		return 0
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		p.fail(name, "an integer within the 64-bit range")
		return 0
	}
	return n
}

// seconds reads whole seconds. Huge values are capped; they mean "practically never".
func (p *envParser) seconds(name string, fallback int64) time.Duration {
	n := p.int(name, fallback)
	if n > int64(maxDuration/time.Second) {
		return maxDuration
	}
	return time.Duration(n) * time.Second
}

func (p *envParser) floatSeconds(name string, fallback float64) time.Duration {
	raw := p.get(name, strconv.FormatFloat(fallback, 'f', -1, 64))
	text, ok := pyNumber(raw, true)
	f, err := strconv.ParseFloat(text, 64)
	if !ok || err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		p.fail(name, "a finite number")
		return 0
	}
	if f*1e9 >= float64(maxDuration) {
		return maxDuration
	}
	d := time.Duration(f * 1e9)
	if f > 0 && d <= 0 {
		d = 1 // positive but below a nanosecond
	}
	return d
}

// pyNumber strips Python whitespace and single underscores between digits, and rejects syntax
// that Go's parsers accept but Python's int() or float() did not (hex, "0x", "p" exponents).
func pyNumber(raw string, float bool) (string, bool) {
	s := strings.TrimFunc(raw, pySpace)
	if s == "" {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '_':
			if i == 0 || i == len(s)-1 || !isDigit(s[i-1]) || !isDigit(s[i+1]) {
				return "", false
			}
		case c == '+' || c == '-':
			b.WriteByte(c)
		case float && (c == '.' || c == 'e' || c == 'E'):
			b.WriteByte(c)
		case float && strings.ContainsRune("infatyINFATY", rune(c)):
			b.WriteByte(c)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// pySpace matches Python's str.isspace(), which str.strip() and int() use.
func pySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
