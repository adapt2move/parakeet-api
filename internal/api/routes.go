package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/adapt2move/parakeet-api/internal/formats"
	"github.com/adapt2move/parakeet-api/internal/store"
)

// API serves the public AssemblyAI and OpenAI compatible endpoints and the internal worker queue.
type API struct {
	cfg    Settings
	store  *store.Store
	log    *slog.Logger
	client *http.Client
	now    func() time.Time
	// poll is how often a synchronous /v1 request checks its job.
	poll time.Duration

	slotsMu sync.Mutex
	uploads int
}

// New returns the HTTP handler for a store.
func New(cfg Settings, st *store.Store, log *slog.Logger) *API {
	return &API{
		cfg:    cfg,
		store:  st,
		log:    log,
		client: downloadClient(cfg.UploadIdle),
		now:    time.Now,
		poll:   200 * time.Millisecond,
	}
}

type handler func(*API, http.ResponseWriter, *http.Request, []string)

type route struct {
	name    string
	method  string
	pattern []string // "*" matches one non-empty segment
	handle  handler
}

// routes is filled in init: handlers log through routeName, which reads routes.
var routes []route

func init() {
	routes = []route{
		{"/health/live", http.MethodGet, []string{"health", "live"}, (*API).live},
		{"/health/ready", http.MethodGet, []string{"health", "ready"}, (*API).ready},
		{"/metrics", http.MethodGet, []string{"metrics"}, (*API).metrics},
		{"/v2/upload", http.MethodPost, []string{"v2", "upload"}, (*API).upload},
		{"/v2/transcript", http.MethodPost, []string{"v2", "transcript"}, (*API).submit},
		{"/v2/transcript/{id}", http.MethodGet, []string{"v2", "transcript", "*"}, (*API).get},
		{"/v2/transcript/{id}", http.MethodDelete, []string{"v2", "transcript", "*"}, (*API).delete},
		{"/v2/transcript/{id}/{extension}", http.MethodGet, []string{"v2", "transcript", "*", "*"}, (*API).captions},
		{"/v1/audio/transcriptions", http.MethodPost, []string{"v1", "audio", "transcriptions"}, (*API).transcriptions},
		{"/internal/jobs/claim", http.MethodPost, []string{"internal", "jobs", "claim"}, (*API).claim},
		{"/internal/jobs/{id}/heartbeat", http.MethodPost, []string{"internal", "jobs", "*", "heartbeat"}, (*API).heartbeat},
		{"/internal/jobs/{id}/audio", http.MethodGet, []string{"internal", "jobs", "*", "audio"}, (*API).audio},
		{"/internal/jobs/{id}/complete", http.MethodPost, []string{"internal", "jobs", "*", "complete"}, (*API).complete},
	}
}

// match returns the parameters when the path fits the pattern.
func match(pattern, segments []string) ([]string, bool) {
	if len(pattern) != len(segments) {
		return nil, false
	}
	var params []string
	for i, p := range pattern {
		switch {
		case p == "*" && segments[i] != "":
			params = append(params, segments[i])
		case p != segments[i]:
			return nil, false
		}
	}
	return params, true
}

func splitPath(path string) []string {
	if !strings.HasPrefix(path, "/") {
		return nil
	}
	return strings.Split(path[1:], "/")
}

// routeName is the route pattern for logs; it never contains IDs or other request data.
func routeName(r *http.Request) string {
	segments := splitPath(r.URL.Path)
	for _, rt := range routes {
		if _, ok := match(rt.pattern, segments); ok {
			return rt.name
		}
	}
	return "unknown"
}

func (a *API) route(w http.ResponseWriter, r *http.Request) {
	segments := splitPath(r.URL.Path)
	var allowed []string
	for _, rt := range routes {
		params, ok := match(rt.pattern, segments)
		if !ok {
			continue
		}
		if rt.method == r.Method {
			rt.handle(a, w, r, params)
			return
		}
		allowed = append(allowed, rt.method)
	}
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		a.fail(w, r, errMethodNotAllowed)
		return
	}
	if location, ok := slashRedirect(r); ok {
		// Starlette's redirect_slashes; its Location was absolute, built from the Host header.
		w.Header().Set("Location", location)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	a.fail(w, r, errNotFound)
}

// slashRedirect returns the path without trailing slashes, and the query, when that path has
// a route for any method.
func slashRedirect(r *http.Request) (string, bool) {
	path := r.URL.EscapedPath()
	if path == "/" || !strings.HasSuffix(path, "/") {
		return "", false
	}
	segments := splitPath(strings.TrimRight(r.URL.Path, "/"))
	for _, rt := range routes {
		if _, ok := match(rt.pattern, segments); ok {
			location := strings.TrimRight(path, "/")
			if r.URL.RawQuery != "" {
				location += "?" + r.URL.RawQuery
			}
			return location, true
		}
	}
	return "", false
}

// reply writes a buffered handler result.
func (a *API) reply(w http.ResponseWriter, r *http.Request, res response, err error) {
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeResponse(w, res)
}

var statusOK = []byte(`{"status":"ok"}`)

func (a *API) live(w http.ResponseWriter, r *http.Request, _ []string) {
	writeResponse(w, jsonResponse(statusOK))
}

func (a *API) ready(w http.ResponseWriter, r *http.Request, _ []string) {
	a.store.Counts()
	writeResponse(w, jsonResponse(statusOK))
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request, _ []string) {
	counts := a.store.Counts()
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	slices.Sort(statuses)
	var b strings.Builder
	for _, status := range statuses {
		b.WriteString(`parakeet_jobs{status="` + status + `"} ` + strconv.Itoa(counts[status]) + "\n")
	}
	writeResponse(w, textResponse("text/plain", b.String()))
}

func (a *API) uploadURL(uploadID string) string {
	return a.cfg.PublicURL + "/uploads/" + uploadID
}

func (a *API) upload(w http.ResponseWriter, r *http.Request, _ []string) {
	uid, err := a.save(r.Body, nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	releaseSlot(r)
	b := append([]byte(`{"upload_url":`), formats.AppendString(nil, a.uploadURL(uid))...)
	writeResponse(w, jsonResponse(append(b, '}')))
}

func (a *API) submit(w http.ResponseWriter, r *http.Request, _ []string) {
	res, err := a.submitJob(r)
	a.reply(w, r, res, err)
}

func (a *API) submitJob(r *http.Request) (response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return response{}, err
	}
	sub, err := formats.DecodeSubmission(r.Header.Get("Content-Type"), body)
	if err != nil {
		return response{}, err
	}
	options := map[string]string{}
	// Python only forwarded a non-empty language_code.
	if code := sub.LanguageCode; code != nil && *code != "" {
		if !formats.ValidLanguage(*code) {
			return response{}, errInvalidOptions
		}
		options["language_code"] = *code
	}
	uid, owned, err := a.resolveAudio(r, sub.AudioURL)
	if err != nil {
		return response{}, err
	}
	job, err := a.store.Submit(uid, options)
	if err != nil {
		if owned {
			a.store.DiscardUpload(uid)
		}
		return response{}, err
	}
	return a.transcriptResponse(job, false)
}

// completed is what the store keeps for a completed job: the validated result and, after the
// first read, its rendered transcript, which every later read shares.
type completed struct {
	result     *formats.Result
	once       sync.Once
	transcript []byte
}

// stored returns what the store keeps for a completed job.
func stored(job store.Job) (*completed, error) {
	c, ok := job.Result.(*completed)
	if !ok {
		return nil, errors.New("stored result has an unexpected type")
	}
	return c, nil
}

// storedResult returns the result of a completed job.
func storedResult(job store.Job) (*formats.Result, error) {
	c, err := stored(job)
	if err != nil {
		return nil, err
	}
	return c.result, nil
}

// transcriptResponse renders a job as an AssemblyAI transcript.
func (a *API) transcriptResponse(job store.Job, deleted bool) (response, error) {
	fj := formats.Job{ID: job.ID, Status: string(job.Status), Error: job.Error, UploadID: job.UploadID}
	if code, ok := job.Options["language_code"]; ok {
		fj.LanguageCode = &code
	}
	if job.Result != nil {
		c, err := stored(job)
		if err != nil {
			return response{}, err
		}
		fj.Result = c.result
		if !deleted {
			// Everything the transcript shows is fixed once the job completed.
			c.once.Do(func() { c.transcript, _ = formats.Assembly(fj, a.cfg.PublicURL).MarshalJSON() })
			return jsonResponse(c.transcript), nil
		}
	}
	t := formats.Assembly(fj, a.cfg.PublicURL)
	if deleted {
		t = t.Deleted()
	}
	body, _ := t.MarshalJSON()
	return jsonResponse(body), nil
}

func (a *API) get(w http.ResponseWriter, r *http.Request, params []string) {
	job, err := a.store.Get(params[0])
	if err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.transcriptResponse(job, false)
	a.reply(w, r, res, err)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request, params []string) {
	job, err := a.store.Get(params[0])
	if err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.transcriptResponse(job, true)
	if err == nil {
		err = a.store.Delete(params[0])
	}
	a.reply(w, r, res, err)
}

var errUnknownEndpoint = newError(http.StatusNotFound, "Unknown endpoint")

func (a *API) captions(w http.ResponseWriter, r *http.Request, params []string) {
	res, err := a.renderCaptions(r, params[0], params[1])
	a.reply(w, r, res, err)
}

func (a *API) renderCaptions(r *http.Request, id, extension string) (response, error) {
	chars := 80
	query, _ := url.ParseQuery(r.URL.RawQuery)
	if values := query["chars_per_caption"]; len(values) > 0 {
		n, ok := pydanticInt(values[len(values)-1])
		if !ok {
			return response{}, errInvalidFields
		}
		chars = n
	}
	if extension != "srt" && extension != "vtt" {
		return response{}, errUnknownEndpoint
	}
	if chars < 20 || chars > 200 {
		return response{}, newError(http.StatusBadRequest, "chars_per_caption must be 20..200")
	}
	job, err := a.store.Get(id)
	if err != nil {
		return response{}, err
	}
	if job.Status != store.StatusCompleted {
		return response{}, newError(http.StatusConflict, "Transcript is not complete")
	}
	result, err := storedResult(job)
	if err != nil {
		return response{}, err
	}
	vtt := extension == "vtt"
	body := formats.AppendSubtitles(nil, result, vtt, chars)
	if vtt {
		return response{status: http.StatusOK, contentType: "text/vtt; charset=utf-8", body: body}, nil
	}
	return response{status: http.StatusOK, contentType: "application/x-subrip", body: body}, nil
}

// pydanticInt parses a query parameter as pydantic's lax int did: surrounding whitespace, a
// sign, underscores between digits and a fraction of zeros are accepted. Values outside the
// int range come back as -1, which every range check rejects.
func pydanticInt(raw string) (int, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	negative := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		negative = s[0] == '-'
		s = s[1:]
	}
	whole, fraction, dotted := strings.Cut(s, ".")
	if dotted && (fraction == "" || strings.Trim(fraction, "0") != "") {
		return 0, false
	}
	if whole == "" {
		return 0, false
	}
	digits := make([]byte, 0, len(whole))
	for i := 0; i < len(whole); i++ {
		c := whole[i]
		switch {
		case isDigit(c):
			digits = append(digits, c)
		case c == '_' && i > 0 && i < len(whole)-1 && isDigit(whole[i-1]) && isDigit(whole[i+1]):
		default:
			return 0, false
		}
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return -1, true
	}
	if negative {
		n = -n
	}
	return n, true
}

func leaseToken(r *http.Request) (string, error) {
	token := r.Header.Get("X-Lease-Token")
	if token == "" {
		return "", errMissingLease
	}
	return token, nil
}

func (a *API) claim(w http.ResponseWriter, r *http.Request, _ []string) {
	c := a.store.Claim()
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b := []byte(`{"id":`)
	b = formats.AppendString(b, c.ID)
	b = append(b, `,"token":`...)
	b = formats.AppendString(b, c.Token)
	b = append(b, `,"lease_seconds":`...)
	b = strconv.AppendInt(b, int64(c.LeaseSeconds), 10)
	b = append(b, `,"bytes":`...)
	b = strconv.AppendInt(b, c.Bytes, 10)
	b = append(b, `,"options":{`...)
	keys := make([]string, 0, len(c.Options))
	for key := range c.Options {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for i, key := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = formats.AppendString(b, key)
		b = append(b, ':')
		b = formats.AppendString(b, c.Options[key])
	}
	writeResponse(w, jsonResponse(append(b, "}}"...)))
}

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request, params []string) {
	token, err := leaseToken(r)
	if err == nil {
		err = a.store.Heartbeat(params[0], token)
	}
	a.reply(w, r, jsonResponse(okBody), err)
}

func (a *API) audio(w http.ResponseWriter, r *http.Request, params []string) {
	token, err := leaseToken(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	f, err := a.store.Audio(params[0], token)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (a *API) complete(w http.ResponseWriter, r *http.Request, params []string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	c, err := formats.DecodeCompletion(r.Header.Get("Content-Type"), body)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	token, err := leaseToken(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var result any
	if c.Result != nil {
		result = &completed{result: c.Result}
	}
	message := ""
	if c.Error != nil {
		message = *c.Error
	}
	err = a.store.Finish(params[0], token, result, message, c.Retry)
	a.reply(w, r, jsonResponse(okBody), err)
}
