package api

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adapt2move/parakeet-api/internal/formats"
	"github.com/adapt2move/parakeet-api/internal/store"
)

// Form limits of the Python API: one file, ten other fields of at most 64 KiB each.
const (
	maxFormFiles  = 1
	maxFormFields = 10
	maxFieldBytes = 65536
)

var (
	errUnsupportedFields = newError(http.StatusUnprocessableEntity, "Unsupported transcription fields; streaming and prompts are not supported")
	errDuplicateField    = newError(http.StatusBadRequest, "Duplicate multipart field")
	errUnknownModel      = newError(http.StatusBadRequest, "Unknown model; use parakeet")
	errTemperature       = newError(http.StatusUnprocessableEntity, "Only greedy decoding is supported")
	errResponseFormat    = newError(http.StatusBadRequest, "Invalid response format or timestamp granularity")
	errNotAFile          = newError(http.StatusBadRequest, "file must be an audio file")
	errSyncTimeout       = newError(http.StatusGatewayTimeout, "Transcription wait timed out; use the asynchronous /v2 API")

	errMissingBoundary = newError(http.StatusBadRequest, "Missing boundary in multipart.")
	errMissingName     = newError(http.StatusBadRequest, `The Content-Disposition header field "name" must be provided.`)
	errTooManyFiles    = newError(http.StatusBadRequest, "Too many files. Maximum number of files is 1.")
	errTooManyFields   = newError(http.StatusBadRequest, "Too many fields. Maximum number of fields is 10.")
	errPartTooLarge    = newError(http.StatusBadRequest, "Part exceeded maximum size of 64KB.")
	errFieldTooLarge   = newError(http.StatusBadRequest, "Field exceeded maximum size of 64KB.")
)

var allowedFields = []string{"file", "model", "language", "response_format", "timestamp_granularities[]", "temperature"}

type formValue struct {
	name  string
	value string
	file  bool // a part with a filename; value is unused
}

// form is a parsed /v1 request. The "file" part streams straight into a reserved upload.
type form struct {
	values  []formValue
	upload  string // reserved upload holding the file part, if any
	size    int64
	saveErr error // storing the file failed; reported only after the fields validate
}

func (f *form) last(name string) (formValue, bool) {
	for i := len(f.values) - 1; i >= 0; i-- {
		if f.values[i].name == name {
			return f.values[i], true
		}
	}
	return formValue{}, false
}

func (a *API) transcriptions(w http.ResponseWriter, r *http.Request, _ []string) {
	res, err := a.transcribe(r)
	a.reply(w, r, res, err)
}

// transcribe runs a synchronous transcription. Its job and audio are gone when it returns.
func (a *API) transcribe(r *http.Request) (response, error) {
	f := &form{}
	jobID := ""
	defer func() {
		if jobID != "" {
			a.store.Delete(jobID)
		} else if f.upload != "" {
			a.store.DiscardUpload(f.upload)
		}
	}()
	if err := a.readForm(r, f); err != nil {
		return response{}, err
	}
	format, granularities, options, err := validateForm(f)
	if err != nil {
		return response{}, err
	}
	if f.saveErr != nil {
		return response{}, f.saveErr
	}
	if f.size == 0 {
		return response{}, errAudioEmpty
	}
	if err := a.store.UploadReady(f.upload, f.size); err != nil {
		return response{}, err
	}
	releaseSlot(r)
	job, err := a.store.Submit(f.upload, options)
	if err != nil {
		return response{}, err
	}
	jobID = job.ID
	job, err = a.wait(r, job.ID)
	if err != nil {
		return response{}, err
	}
	result, err := storedResult(job)
	if err != nil {
		return response{}, err
	}
	switch format {
	case "json":
		return jsonResponse(formats.TextJSON(result.Text)), nil
	case "verbose_json":
		body, _ := formats.OpenAI(result, granularities).MarshalJSON()
		return jsonResponse(body), nil
	case "text":
		return textResponse("text/plain", result.Text), nil
	case "vtt":
		return textResponse("text/vtt", formats.Subtitles(result, true, 80)), nil
	default:
		return textResponse("text/plain", formats.Subtitles(result, false, 80)), nil
	}
}

// wait polls the job until it completes, fails, the client disconnects or the sync timeout hits.
func (a *API) wait(r *http.Request, id string) (store.Job, error) {
	timeout := time.NewTimer(a.cfg.SyncTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(a.poll)
	defer ticker.Stop()
	for {
		job, err := a.store.Get(id)
		if err != nil {
			return store.Job{}, err
		}
		switch job.Status {
		case store.StatusError:
			message := ""
			if job.Error != nil {
				message = *job.Error
			}
			return store.Job{}, newError(http.StatusUnprocessableEntity, message)
		case store.StatusCompleted:
			return job, nil
		}
		select {
		case <-r.Context().Done():
			return store.Job{}, errDisconnected
		case <-timeout.C:
			return store.Job{}, errSyncTimeout
		case <-ticker.C:
		}
	}
}

func validateForm(f *form) (format string, granularities []string, options map[string]string, err error) {
	counts := map[string]int{}
	for _, v := range f.values {
		if !contains(allowedFields, v.name) {
			return "", nil, nil, errUnsupportedFields
		}
		counts[v.name]++
	}
	for name, n := range counts {
		if name != "timestamp_granularities[]" && n != 1 {
			return "", nil, nil, errDuplicateField
		}
	}
	// A file part never equals a string, as an UploadFile never did in Python.
	if v, ok := f.last("model"); ok && (v.file || !contains([]string{"parakeet", "parakeet-tdt-0.6b-v3", "whisper-1"}, v.value)) {
		return "", nil, nil, errUnknownModel
	}
	if v, ok := f.last("temperature"); ok && (v.file || (v.value != "0" && v.value != "0.0")) {
		return "", nil, nil, errTemperature
	}
	format = "json"
	valid := true
	if v, ok := f.last("response_format"); ok {
		format, valid = v.value, !v.file
	}
	valid = valid && contains([]string{"json", "text", "verbose_json", "srt", "vtt"}, format)
	for _, v := range f.values {
		if v.name == "timestamp_granularities[]" {
			valid = valid && !v.file && (v.value == "word" || v.value == "segment")
			granularities = append(granularities, v.value)
		}
	}
	if !valid {
		return "", nil, nil, errResponseFormat
	}
	if len(granularities) == 0 {
		granularities = []string{"segment"}
	}
	options = map[string]string{}
	if v, ok := f.last("language"); ok && (v.file || v.value != "") {
		if v.file || !formats.ValidLanguage(v.value) {
			return "", nil, nil, errInvalidOptions
		}
		options["language_code"] = v.value
	}
	if v, ok := f.last("file"); !ok || !v.file {
		return "", nil, nil, errNotAFile
	}
	return format, granularities, options, nil
}

// readForm parses the body like Starlette's request.form(): multipart and urlencoded bodies are
// read, any other content type yields an empty form without reading the body.
func (a *API) readForm(r *http.Request, f *form) error {
	ctype, params := parseOptionsHeader(r.Header.Get("Content-Type"))
	switch ctype {
	case "multipart/form-data":
		boundary, ok := params["boundary"]
		if !ok {
			return errMissingBoundary
		}
		if err := a.readMultipart(r.Body, boundary, f); err != nil {
			// A parser may hide a failure of the body behind its own error; the body's error wins.
			if b := state(r).body; b != nil && b.err != nil {
				return b.err
			}
			return err
		}
	case "application/x-www-form-urlencoded":
		if err := readURLEncoded(r.Body, f); err != nil {
			return err
		}
	default:
		return nil
	}
	// Reach the end of the body so that the server notices a client that disconnects later.
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

func (a *API) readMultipart(body io.Reader, boundary string, f *form) error {
	br := bufio.NewReaderSize(body, 64*1024)
	if ok, err := multipartStart(br, boundary); !ok || err != nil {
		return err
	}
	mr := multipart.NewReader(br, boundary)
	files, fields := 0, 0
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF || truncated(err) {
			// python-multipart ended the form at the end of the body and dropped an unfinished part.
			return nil
		}
		if err != nil {
			return multipartError(err)
		}
		dispositions := part.Header.Values("Content-Disposition")
		disposition := ""
		if len(dispositions) > 0 {
			disposition = dispositions[len(dispositions)-1]
		}
		_, options := parseOptionsHeader(disposition)
		name, ok := options["name"]
		if !ok {
			return errMissingName
		}
		if _, isFile := options["filename"]; !isFile {
			fields++
			if fields > maxFormFields {
				return errTooManyFields
			}
			data, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if truncated(err) {
				return nil
			}
			if err != nil {
				return multipartError(err)
			}
			if len(data) > maxFieldBytes {
				return errPartTooLarge
			}
			f.values = append(f.values, formValue{name: name, value: string(data)})
			continue
		}
		files++
		if files > maxFormFiles {
			return errTooManyFiles
		}
		if name == "file" {
			err = a.receiveFile(part, f)
		} else {
			_, err = io.Copy(io.Discard, part)
		}
		if truncated(err) {
			if f.upload != "" {
				a.store.DiscardUpload(f.upload)
				f.upload, f.size, f.saveErr = "", 0, nil
			}
			return nil
		}
		if err != nil {
			return multipartError(err)
		}
		f.values = append(f.values, formValue{name: name, file: true})
	}
}

// receiveFile streams the file part into a new upload. Storage and size failures are kept in
// f.saveErr so the fields can still be validated first, as Python did after spooling the form.
func (a *API) receiveFile(part io.Reader, f *form) error {
	uid, err := a.store.Reserve()
	if err != nil {
		f.saveErr = err
		_, err = io.Copy(io.Discard, part)
		return err
	}
	f.upload = uid
	f.size, err = a.writeBlob(uid, part, nil)
	if errors.Is(err, errAudioTooLarge) {
		f.saveErr = err
		_, err = io.Copy(io.Discard, part)
	}
	return err
}

// multipartStart checks the start of a multipart body as python-multipart did: optional line
// breaks, then the first boundary. It returns false without error for a body that ends before
// any part starts, which is an empty form.
func multipartStart(br *bufio.Reader, boundary string) (bool, error) {
	skip := 0
	if head, err := br.Peek(1); err != nil {
		return false, peekError(err)
	} else if head[0] == '\r' || head[0] == '\n' {
		// python-multipart jumped from a leading line break to the next hyphen.
		for {
			data, err := br.Peek(skip + 1)
			if err != nil {
				return false, peekError(err)
			}
			if data[skip] == '-' {
				break
			}
			skip++
			if skip >= br.Size() {
				return false, errInvalidOptions
			}
		}
	}
	delimiter := "--" + boundary
	for i := 0; i <= len(delimiter)+1; i++ {
		data, err := br.Peek(skip + i + 1)
		if err != nil {
			return false, peekError(err)
		}
		c := data[skip+i]
		switch {
		case i < len(delimiter):
			if c != delimiter[i] {
				return false, errInvalidOptions
			}
		case i == len(delimiter):
			if c != '\r' && c != '-' {
				return false, errInvalidOptions
			}
			if c == '-' {
				i++ // an immediately closed, empty form
			}
		default:
			if c != '\n' {
				return false, errInvalidOptions
			}
		}
	}
	_, err := br.Discard(skip)
	return true, err
}

// truncated reports a body that ended inside a part, as opposed to a failure of the body itself.
func truncated(err error) bool {
	var ae *apiError
	return err != nil && !errors.As(err, &ae) && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
}

func peekError(err error) error {
	if err == io.EOF {
		return nil
	}
	return multipartError(err)
}

// multipartError keeps limit errors of the body and maps parser failures to the generic 400 that
// python-multipart's ValueError produced.
func multipartError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	return errInvalidOptions
}

// readURLEncoded parses a form body like python-multipart's QuerystringParser.
func readURLEncoded(body io.Reader, f *form) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	fields := 0
	for _, chunk := range bytes.Split(data, []byte("&")) {
		if len(chunk) == 0 {
			continue
		}
		name, value, _ := bytes.Cut(chunk, []byte("="))
		if len(name)+len(value) > maxFieldBytes {
			return errFieldTooLarge
		}
		fields++
		if fields > maxFormFields {
			return errTooManyFields
		}
		f.values = append(f.values, formValue{name: unquotePlus(name), value: unquotePlus(value)})
	}
	return nil
}

func unquotePlus(b []byte) string {
	s := strings.ReplaceAll(string(b), "+", " ")
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

// parseOptionsHeader ports python-multipart's parse_options_header for the values the form
// parser inspects. Without parameters the type is lowercased; with parameters it is not.
func parseOptionsHeader(value string) (string, map[string]string) {
	options := map[string]string{}
	if value == "" {
		return "", options
	}
	if !strings.Contains(value, ";") {
		return strings.TrimFunc(strings.ToLower(value), pySpace), options
	}
	segments := parseParams(value)
	for _, segment := range segments[1:] {
		key, val, _ := strings.Cut(segment, "=")
		if strings.Contains(key, "*") {
			continue
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = strings.ReplaceAll(strings.ReplaceAll(val[1:len(val)-1], `\\`, `\`), `\"`, `"`)
		}
		if key == "filename" && (strings.HasPrefix(safeSlice(val, 1, 3), `:\`) || strings.HasPrefix(val, `\\`)) {
			val = val[strings.LastIndexByte(val, '\\')+1:]
		}
		options[key] = val
	}
	return segments[0], options
}

func safeSlice(s string, from, to int) string {
	if from > len(s) {
		return ""
	}
	return s[from:min(to, len(s))]
}

// parseParams ports python-multipart's _parseparam, including its quote counting.
func parseParams(value string) []string {
	s := ";" + value
	var params []string
	start := 0
	for pyFind(s, ";", start, len(s)) == start {
		start++
		end := pyFind(s, ";", start, len(s))
		ind, diff := start, 0
		for end > 0 {
			diff += pyCount(s, `"`, ind, end) - pyCount(s, `\"`, ind, end)
			if diff%2 == 0 {
				break
			}
			end, ind = ind, pyFind(s, ";", end+1, len(s))
		}
		if end < 0 {
			end = len(s)
		}
		var f string
		if i := pyFind(s, "=", start, end); i == -1 {
			f = pySlice(s, start, end)
		} else {
			f = strings.ToLower(strings.TrimRightFunc(pySlice(s, start, i), pySpace)) + "=" +
				strings.TrimLeftFunc(pySlice(s, i+1, end), pySpace)
		}
		params = append(params, strings.TrimFunc(f, pySpace))
		start = end
	}
	return params
}

// pyIndices normalizes slice bounds like Python does for str.find, str.count and slicing.
func pyIndices(n, start, end int) (int, int) {
	if start < 0 {
		start = max(start+n, 0)
	}
	if end < 0 {
		end = max(end+n, 0)
	}
	return min(start, n), min(end, n)
}

func pySlice(s string, start, end int) string {
	start, end = pyIndices(len(s), start, end)
	if start >= end {
		return ""
	}
	return s[start:end]
}

func pyFind(s, sub string, start, end int) int {
	start, end = pyIndices(len(s), start, end)
	if start > end {
		return -1
	}
	if i := strings.Index(s[start:end], sub); i >= 0 {
		return start + i
	}
	return -1
}

func pyCount(s, sub string, start, end int) int {
	start, end = pyIndices(len(s), start, end)
	if start > end {
		return 0
	}
	return strings.Count(s[start:end], sub)
}
