// Package formats defines the worker result and renders it as captions and OpenAI responses.
package formats

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits enforced on worker results.
const (
	MaxWordChars = 4096
	MaxTextChars = 2_000_000
	MaxWords     = 100_000
	MaxAudioMS   = 10_800_000
)

var errInvalid = errors.New("invalid JSON object")

// Languages are the accepted language codes.
var Languages = strings.Fields("bg hr cs da nl en et fi fr de el hu it lv lt mt pl pt ro ru sk sl es sv uk")

// ValidLanguage reports whether code is one of Languages.
func ValidLanguage(code string) bool { return slices.Contains(Languages, code) }

// Word is one aligned word, in milliseconds.
type Word struct {
	Text       string    `json:"text"`
	Start      int64     `json:"start"`
	End        int64     `json:"end"`
	Confidence float64   `json:"confidence"`
	Speaker    *struct{} `json:"speaker"` // nil in a valid word, so it encodes as null
	Channel    *struct{} `json:"channel"` // likewise
}

// UnmarshalJSON decodes a word with DecodeObject.
func (w *Word) UnmarshalJSON(data []byte) error {
	type plain Word
	return DecodeObject(data, (*plain)(w), []string{"text", "start", "end", "confidence"}, []string{"speaker", "channel"})
}

// Result is the transcript a worker submits.
type Result struct {
	Text            string   `json:"text"`
	Words           wordList `json:"words"`
	AudioDurationMS int64    `json:"audio_duration_ms"`
	Chunks          int64    `json:"chunks"`
	SeamFallbacks   int64    `json:"seam_fallbacks"`
}

// UnmarshalJSON decodes a result with DecodeObject and validates it.
func (r *Result) UnmarshalJSON(data []byte) error {
	type plain Result
	err := DecodeObject(data, (*plain)(r), []string{"text", "words", "audio_duration_ms", "chunks", "seam_fallbacks"}, nil)
	if err == nil && !r.valid() {
		err = errInvalid
	}
	return err
}

// valid checks the limits and that the words are ordered, inside the audio and spell the text.
func (r *Result) valid() bool {
	if len(r.Words) > MaxWords || utf8.RuneCountInString(r.Text) > MaxTextChars ||
		r.AudioDurationMS <= 0 || r.AudioDurationMS > MaxAudioMS || r.Chunks < 1 || r.SeamFallbacks < 0 {
		return false
	}
	for i, w := range r.Words {
		chars := utf8.RuneCountInString(w.Text)
		if chars < 1 || chars > MaxWordChars || w.Start < 0 || w.Start >= w.End || w.End > r.AudioDurationMS ||
			!(w.Confidence >= 0 && w.Confidence <= 1) || w.Speaker != nil || w.Channel != nil {
			return false
		}
		if i > 0 && (w.Start < r.Words[i-1].Start || w.End < r.Words[i-1].End) {
			return false
		}
	}
	return strings.Join(texts(r.Words), " ") == r.Text
}

// wordList decodes at most MaxWords words. It stops at the first word beyond, so that a large
// body cannot allocate millions of them.
type wordList []Word

func (l *wordList) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return errInvalid
	}
	list := []Word{}
	for dec.More() {
		var w Word
		if len(list) == MaxWords || dec.Decode(&w) != nil {
			return errInvalid
		}
		list = append(list, w)
	}
	*l = list
	return nil
}

// DecodeObject decodes the JSON object data into v, a pointer to a struct. Every key must be
// spelled exactly as one of required or optional. Required keys must be present and not null;
// optional keys may be absent or null.
func DecodeObject(data []byte, v any, required, optional []string) error {
	var keys map[string]isNull
	if json.Unmarshal(data, &keys) != nil || keys == nil {
		return errInvalid
	}
	for key, null := range keys {
		if !slices.Contains(optional, key) && (bool(null) || !slices.Contains(required, key)) {
			return errInvalid
		}
	}
	for _, key := range required {
		if _, ok := keys[key]; !ok {
			return errInvalid
		}
	}
	return json.Unmarshal(data, v)
}

// isNull records whether a JSON value is null without keeping the value.
type isNull bool

func (n *isNull) UnmarshalJSON(data []byte) error {
	*n = string(data) == "null"
	return nil
}

func texts(words []Word) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w.Text
	}
	return out
}
