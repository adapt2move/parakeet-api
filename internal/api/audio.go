package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

var (
	errInvalidAudioURL  = newError(http.StatusBadRequest, "Invalid audio URL")
	errInvalidUploadURL = newError(http.StatusBadRequest, "Invalid upload URL")
	errUntrustedURL     = newError(http.StatusBadRequest, "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin")
	errDownloadFailed   = newError(http.StatusBadRequest, "Audio download failed")
)

// save stores src as a new ready upload and returns its ID. A non-nil p also bounds the pace of
// src. On failure nothing of the upload remains.
func (a *API) save(src io.Reader, p *pace) (string, error) {
	uid, err := a.store.Reserve()
	if err != nil {
		return "", err
	}
	size, err := a.writeBlob(uid, src, p)
	if err == nil && size == 0 {
		err = errAudioEmpty
	}
	if err == nil {
		err = a.store.UploadReady(uid, size)
	}
	if err != nil {
		a.store.DiscardUpload(uid)
		return "", err
	}
	return uid, nil
}

// writeBlob copies src into the audio file of a reserved upload. It stops at the first byte
// beyond MaxUploadBytes with errAudioTooLarge, leaving the rest of src unread.
func (a *API) writeBlob(uid string, src io.Reader, p *pace) (int64, error) {
	f, err := a.store.CreateBlob(uid)
	if err != nil {
		return 0, err
	}
	size, err := a.copyAudio(f, src, p)
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	return size, err
}

func (a *API) copyAudio(dst io.Writer, src io.Reader, p *pace) (int64, error) {
	deadline := a.now().Add(transferLimit)
	buf := make([]byte, 256*1024)
	var size int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > a.cfg.MaxUploadBytes {
				return size, errAudioTooLarge
			}
			now := a.now()
			if p != nil && !p.add(n, now) {
				return size, errTooSlow
			}
			if now.After(deadline) {
				return size, errTransferTimeout
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return size, err
			}
		}
		if err == io.EOF {
			return size, nil
		}
		if err != nil {
			return size, err
		}
	}
}

// resolveAudio returns the upload for an audio_url: this server's own upload URL, or a new
// upload downloaded from a trusted HTTPS host (owned is then true).
func (a *API) resolveAudio(r *http.Request, raw string) (uid string, owned bool, err error) {
	// Python's urlsplit silently dropped tabs and newlines; reject every control character,
	// space and non-ASCII byte instead.
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return "", false, errInvalidAudioURL
		}
	}
	if prefix := a.cfg.PublicURL + "/uploads/"; strings.HasPrefix(raw, prefix) {
		uid := raw[len(prefix):]
		if !canonicalUUID(uid) {
			return "", false, errInvalidUploadURL
		}
		return uid, false, nil
	}
	target, err := trustedURL(raw, a.cfg.AudioURLHosts)
	if err != nil {
		return "", false, err
	}
	uid, err = a.download(r.Context(), target)
	return uid, true, err
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// canonicalUUID reports whether s is what Python's str(uuid.UUID(s)) returns unchanged.
func canonicalUUID(s string) bool { return uuidPattern.MatchString(s) }

// trustedURL checks a printable ASCII URL the way the Python API did with urllib.parse.urlsplit
// and returns the URL to fetch. Only HTTPS on port 443 to an allowlisted host passes; unlike
// Python, any userinfo is rejected, even an empty one.
func trustedURL(raw string, hosts []string) (string, error) {
	scheme, rest := "", raw
	if i := strings.IndexByte(raw, ':'); i > 0 && isAlpha(raw[0]) && strings.Trim(raw[:i], schemeChars) == "" {
		scheme, rest = strings.ToLower(raw[:i]), raw[i+1:]
	}
	netloc := ""
	if strings.HasPrefix(rest, "//") {
		end := len(rest)
		if j := strings.IndexAny(rest[2:], "/?#"); j >= 0 {
			end = j + 2
		}
		netloc, rest = rest[2:end], rest[end:]
		opened, closed := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
		if opened != closed || (opened && !validBracketedNetloc(netloc)) {
			// urlsplit raised ValueError, which the API answered with its generic 400.
			return "", errInvalidOptions
		}
	}
	rest, fragment, _ := strings.Cut(rest, "#")

	hostinfo := netloc
	if i := strings.LastIndexByte(netloc, '@'); i >= 0 {
		hostinfo = netloc[i+1:]
	}
	var hostname, port string
	_, bracketed, isBracketed := strings.Cut(hostinfo, "[")
	if isBracketed {
		var after string
		hostname, after, _ = strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(after, ":")
	} else {
		hostname, port, _ = strings.Cut(hostinfo, ":")
	}
	hostname = strings.ToLower(hostname)

	if scheme != "https" || hostname == "" || isBracketed || !contains(hosts, hostname) {
		return "", errUntrustedURL
	}
	if port != "" {
		if strings.Trim(port, "0123456789") != "" {
			return "", errInvalidOptions
		}
		digits := strings.TrimLeft(port, "0")
		if len(digits) > 5 || (len(digits) == 5 && digits > "65535") {
			return "", errInvalidOptions
		}
		if digits != "443" {
			return "", errUntrustedURL
		}
	}
	if strings.Contains(netloc, "@") || fragment != "" {
		return "", errUntrustedURL
	}
	if rest == "" || rest[0] == '?' {
		rest = "/" + rest
	}
	return "https://" + hostname + rest, nil
}

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

func isAlpha(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var ipvFuture = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

// validBracketedNetloc mirrors urllib.parse._check_bracketed_netloc.
func validBracketedNetloc(netloc string) bool {
	hostAndPort := netloc[strings.LastIndexByte(netloc, '@')+1:]
	before, bracketed, found := strings.Cut(hostAndPort, "[")
	var hostname string
	if found {
		if before != "" {
			return false
		}
		var port string
		hostname, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return false
		}
	} else {
		hostname, _, _ = strings.Cut(hostAndPort, ":")
	}
	if strings.HasPrefix(hostname, "v") {
		return ipvFuture.MatchString(hostname)
	}
	addr, err := netip.ParseAddr(hostname)
	return err == nil && !addr.Is4()
}

// downloadClient never follows redirects and never uses a proxy from the environment.
func downloadClient(idle time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            (&net.Dialer{Timeout: idle}).DialContext,
			TLSHandshakeTimeout:    idle,
			ResponseHeaderTimeout:  idle,
			MaxResponseHeaderBytes: 64 << 10,
			IdleConnTimeout:        30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var errIdle = errors.New("download idle timeout")

// download fetches target into a new upload. Every failure of the transfer itself is the
// generic errDownloadFailed; size, pace and storage limits keep their own errors.
func (a *API) download(ctx context.Context, target string) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
	body := &idleReader{src: res.Body, idle: a.cfg.UploadIdle, cancel: cancel}
	defer body.stop()
	return a.save(body, &pace{start: a.now(), idle: a.cfg.UploadIdle, rate: a.cfg.UploadRate})
}

// idleReader cancels the download when a single read waits longer than idle.
type idleReader struct {
	src    io.Reader
	idle   time.Duration
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.timer == nil {
		r.timer = time.AfterFunc(r.idle, func() { r.cancel(errIdle) })
	} else {
		r.timer.Reset(r.idle)
	}
	n, err := r.src.Read(p)
	r.timer.Stop()
	if err != nil && err != io.EOF {
		return n, errDownloadFailed
	}
	return n, err
}

func (r *idleReader) stop() {
	if r.timer != nil {
		r.timer.Stop()
	}
}
