package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/adapt2move/parakeet-api/internal/store"
)

var (
	errInvalidAudioURL  = newError(http.StatusBadRequest, "Invalid audio URL")
	errInvalidUploadURL = newError(http.StatusBadRequest, "Invalid upload URL")
	errUntrustedURL     = newError(http.StatusBadRequest, "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin")
	errDownloadFailed   = newError(http.StatusBadRequest, "Audio download failed")
)

// resolveAudio returns the upload for an audio_url: one of this server's upload URLs, or a new
// upload downloaded from a trusted host, which the caller then owns.
func (a *API) resolveAudio(ctx context.Context, raw string) (uploadID string, owned bool, err error) {
	if strings.ContainsFunc(raw, func(c rune) bool { return c <= ' ' || c >= 0x7f }) {
		return "", false, errInvalidAudioURL
	}
	if id, ok := strings.CutPrefix(raw, a.cfg.PublicURL+"/uploads/"); ok {
		if u, err := uuid.Parse(id); err != nil || u.String() != id {
			return "", false, errInvalidUploadURL
		}
		return id, false, nil
	}
	u, err := trustedURL(raw, a.cfg.AudioURLHosts)
	if err != nil {
		return "", false, err
	}
	uploadID, err = a.download(ctx, u)
	return uploadID, err == nil, err
}

// trustedURL accepts only HTTPS URLs on port 443 of an allowed host, without userinfo or fragment.
func trustedURL(raw string, hosts []string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errInvalidAudioURL
	}
	if u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") || !slices.Contains(hosts, strings.ToLower(u.Hostname())) {
		return nil, errUntrustedURL
	}
	return u, nil
}

// downloadClient never follows redirects and never uses a proxy.
func downloadClient(idle time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:            (&net.Dialer{Timeout: idle}).DialContext,
			TLSHandshakeTimeout:    idle,
			ResponseHeaderTimeout:  idle,
			MaxResponseHeaderBytes: 64 << 10,
			IdleConnTimeout:        30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// download fetches u into a new upload. Failures of the transfer are all errDownloadFailed.
func (a *API) download(ctx context.Context, u *url.URL) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errDownloadFailed
	}
	res, err := a.client.Do(req)
	if err != nil {
		return "", errDownloadFailed
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", errDownloadFailed
	}
	src := &downloadReader{body: res.Body, idle: a.cfg.UploadIdle, timer: time.AfterFunc(a.cfg.UploadIdle, cancel),
		pace: pace{start: time.Now(), idle: a.cfg.UploadIdle, rate: a.cfg.UploadRate}}
	defer src.timer.Stop()
	id, err := a.store.Save(src)
	if src.failed || errors.Is(err, store.ErrTooLarge) || errors.Is(err, store.ErrEmpty) {
		return "", errDownloadFailed
	}
	return id, err
}

// downloadReader cancels the download when one read waits longer than idle, and fails it when
// the average rate stays too low.
type downloadReader struct {
	body   io.Reader
	idle   time.Duration
	timer  *time.Timer
	pace   pace
	failed bool
}

func (d *downloadReader) Read(p []byte) (int, error) {
	d.timer.Reset(d.idle)
	n, err := d.body.Read(p)
	if (err != nil && err != io.EOF) || !d.pace.add(n) {
		d.failed = true
		return 0, errDownloadFailed
	}
	return n, err
}
