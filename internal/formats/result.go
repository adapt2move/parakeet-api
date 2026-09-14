// Package formats defines the worker result and renders it as captions and OpenAI responses.
package formats

import (
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

// ErrInvalid reports a result that breaks a validation rule.
var ErrInvalid = errors.New("invalid result")

// Languages are the accepted language codes.
var Languages = strings.Fields("bg hr cs da nl en et fi fr de el hu it lv lt mt pl pt ro ru sk sl es sv uk")

// ValidLanguage reports whether code is one of Languages.
func ValidLanguage(code string) bool { return slices.Contains(Languages, code) }

// Word is one aligned word, in milliseconds. Speaker and channel must be null.
type Word struct {
	Text       string  `json:"text"`
	Start      int64   `json:"start"`
	End        int64   `json:"end"`
	Confidence float64 `json:"confidence"`
	Speaker    any     `json:"speaker"`
	Channel    any     `json:"channel"`
}

// Result is the transcript a worker submits.
type Result struct {
	Text            string `json:"text"`
	Words           []Word `json:"words"`
	AudioDurationMS int64  `json:"audio_duration_ms"`
	Chunks          int64  `json:"chunks"`
	SeamFallbacks   int64  `json:"seam_fallbacks"`
}

// Validate checks the limits and that the words are ordered, inside the audio and spell the text.
func (r *Result) Validate() error {
	if r.Words == nil || len(r.Words) > MaxWords || utf8.RuneCountInString(r.Text) > MaxTextChars ||
		r.AudioDurationMS <= 0 || r.AudioDurationMS > MaxAudioMS || r.Chunks < 1 || r.SeamFallbacks < 0 {
		return ErrInvalid
	}
	size := len(r.Words) - 1
	for i, w := range r.Words {
		chars := utf8.RuneCountInString(w.Text)
		if chars < 1 || chars > MaxWordChars || w.Start < 0 || w.Start >= w.End || w.End > r.AudioDurationMS ||
			!(w.Confidence >= 0 && w.Confidence <= 1) || w.Speaker != nil || w.Channel != nil {
			return ErrInvalid
		}
		if i > 0 && (w.Start < r.Words[i-1].Start || w.End < r.Words[i-1].End) {
			return ErrInvalid
		}
		size += len(w.Text)
	}
	// Compare lengths first so that a mismatch never builds a huge joined string.
	if max(size, 0) != len(r.Text) || strings.Join(texts(r.Words), " ") != r.Text {
		return ErrInvalid
	}
	return nil
}

func texts(words []Word) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w.Text
	}
	return out
}
