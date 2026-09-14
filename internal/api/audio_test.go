package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTrustedURL(t *testing.T) {
	hosts := []string{"audio.example"}
	for raw, want := range map[string]error{
		"https://audio.example/a.wav":              nil,
		"https://AUDIO.example:443/a.wav?x=1":      nil,
		"https://audio.example":                    nil,
		"http://audio.example/a.wav":               errUntrustedURL,
		"ftp://audio.example/a.wav":                errUntrustedURL,
		"//audio.example/a.wav":                    errUntrustedURL,
		"audio.example/a.wav":                      errUntrustedURL,
		"https:audio.example/a.wav":                errUntrustedURL,
		"https://evil.example/a.wav":               errUntrustedURL,
		"https://audio.example.evil.example/a.wav": errUntrustedURL,
		"https://audio.example:8443/a.wav":         errUntrustedURL,
		"https://audio.example:0443/a.wav":         errUntrustedURL,
		"https://user@audio.example/a.wav":         errUntrustedURL,
		"https://:secret@audio.example/a.wav":      errUntrustedURL,
		"https://audio.example@evil.example/a.wav": errUntrustedURL,
		"https://audio.example/a.wav#fragment":     errUntrustedURL,
		"https://[::1]/a.wav":                      errUntrustedURL,
		"https://audio.example:99999999999999/a":   errUntrustedURL,
		"https://audio.example:https/a":            errInvalidAudioURL,
		"https://evil.example\\@audio.example/a":   errInvalidAudioURL,
		"https://audio.example/%zz":                errInvalidAudioURL,
	} {
		if _, err := trustedURL(raw, hosts); err != want {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestResolveAudio(t *testing.T) {
	a, _ := newServer(t, nil)
	id, err := a.store.Save(strings.NewReader("audio"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := a.cfg.PublicURL + "/uploads/"
	for raw, want := range map[string]error{
		prefix + id:                              nil,
		prefix + strings.ToUpper(id):             errInvalidUploadURL,
		prefix + strings.ReplaceAll(id, "-", ""): errInvalidUploadURL,
		prefix + id + "/extra":                   errInvalidUploadURL,
		prefix + id + "?download=1":              errInvalidUploadURL,
		prefix:                                   errInvalidUploadURL,
		"https://audio.example/\x00":             errInvalidAudioURL,
		"https://audio.example/a\x7f":            errInvalidAudioURL,
		"https://audio.example/a\n.wav":          errInvalidAudioURL,
		"https://audio\t.example/a.wav":          errInvalidAudioURL,
		"https://audio.example/a b.wav":          errInvalidAudioURL,
		" https://audio.example/a.wav":           errInvalidAudioURL,
		"https://audío.example/a.wav":            errInvalidAudioURL,
		"https://audio.example/\xff":             errInvalidAudioURL,
		prefix + id + "\n":                       errInvalidAudioURL,
	} {
		got, owned, err := a.resolveAudio(context.Background(), raw)
		if err != want || owned || (err == nil && got != id) {
			t.Errorf("%q: %q %v %v", raw, got, owned, err)
		}
	}
}

func TestDownload(t *testing.T) {
	a, _ := newServer(t, func(s *Settings) { s.UploadIdle, s.UploadRate = 200*time.Millisecond, 1000 })
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("audio")) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 4097)) })
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		time.Sleep(time.Second)
	})
	mux.HandleFunc("/trickle", func(w http.ResponseWriter, r *http.Request) {
		for range 20 {
			w.Write([]byte("a"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	transport := srv.Client().Transport.(*http.Transport)
	a.client.Transport.(*http.Transport).TLSClientConfig = transport.TLSClientConfig

	for path, ok := range map[string]bool{"/ok": true, "/empty": false, "/large": false, "/missing": false,
		"/redirect": false, "/stall": false, "/trickle": false} {
		u, _ := url.Parse(srv.URL + path)
		id, err := a.download(context.Background(), u)
		if ok != (err == nil) || (err != nil && err != errDownloadFailed) {
			t.Errorf("%s: %q %v", path, id, err)
		}
	}
}
