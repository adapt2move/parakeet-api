package api

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(overrides map[string]string) func(string) (string, bool) {
	values := map[string]string{
		"API_KEY":        "client-" + strings.Repeat("x", 32),
		"WORKER_API_KEY": "worker-" + strings.Repeat("y", 32),
	}
	for k, v := range overrides {
		values[k] = v
	}
	return func(name string) (string, bool) {
		v, ok := values[name]
		if ok && v == "<unset>" {
			return "", false
		}
		return v, ok
	}
}

func TestSettingsDefaults(t *testing.T) {
	s, err := LoadSettings(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		DataDir:         "/data",
		APIKey:          "client-" + strings.Repeat("x", 32),
		WorkerKey:       "worker-" + strings.Repeat("y", 32),
		PublicURL:       "http://localhost:8080",
		MaxUploadBytes:  128 << 20,
		MaxStorageBytes: 2 << 30,
		MaxPendingJobs:  32,
		Retention:       time.Hour,
		MaxJobAge:       6 * time.Hour,
		Lease:           90 * time.Second,
		MaxAttempts:     3,
		SyncTimeout:     30 * time.Minute,
		UploadSlots:     2,
		UploadIdle:      15 * time.Second,
		UploadRate:      64 * 1024,
		ListenAddr:      ":8080",
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", s, want)
	}
}

func TestSettingsParsing(t *testing.T) {
	s, err := LoadSettings(env(map[string]string{
		"PUBLIC_BASE_URL":             "https://api.example//",
		"MAX_UPLOAD_BYTES":            " +1_024\n",
		"MAX_STORAGE_BYTES":           "4096",
		"UPLOAD_IDLE_SECONDS":         "0.25",
		"LEASE_SECONDS":               "15",
		"RETENTION_SECONDS":           "99999999999999",
		"AUDIO_URL_HOSTS":             " Audio.EXAMPLE , ,cdn.example, ",
		"LISTEN_ADDR":                 "127.0.0.1:9000",
		"DB_MODE":                     "memory",
		"DATA_DIR":                    "/tmp/x",
		"MIN_UPLOAD_BYTES_PER_SECOND": "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicURL != "https://api.example" || s.MaxUploadBytes != 1024 || s.UploadIdle != 250*time.Millisecond ||
		s.Lease != 15*time.Second || s.Retention != maxDuration || s.ListenAddr != "127.0.0.1:9000" || s.DataDir != "/tmp/x" {
		t.Fatalf("unexpected settings %+v", s)
	}
	if !reflect.DeepEqual(s.AudioURLHosts, []string{"audio.example", "cdn.example"}) {
		t.Fatalf("hosts %q", s.AudioURLHosts)
	}
	if cfg := s.StoreConfig(); cfg.Lease != s.Lease || cfg.MaxStorageBytes != 4096 || cfg.DataDir != "/tmp/x" {
		t.Fatalf("store config %+v", cfg)
	}
	for _, mode := range []string{"", "memory"} {
		if _, err := LoadSettings(env(map[string]string{"DB_MODE": mode})); err != nil {
			t.Errorf("DB_MODE=%q: %v", mode, err)
		}
	}
}

func TestSettingsValidation(t *testing.T) {
	cases := map[string]struct {
		env     map[string]string
		message string
	}{
		"file mode":              {map[string]string{"DB_MODE": "file"}, "DB_MODE file was removed"},
		"sqlite mode":            {map[string]string{"DB_MODE": "sqlite"}, "DB_MODE file was removed"},
		"missing client key":     {map[string]string{"API_KEY": "<unset>"}, "distinct API_KEY"},
		"short worker key":       {map[string]string{"WORKER_API_KEY": strings.Repeat("y", 31)}, "distinct API_KEY"},
		"equal keys":             {map[string]string{"WORKER_API_KEY": "client-" + strings.Repeat("x", 32)}, "distinct API_KEY"},
		"key counts code points": {map[string]string{"API_KEY": strings.Repeat("é", 31)}, "distinct API_KEY"},
		"zero upload":            {map[string]string{"MAX_UPLOAD_BYTES": "0"}, "Limits must be positive"},
		"negative retention":     {map[string]string{"RETENTION_SECONDS": "-1"}, "Limits must be positive"},
		"zero slots":             {map[string]string{"MAX_CONCURRENT_UPLOADS": "0"}, "Limits must be positive"},
		"zero idle":              {map[string]string{"UPLOAD_IDLE_SECONDS": "0.0"}, "Limits must be positive"},
		"zero rate":              {map[string]string{"MIN_UPLOAD_BYTES_PER_SECOND": "0"}, "Limits must be positive"},
		"storage below upload":   {map[string]string{"MAX_UPLOAD_BYTES": "4096", "MAX_STORAGE_BYTES": "4095"}, "Storage must fit"},
		"short lease":            {map[string]string{"LEASE_SECONDS": "14"}, "lease must be at least 15"},
		"empty number":           {map[string]string{"MAX_PENDING_JOBS": ""}, "MAX_PENDING_JOBS must be an integer"},
		"words":                  {map[string]string{"MAX_UPLOAD_BYTES": "lots"}, "MAX_UPLOAD_BYTES must be an integer"},
		"float for int":          {map[string]string{"MAX_ATTEMPTS": "1.5"}, "MAX_ATTEMPTS must be an integer"},
		"hex":                    {map[string]string{"MAX_ATTEMPTS": "0x10"}, "MAX_ATTEMPTS must be an integer"},
		"leading underscore":     {map[string]string{"MAX_ATTEMPTS": "_3"}, "MAX_ATTEMPTS must be an integer"},
		"double underscore":      {map[string]string{"MAX_ATTEMPTS": "1__0"}, "MAX_ATTEMPTS must be an integer"},
		"int overflow":           {map[string]string{"MAX_UPLOAD_BYTES": "99999999999999999999"}, "64-bit range"},
		"nan idle":               {map[string]string{"UPLOAD_IDLE_SECONDS": "nan"}, "finite number"},
		"infinite idle":          {map[string]string{"UPLOAD_IDLE_SECONDS": "inf"}, "finite number"},
		"host scheme":            {map[string]string{"AUDIO_URL_HOSTS": "https://a.example"}, "bare hostnames"},
		"host port":              {map[string]string{"AUDIO_URL_HOSTS": "a.example:443"}, "bare hostnames"},
		"host userinfo":          {map[string]string{"AUDIO_URL_HOSTS": "u@a.example"}, "bare hostnames"},
		"host brackets":          {map[string]string{"AUDIO_URL_HOSTS": "[::1]"}, "bare hostnames"},
		"host percent":           {map[string]string{"AUDIO_URL_HOSTS": "a%2e.example"}, "bare hostnames"},
		"host non-ascii":         {map[string]string{"AUDIO_URL_HOSTS": "ok.example,audío.example"}, "bare hostnames"},
		"host inner space":       {map[string]string{"AUDIO_URL_HOSTS": "a .example"}, "bare hostnames"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSettings(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("got %v, want an error containing %q", err, tc.message)
			}
			for key, value := range tc.env {
				if key != "DB_MODE" && len(value) > 3 && value != "<unset>" && strings.Contains(err.Error(), value) {
					t.Fatalf("error %q repeats the value", err)
				}
			}
		})
	}
}

func TestSettingsCheckKeysBeforeLimitsAndHostsLast(t *testing.T) {
	_, err := LoadSettings(env(map[string]string{"API_KEY": "short", "MAX_ATTEMPTS": "0", "AUDIO_URL_HOSTS": "a:1"}))
	if err == nil || !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("got %v", err)
	}
	_, err = LoadSettings(env(map[string]string{"MAX_ATTEMPTS": "0", "AUDIO_URL_HOSTS": "a:1"}))
	if err == nil || !strings.Contains(err.Error(), "Limits") {
		t.Fatalf("got %v", err)
	}
}
