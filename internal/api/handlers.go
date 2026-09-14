package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/adapt2move/parakeet-api/internal/formats"
	"github.com/adapt2move/parakeet-api/internal/store"
)

var (
	errInvalidOptions = newError(http.StatusBadRequest, "Invalid transcription options or audio")
	errCharsPerCue    = newError(http.StatusBadRequest, "chars_per_caption must be 20..200")
	errNotComplete    = newError(http.StatusConflict, "Transcript is not complete")
	errMissingLease   = newError(http.StatusConflict, "Missing lease token")
	errResultOrError  = newError(http.StatusBadRequest, "Provide exactly one of result or error")
)

var statusOK = map[string]string{"status": "ok"}

func (a *API) live(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, statusOK) }

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	a.store.Counts() // the queue lock is free
	writeJSON(w, http.StatusOK, statusOK)
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	counts := a.store.Counts()
	var b strings.Builder
	for _, status := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(&b, "parakeet_jobs{status=%q} %d\n", status, counts[status])
	}
	writeText(w, "text/plain; version=0.0.4; charset=utf-8", b.String())
}

func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	release, ok := a.acquireSlot()
	if !ok {
		a.fail(w, r, errSlotsFull)
		return
	}
	defer release()
	id, err := a.store.Save(r.Body)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	release()
	writeJSON(w, http.StatusOK, map[string]string{"upload_url": a.cfg.PublicURL + "/uploads/" + id})
}

func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AudioURL     *string `json:"audio_url"`
		LanguageCode *string `json:"language_code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.AudioURL == nil {
		a.fail(w, r, errInvalidFields)
		return
	}
	language := ""
	if req.LanguageCode != nil && *req.LanguageCode != "" {
		if !formats.ValidLanguage(*req.LanguageCode) {
			a.fail(w, r, errInvalidOptions)
			return
		}
		language = *req.LanguageCode
	}
	uploadID, owned, err := a.resolveAudio(r.Context(), *req.AudioURL)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	job, err := a.store.Submit(uploadID, language)
	if err != nil {
		if owned {
			a.store.DiscardUpload(uploadID)
		}
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.transcript(job))
}

// transcript is the AssemblyAI transcript object.
type transcript struct {
	ID                 string         `json:"id"`
	Status             store.Status   `json:"status"`
	Error              *string        `json:"error"`
	AudioURL           string         `json:"audio_url"`
	Text               *string        `json:"text"`
	Words              []formats.Word `json:"words"`
	Confidence         *float64       `json:"confidence"`
	AudioDuration      *int64         `json:"audio_duration"`
	LanguageCode       *string        `json:"language_code"`
	LanguageConfidence *float64       `json:"language_confidence"`
	Utterances         []any          `json:"utterances"`
}

func (a *API) transcript(job store.Job) transcript {
	t := transcript{ID: job.ID, Status: job.Status, AudioURL: a.cfg.PublicURL + "/uploads/" + job.UploadID}
	if job.Status == store.StatusError {
		t.Error = &job.Error
	}
	if job.Language != "" {
		t.LanguageCode = &job.Language
	}
	if r := job.Result; r != nil {
		t.Text, t.Words = &r.Text, r.Words
		if len(r.Words) > 0 {
			sum := 0.0
			for _, w := range r.Words {
				sum += w.Confidence
			}
			t.Confidence = new(sum / float64(len(r.Words)))
		}
		t.AudioDuration = new((r.AudioDurationMS + 999) / 1000)
	}
	return t
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.Get(r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.transcript(job))
}

// delete answers with the transcript as AssemblyAI does: completed, with text and words removed.
func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.Get(r.PathValue("id"))
	if err == nil {
		err = a.store.Delete(job.ID)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	t := a.transcript(job)
	t.Status, t.Text, t.Words = store.StatusCompleted, nil, nil
	writeJSON(w, http.StatusOK, t)
}

func (a *API) captions(w http.ResponseWriter, r *http.Request) {
	chars := 80
	if query := r.URL.Query(); query.Has("chars_per_caption") {
		n, err := strconv.Atoi(query.Get("chars_per_caption"))
		if err != nil || n < 20 || n > 200 {
			a.fail(w, r, errCharsPerCue)
			return
		}
		chars = n
	}
	job, err := a.store.Get(r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if job.Result == nil {
		a.fail(w, r, errNotComplete)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/vtt") {
		writeText(w, "text/vtt; charset=utf-8", formats.Captions(job.Result.Words, true, chars))
	} else {
		writeText(w, "application/x-subrip", formats.Captions(job.Result.Words, false, chars))
	}
}

func (a *API) claim(w http.ResponseWriter, r *http.Request) {
	if c := a.store.Claim(); c != nil {
		writeJSON(w, http.StatusOK, c)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func leaseToken(r *http.Request) (string, error) {
	if token := r.Header.Get("X-Lease-Token"); token != "" {
		return token, nil
	}
	return "", errMissingLease
}

var okBody = map[string]bool{"ok": true}

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	token, err := leaseToken(r)
	if err == nil {
		err = a.store.Heartbeat(r.PathValue("id"), token)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

func (a *API) audio(w http.ResponseWriter, r *http.Request) {
	token, err := leaseToken(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	f, err := a.store.Audio(r.PathValue("id"), token)
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

func (a *API) complete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Result *formats.Result `json:"result"`
		Error  *string         `json:"error"`
		Retry  bool            `json:"retry"`
	}
	err := decodeJSON(r, &req)
	switch {
	case err != nil:
	case req.Result != nil && req.Result.Validate() != nil, req.Error != nil && utf8.RuneCountInString(*req.Error) > 200:
		err = errInvalidFields
	case (req.Result == nil) == (req.Error == nil):
		err = errResultOrError
	}
	token, tokenErr := leaseToken(r)
	if err == nil {
		err = tokenErr
	}
	if err == nil {
		message := ""
		if req.Error != nil {
			message = *req.Error
		}
		err = a.store.Finish(r.PathValue("id"), token, req.Result, message, req.Retry)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}
