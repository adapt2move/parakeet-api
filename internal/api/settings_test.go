package api

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	clientKey = "client-" + strings.Repeat("x", 32)
	workerKey = "worker-" + strings.Repeat("y", 32)
)

func env(values map[string]string) func(string) string {
	base := map[string]string{"API_KEY": clientKey, "WORKER_API_KEY": workerKey}
	return func(name string) string {
		if value, ok := values[name]; ok {
			return value
		}
		return base[name]
	}
}

func TestSettingsDefaults(t *testing.T) {
	s, err := LoadSettings(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{DataDir: "/data", APIKey: clientKey, WorkerKey: workerKey, PublicURL: "http://localhost:8080",
		MaxUploadBytes: 128 << 20, MaxStorageBytes: 2 << 30, MaxPendingJobs: 32, Retention: time.Hour,
		MaxJobAge: 6 * time.Hour, Lease: 90 * time.Second, MaxAttempts: 3, SyncTimeout: 30 * time.Minute,
		UploadSlots: 2, UploadIdle: 15 * time.Second, UploadRate: 64 << 10, ListenAddr: ":8080"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got  %+v\nwant %+v", s, want)
	}
}

func TestSettingsErrors(t *testing.T) {
	for _, tc := range []struct {
		env     map[string]string
		mention string
	}{
		{map[string]string{"MAX_UPLOAD_BYTES": "lots"}, "MAX_UPLOAD_BYTES"},
		{map[string]string{"MAX_UPLOAD_BYTES": "0x10"}, "MAX_UPLOAD_BYTES"},
		{map[string]string{"MAX_STORAGE_BYTES": "0"}, "MAX_STORAGE_BYTES"},
		{map[string]string{"MAX_UPLOAD_BYTES": "9223372036854775807", "MAX_STORAGE_BYTES": "9223372036854775807"}, "MAX_UPLOAD_BYTES"},
		{map[string]string{"MAX_PENDING_JOBS": "1.5"}, "MAX_PENDING_JOBS"},
		{map[string]string{"MAX_PENDING_JOBS": "99999999999"}, "MAX_PENDING_JOBS"},
		{map[string]string{"RETENTION_SECONDS": "-1"}, "RETENTION_SECONDS"},
		{map[string]string{"MAX_JOB_AGE_SECONDS": "1e3"}, "MAX_JOB_AGE_SECONDS"},
		{map[string]string{"MAX_ATTEMPTS": "0"}, "MAX_ATTEMPTS"},
		{map[string]string{"SYNC_TIMEOUT_SECONDS": "9999999999999"}, "SYNC_TIMEOUT_SECONDS"},
		{map[string]string{"MAX_CONCURRENT_UPLOADS": "0"}, "MAX_CONCURRENT_UPLOADS"},
		{map[string]string{"UPLOAD_IDLE_SECONDS": "soon"}, "UPLOAD_IDLE_SECONDS"},
		{map[string]string{"UPLOAD_IDLE_SECONDS": "NaN"}, "UPLOAD_IDLE_SECONDS"},
		{map[string]string{"UPLOAD_IDLE_SECONDS": "0"}, "UPLOAD_IDLE_SECONDS"},
		{map[string]string{"MIN_UPLOAD_BYTES_PER_SECOND": "0"}, "MIN_UPLOAD_BYTES_PER_SECOND"},
		{map[string]string{"LEASE_SECONDS": "14"}, "LEASE_SECONDS"},
		{map[string]string{"MAX_UPLOAD_BYTES": "4096", "MAX_STORAGE_BYTES": "4095"}, "MAX_STORAGE_BYTES"},
		{map[string]string{"API_KEY": ""}, "API_KEY"},
		{map[string]string{"API_KEY": strings.Repeat("x", 31)}, "API_KEY"},
		{map[string]string{"WORKER_API_KEY": strings.Repeat("é", 31)}, "WORKER_API_KEY"},
		{map[string]string{"WORKER_API_KEY": clientKey}, "WORKER_API_KEY"},
		{map[string]string{"DB_MODE": "file"}, "DB_MODE"},
		{map[string]string{"DB_MODE": "sqlite"}, "DB_MODE"},
		{map[string]string{"AUDIO_URL_HOSTS": "https://audio.example"}, "AUDIO_URL_HOSTS"},
		{map[string]string{"AUDIO_URL_HOSTS": "audio.example:443"}, "AUDIO_URL_HOSTS"},
		{map[string]string{"AUDIO_URL_HOSTS": "ok.example,audio.example/a"}, "AUDIO_URL_HOSTS"},
		{map[string]string{"AUDIO_URL_HOSTS": "user@audio.example"}, "AUDIO_URL_HOSTS"},
		{map[string]string{"AUDIO_URL_HOSTS": "[::1]"}, "AUDIO_URL_HOSTS"},
		{map[string]string{"AUDIO_URL_HOSTS": "audio .example"}, "AUDIO_URL_HOSTS"},
		{map[string]string{"AUDIO_URL_HOSTS": "audío.example"}, "AUDIO_URL_HOSTS"},
	} {
		_, err := LoadSettings(env(tc.env))
		if err == nil {
			t.Errorf("%v: accepted", tc.env)
			continue
		}
		if !strings.Contains(err.Error(), tc.mention) || strings.Contains(err.Error(), clientKey) {
			t.Errorf("%v: %v", tc.env, err)
		}
		for _, value := range tc.env {
			if len(value) > 1 && strings.Contains(err.Error(), value) {
				t.Errorf("%v: the error repeats the value: %v", tc.env, err)
			}
		}
	}
}
