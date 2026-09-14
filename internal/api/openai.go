package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/adapt2move/parakeet-api/internal/formats"
	"github.com/adapt2move/parakeet-api/internal/store"
)

const (
	maxFormFields = 10
	maxFieldBytes = 64 << 10
)

var (
	errUnsupported  = newError(http.StatusUnprocessableEntity, "Unsupported transcription fields; streaming and prompts are not supported")
	errDuplicate    = newError(http.StatusBadRequest, "Duplicate multipart field")
	errModel        = newError(http.StatusBadRequest, "Unknown model; use parakeet")
	errTemperature  = newError(http.StatusUnprocessableEntity, "Only greedy decoding is supported")
	errFormat       = newError(http.StatusBadRequest, "Invalid response format or timestamp granularity")
	errNoFile       = newError(http.StatusBadRequest, "file must be an audio file")
	errForm         = newError(http.StatusBadRequest, "Invalid multipart form")
	errSyncTimeout  = newError(http.StatusGatewayTimeout, "Transcription wait timed out; use the asynchronous /v2 API")
	errDisconnected = newError(499, "Client disconnected")
)

var (
	formFields      = []string{"file", "model", "language", "response_format", "timestamp_granularities[]", "temperature"}
	models          = []string{"parakeet", "parakeet-tdt-0.6b-v3", "whisper-1"}
	responseFormats = []string{"json", "text", "verbose_json", "srt", "vtt"}
)

// transcriptions runs a synchronous transcription. Its job and audio are gone when it returns.
func (a *API) transcriptions(w http.ResponseWriter, r *http.Request) {
	fields, uploadID, err := a.readForm(r)
	var opts transcriptionOptions
	if err == nil {
		opts, err = parseOptions(fields, uploadID != "")
	}
	var job store.Job
	if err == nil {
		job, err = a.store.Submit(uploadID, opts.language)
	}
	if err != nil {
		a.store.DiscardUpload(uploadID)
		a.fail(w, r, err)
		return
	}
	defer a.store.Delete(job.ID)
	result, err := a.wait(r.Context(), job.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	switch opts.format {
	case "json":
		writeJSON(w, http.StatusOK, map[string]string{"text": result.Text})
	case "verbose_json":
		writeJSON(w, http.StatusOK, formats.NewVerbose(result, opts.words, opts.segments))
	case "text":
		writeText(w, "text/plain; charset=utf-8", result.Text)
	case "srt":
		writeText(w, "text/plain; charset=utf-8", formats.Captions(result.Words, false, 80))
	case "vtt":
		writeText(w, "text/vtt; charset=utf-8", formats.Captions(result.Words, true, 80))
	}
}

// readForm reads a multipart form, streaming the file part into a new upload. The body is read
// to its end so that the server notices a client that disconnects while waiting.
func (a *API) readForm(r *http.Request) (fields map[string][]string, uploadID string, err error) {
	defer func() {
		if b, ok := r.Body.(*bodyReader); ok && b.err != nil {
			err = b.err // a failing body explains any parse error
		}
		if err != nil {
			a.store.DiscardUpload(uploadID)
			uploadID = ""
		}
	}()
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", errForm
	}
	fields = map[string][]string{}
	count := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, uploadID, errForm
		}
		if part.FileName() != "" {
			if part.FormName() != "file" || uploadID != "" {
				return nil, uploadID, errForm
			}
			src := &tracked{Reader: part}
			id, err := a.store.Save(src)
			switch {
			case src.err != nil:
				return nil, "", errForm
			case err != nil:
				return nil, "", err
			}
			uploadID = id
			continue
		}
		count++
		value, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
		if err != nil || count > maxFormFields || len(value) > maxFieldBytes {
			return nil, uploadID, errForm
		}
		fields[part.FormName()] = append(fields[part.FormName()], string(value))
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, uploadID, errForm
	}
	return fields, uploadID, nil
}

type transcriptionOptions struct {
	format          string
	words, segments bool
	language        string
}

func parseOptions(fields map[string][]string, hasFile bool) (transcriptionOptions, error) {
	for name := range fields {
		if !slices.Contains(formFields, name) {
			return transcriptionOptions{}, errUnsupported
		}
	}
	for name, values := range fields {
		if len(values) > 1 && name != "timestamp_granularities[]" {
			return transcriptionOptions{}, errDuplicate
		}
	}
	first := func(name string) (string, bool) {
		if values := fields[name]; len(values) > 0 {
			return values[0], true
		}
		return "", false
	}
	opts := transcriptionOptions{format: "json"}
	if model, ok := first("model"); ok && !slices.Contains(models, model) {
		return opts, errModel
	}
	if t, ok := first("temperature"); ok && t != "0" && t != "0.0" {
		return opts, errTemperature
	}
	if format, ok := first("response_format"); ok {
		opts.format = format
	}
	if !slices.Contains(responseFormats, opts.format) {
		return opts, errFormat
	}
	for _, g := range fields["timestamp_granularities[]"] {
		switch g {
		case "word":
			opts.words = true
		case "segment":
			opts.segments = true
		default:
			return opts, errFormat
		}
	}
	opts.segments = opts.segments || !opts.words
	if language, _ := first("language"); language != "" {
		if !formats.ValidLanguage(language) {
			return opts, errInvalidOptions
		}
		opts.language = language
	}
	if _, plain := first("file"); plain || !hasFile {
		return opts, errNoFile
	}
	return opts, nil
}

// wait polls the job until it completes or fails, the client disconnects or the sync timeout hits.
func (a *API) wait(ctx context.Context, id string) (*formats.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.SyncTimeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		job, err := a.store.Get(id)
		switch {
		case err != nil:
			return nil, err
		case job.Status == store.StatusError:
			return nil, newError(http.StatusUnprocessableEntity, job.Error)
		case job.Result != nil:
			return job.Result, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errSyncTimeout
			}
			return nil, errDisconnected
		case <-ticker.C:
		}
	}
}

// tracked records the first read error, to tell failures of a source from failures to store it.
type tracked struct {
	io.Reader
	err error
}

func (t *tracked) Read(p []byte) (int, error) {
	n, err := t.Reader.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}
