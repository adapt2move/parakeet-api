package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestTrustedURL(t *testing.T) {
	hosts := []string{"audio.example", "v1.x"}
	cases := []struct {
		url, target string
		err         *apiError
	}{
		{"https://audio.example/a.wav", "https://audio.example/a.wav", nil},
		{"HTTPS://Audio.EXAMPLE:443/a.wav?x=1", "https://audio.example/a.wav?x=1", nil},
		{"https://audio.example:0443/a", "https://audio.example/a", nil},
		{"https://audio.example:/a", "https://audio.example/a", nil},
		{"https://audio.example", "https://audio.example/", nil},
		{"https://audio.example?x=1", "https://audio.example/?x=1", nil},
		{"https://audio.example/a#", "https://audio.example/a", nil},
		{"http://audio.example/a", "", errUntrustedURL},
		{"https:audio.example/a", "", errUntrustedURL},
		{"//audio.example/a", "", errUntrustedURL},
		{"1https://audio.example/a", "", errUntrustedURL},
		{"https://audio.example.evil/a", "", errUntrustedURL},
		{"https://audio.example:8443/a", "", errUntrustedURL},
		{"https://audio.example/a#frag", "", errUntrustedURL},
		{"https://user@audio.example/a", "", errUntrustedURL},
		{"https://@audio.example/a", "", errUntrustedURL},
		{"https://:@audio.example/a", "", errUntrustedURL},
		{`https://evil\@audio.example/a`, "", errUntrustedURL},
		{"https://audio.example@evil/a", "", errUntrustedURL},
		{"https://[v1.x]/a", "", errUntrustedURL},
		{"https://[::1]/a", "", errUntrustedURL},
		{"https://audio.example:x/a", "", errInvalidOptions},
		{"https://audio.example:65536/a", "", errInvalidOptions},
		{"https://audio.example:1:2/a", "", errInvalidOptions},
		{"https://[audio.example]/a", "", errInvalidOptions},
		{"https://[1.2.3.4]/a", "", errInvalidOptions},
		{"https://a[::1]/a", "", errInvalidOptions},
		{"https://[::1]x/a", "", errInvalidOptions},
		{"https://[::1/a", "", errInvalidOptions},
		{"http://x]/a", "", errInvalidOptions},
		{"https://[x]@audio.example/a", "", errInvalidOptions},
		// A bad port of an untrusted host never gets evaluated, as in Python.
		{"https://evil:x/a", "", errUntrustedURL},
	}
	for _, tc := range cases {
		target, err := trustedURL(tc.url, hosts)
		if tc.err != nil {
			if err != tc.err {
				t.Errorf("%s: got %v, want %v", tc.url, err, tc.err)
			}
			continue
		}
		if err != nil || target != tc.target {
			t.Errorf("%s: got %q %v, want %q", tc.url, target, err, tc.target)
		}
	}
}

func TestResolveAudioOwnUploads(t *testing.T) {
	a := newAPI(t, func(s *Settings) { s.AudioURLHosts = []string{"audio.example"} })
	r := httptest.NewRequest("POST", "/v2/transcript", nil)
	uid := "0f8fad5b-d9cb-469f-a165-70867728950e"
	got, owned, err := a.resolveAudio(r, "http://api.test/uploads/"+uid)
	if err != nil || got != uid || owned {
		t.Fatalf("canonical: %q %v %v", got, owned, err)
	}
	for _, url := range []string{
		"http://api.test/uploads/" + strings.ToUpper(uid),
		"http://api.test/uploads/" + strings.ReplaceAll(uid, "-", ""),
		"http://api.test/uploads/{" + uid + "}",
		"http://api.test/uploads/" + uid + "/x",
		"http://api.test/uploads/",
	} {
		if _, _, err := a.resolveAudio(r, url); err != errInvalidUploadURL {
			t.Errorf("%s: %v", url, err)
		}
	}
	for _, url := range []string{"https://audio.example/\x00", "https://audio.example/a b", "https://audío.example/", " https://audio.example/", "https://audio.example/\x7f"} {
		if _, _, err := a.resolveAudio(r, url); err != errInvalidAudioURL {
			t.Errorf("%q: %v", url, err)
		}
	}
}

func TestPydanticInt(t *testing.T) {
	valid := map[string]int{" 20": 20, "+20\t": 20, "020": 20, "20.0": 20, "20.000": 20, "2_0": 20, "-0": 0, "\u00a020": 20, "-5": -5}
	for in, want := range valid {
		if got, ok := pydanticInt(in); !ok || got != want {
			t.Errorf("%q: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "abc", "20.5", "20.", ".0", "1e2", "_20", "20_", "2__0", "+-20", "0x14", "20.0_0", "\x1c20", "+"} {
		if _, ok := pydanticInt(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
	if n, ok := pydanticInt("99999999999999999999999"); !ok || (n >= 20 && n <= 200) {
		t.Errorf("huge: %d %v", n, ok)
	}
}

func TestParseOptionsHeader(t *testing.T) {
	cases := []struct {
		in      string
		ctype   string
		options map[string]string
	}{
		{"", "", map[string]string{}},
		{" Multipart/Form-Data ", "multipart/form-data", map[string]string{}},
		// With parameters python-multipart did not lowercase the type.
		{"Multipart/Form-Data; boundary=x", "Multipart/Form-Data", map[string]string{"boundary": "x"}},
		{`multipart/form-data; BOUNDARY = "a;b" ; charset=utf-8`, "multipart/form-data", map[string]string{"boundary": "a;b", "charset": "utf-8"}},
		{`form-data; name="file"; filename="C:\dir\a.wav"`, "form-data", map[string]string{"name": "file", "filename": "a.wav"}},
		{`form-data; name="a\"b"; filename*=utf-8''x`, "form-data", map[string]string{"name": `a"b`}},
		{`form-data; name=file; flag`, "form-data", map[string]string{"name": "file", "flag": ""}},
	}
	for _, tc := range cases {
		ctype, options := parseOptionsHeader(tc.in)
		if ctype != tc.ctype || !reflect.DeepEqual(options, tc.options) {
			t.Errorf("%q: got %q %v", tc.in, ctype, options)
		}
	}
}

func TestMultipartStart(t *testing.T) {
	cases := []struct {
		body    string
		more    bool
		err     error
		remains string
	}{
		{"", false, nil, ""},
		{"\r\n\r\n", false, nil, ""},
		{"--b\r\nrest", true, nil, "rest"},
		{"\r\n--b\r\nrest", true, nil, "rest"},
		{"\r\njunk--b\r\nrest", true, nil, "rest"},
		{"\n" + strings.Repeat("x", 64*1024) + "--b\r\n", false, errInvalidOptions, ""},
		{"--b--", false, nil, ""},
		{"--b--junk", false, nil, "junk"},
		{"--b-x", false, errInvalidOptions, ""},
		{"--b", false, nil, ""},
		{"--b\r", false, nil, ""},
		{"pre\r\n--b\r\n", false, errInvalidOptions, ""},
		{"--c\r\n", false, errInvalidOptions, ""},
		{"--b \r\n", false, errInvalidOptions, ""},
		{"--b\n", false, errInvalidOptions, ""},
		{"--b\rx", false, errInvalidOptions, ""},
	}
	for _, tc := range cases {
		p := &multipartParser{br: bufio.NewReaderSize(strings.NewReader(tc.body), 64*1024), delimiter: []byte("\r\n--b")}
		more, err := p.start()
		rest, _ := io.ReadAll(p.br)
		if more != tc.more || err != tc.err || (err == nil && string(rest) != tc.remains) {
			t.Errorf("%.20q: got %v %v, remains %q", tc.body, more, err, rest)
		}
	}
}

// The part parser takes a delimiter only before CRLF or "--", and at the end of a truncated
// body keeps back what could still start one, as python-multipart's partial match did.
func TestPartReader(t *testing.T) {
	cases := []struct {
		body, data string
		err        error
		last       bool
		remains    string
	}{
		{"abc\r\n--b\r\nnext", "abc", nil, false, "next"},
		{"abc\r\n--b--epilogue", "abc", nil, true, "epilogue"},
		{"a\r\n--b \r\n--bX\r\n--b-\r\r\n--b--", "a\r\n--b \r\n--bX\r\n--b-\r", nil, true, ""},
		{"\r\n--b\r\n", "", nil, false, ""},
		{"abc", "abc", errTruncated, false, ""},
		{"abc\r\n-", "abc", errTruncated, false, ""},
		{"abc\r\n--b", "abc", errTruncated, false, ""},
		{"abc\r\n--b-", "abc", errTruncated, false, ""},
		{"abc\r\n--b\r", "abc", errTruncated, false, ""},
		{"abc\r\n--bZ", "abc\r\n--bZ", errTruncated, false, ""},
		{"abc\r\n--b\n", "abc\r\n--b\n", errTruncated, false, ""},
		{strings.Repeat("x\r\n--b \r\n-\r\n--", 20000) + "\r\n--b--", strings.Repeat("x\r\n--b \r\n-\r\n--", 20000), nil, true, ""},
	}
	for _, tc := range cases {
		for _, size := range []int{16, 64 * 1024} {
			p := &multipartParser{br: bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(tc.body)), size), delimiter: []byte("\r\n--b")}
			part := &partReader{p: p}
			data, err := io.ReadAll(part)
			rest, _ := io.ReadAll(p.br)
			if string(data) != tc.data || err != tc.err || part.last != tc.last || (err == nil && string(rest) != tc.remains) {
				t.Errorf("%q (buffer %d): got %q %v last=%v, remains %q", tc.body, size, data, err, part.last, rest)
			}
		}
	}
}

func TestDownload(t *testing.T) {
	a := newAPI(t, func(s *Settings) { s.UploadIdle = 200 * time.Millisecond })
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("remote audio")) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 4097)) })
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	a.client.Transport = transport

	uid, err := a.download(context.Background(), srv.URL+"/ok")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(a.store.BlobPath(uid)); err != nil || string(data) != "remote audio" {
		t.Fatalf("stored %q %v", data, err)
	}
	for path, want := range map[string]*apiError{
		"/empty":    errAudioEmpty,
		"/large":    errAudioTooLarge,
		"/missing":  errDownloadFailed,
		"/redirect": errDownloadFailed,
		"/stall":    errDownloadFailed,
	} {
		started := time.Now()
		if _, err := a.download(context.Background(), srv.URL+path); err != want {
			t.Errorf("%s: got %v, want %v", path, err, want)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Errorf("%s took %v", path, elapsed)
		}
	}
	if files := audioFiles(t, a); len(files) != 1 {
		t.Fatalf("audio files %v", files)
	}
}
