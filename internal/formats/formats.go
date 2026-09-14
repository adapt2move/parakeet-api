// Package formats validates worker results and renders them in the AssemblyAI and OpenAI response
// shapes, byte-compatible with the Python API it replaces.
//
// JSON produced here matches Starlette's JSONResponse (compact separators, ensure_ascii=False,
// Python float repr). Write the bytes from MarshalJSON directly: encoding/json would re-escape
// <, > and & and change the output.
package formats

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Languages are the language_code values accepted as caller labels.
var Languages = strings.Fields("bg hr cs da nl en et fi fr de el hu it lv lt mt pl pt ro ru sk sl es sv uk")

// ValidLanguage reports whether code is an accepted language_code.
func ValidLanguage(code string) bool {
	for _, language := range Languages {
		if code == language {
			return true
		}
	}
	return false
}

// MarshalJSON renders the result as Python stored it: model_dump() with null speaker and channel.
func (r *Result) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 160+len(r.Text)+wordsSize(r.Words))
	b = append(b, `{"text":`...)
	b = AppendString(b, r.Text)
	b = append(b, `,"words":`...)
	b = appendWords(b, r.Words)
	b = append(b, `,"audio_duration_ms":`...)
	b = strconv.AppendInt(b, r.AudioDurationMS, 10)
	b = append(b, `,"chunks":`...)
	b = strconv.AppendInt(b, r.Chunks, 10)
	b = append(b, `,"seam_fallbacks":`...)
	b = strconv.AppendInt(b, r.SeamFallbacks, 10)
	return append(b, '}'), nil
}

// wordsSize estimates the rendered size of words, so that a buffer is allocated once instead of
// growing through every doubling for a large transcript.
func wordsSize(words []Word) int {
	n := 2
	for _, w := range words {
		n += len(w.Text) + 100
	}
	return n
}

func appendWords(b []byte, words []Word) []byte {
	if words == nil {
		return append(b, "null"...)
	}
	b = append(b, '[')
	for i, w := range words {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"text":`...)
		b = AppendString(b, w.Text)
		b = append(b, `,"start":`...)
		b = strconv.AppendInt(b, w.Start, 10)
		b = append(b, `,"end":`...)
		b = strconv.AppendInt(b, w.End, 10)
		b = append(b, `,"confidence":`...)
		b = AppendFloat(b, w.Confidence)
		b = append(b, `,"speaker":null,"channel":null}`...)
	}
	return append(b, ']')
}

// Job is the queue state Assembly needs. LanguageCode and Error are nil when unset; Result is
// nil until the job completed.
type Job struct {
	ID           string
	Status       string
	Error        *string
	UploadID     string
	LanguageCode *string
	Result       *Result
}

// Transcript is the AssemblyAI transcript object. A nil Text, Words, Confidence or AudioDuration
// renders as null; a non-nil empty Words renders as [].
type Transcript struct {
	ID            string
	Status        string
	Error         *string
	AudioURL      string
	Text          *string
	Words         []Word
	Confidence    *float64
	AudioDuration *int64
	LanguageCode  *string
}

// Assembly builds the AssemblyAI transcript for a job.
func Assembly(job Job, publicURL string) Transcript {
	t := Transcript{
		ID:           job.ID,
		Status:       job.Status,
		Error:        job.Error,
		AudioURL:     publicURL + "/uploads/" + job.UploadID,
		LanguageCode: job.LanguageCode,
	}
	if r := job.Result; r != nil {
		text := r.Text
		t.Text = &text
		t.Words = r.Words
		if t.Words == nil {
			t.Words = []Word{}
		}
		if len(r.Words) > 0 {
			// Python 3.11 sum(): plain left-to-right addition.
			sum := 0.0
			for _, w := range r.Words {
				sum += w.Confidence
			}
			mean := sum / float64(len(r.Words))
			t.Confidence = &mean
		}
		duration := int64(math.Ceil(float64(r.AudioDurationMS) / 1000))
		t.AudioDuration = &duration
	}
	return t
}

// Deleted is the DELETE response: the transcript with status completed and text and words
// removed.
func (t Transcript) Deleted() Transcript {
	t.Status, t.Text, t.Words = "completed", nil, nil
	return t
}

// MarshalJSON renders the transcript as the Python API did.
func (t Transcript) MarshalJSON() ([]byte, error) {
	size := 300 + len(t.ID) + len(t.AudioURL) + wordsSize(t.Words)
	if t.Text != nil {
		size += len(*t.Text)
	}
	b := make([]byte, 0, size)
	b = append(b, `{"id":`...)
	b = AppendString(b, t.ID)
	b = append(b, `,"status":`...)
	b = AppendString(b, t.Status)
	b = append(b, `,"error":`...)
	b = appendOptionalString(b, t.Error)
	b = append(b, `,"audio_url":`...)
	b = AppendString(b, t.AudioURL)
	b = append(b, `,"text":`...)
	b = appendOptionalString(b, t.Text)
	b = append(b, `,"words":`...)
	b = appendWords(b, t.Words)
	b = append(b, `,"confidence":`...)
	if t.Confidence == nil {
		b = append(b, "null"...)
	} else {
		b = AppendFloat(b, *t.Confidence)
	}
	b = append(b, `,"audio_duration":`...)
	if t.AudioDuration == nil {
		b = append(b, "null"...)
	} else {
		b = strconv.AppendInt(b, *t.AudioDuration, 10)
	}
	b = append(b, `,"language_code":`...)
	b = appendOptionalString(b, t.LanguageCode)
	return append(b, `,"language_confidence":null,"utterances":null}`...), nil
}

func appendOptionalString(b []byte, s *string) []byte {
	if s == nil {
		return append(b, "null"...)
	}
	return AppendString(b, *s)
}

// Segment is a caption-sized group of words, in seconds.
type Segment struct {
	ID    int
	Start float64
	End   float64
	Text  string
}

// Segments groups words into captions of at most maxChars code points, splitting when a caption
// would span more than 6 s or a pause exceeds 1.5 s. A single word longer than maxChars still
// forms its own caption.
func Segments(r *Result, maxChars int) []Segment {
	segments := []Segment{}
	eachSegment(r.Words, maxChars, func(words []Word) {
		texts := make([]string, len(words))
		for i, w := range words {
			texts[i] = w.Text
		}
		segments = append(segments, Segment{
			ID:    len(segments),
			Start: float64(words[0].Start) / 1000,
			End:   float64(words[len(words)-1].End) / 1000,
			Text:  strings.Join(texts, " "),
		})
	})
	return segments
}

// eachSegment calls emit with the words of each caption, in order. The slices share words.
func eachSegment(words []Word, maxChars int, emit func([]Word)) {
	first := 0
	chars := 0 // code points in words[first:i] joined by spaces
	for i, w := range words {
		length := utf8.RuneCountInString(w.Text)
		if i > first && (chars+length+1 > maxChars ||
			w.End-words[first].Start > 6000 ||
			w.Start-words[i-1].End > 1500) {
			emit(words[first:i])
			first, chars = i, 0
		}
		if i > first {
			chars++
		}
		chars += length
	}
	if first < len(words) {
		emit(words[first:])
	}
}

func appendSegments(b []byte, segments []Segment) []byte {
	b = append(b, '[')
	for i, s := range segments {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"id":`...)
		b = strconv.AppendInt(b, int64(s.ID), 10)
		b = append(b, `,"start":`...)
		b = AppendFloat(b, s.Start)
		b = append(b, `,"end":`...)
		b = AppendFloat(b, s.End)
		b = append(b, `,"text":`...)
		b = AppendString(b, s.Text)
		b = append(b, '}')
	}
	return append(b, ']')
}

// Timestamp formats seconds as HH:MM:SS,mmm (SRT) or HH:MM:SS.mmm (VTT), rounding half to even
// like Python's round().
func Timestamp(seconds float64, vtt bool) string {
	return string(appendTimestamp(nil, seconds, vtt))
}

func appendTimestamp(b []byte, seconds float64, vtt bool) []byte {
	ms := int64(math.RoundToEven(seconds * 1000))
	hours, ms := ms/3600000, ms%3600000
	minutes, ms := ms/60000, ms%60000
	secs, ms := ms/1000, ms%1000
	separator := byte(',')
	if vtt {
		separator = '.'
	}
	b = appendPadded(b, hours, 2)
	b = append(b, ':')
	b = appendPadded(b, minutes, 2)
	b = append(b, ':')
	b = appendPadded(b, secs, 2)
	b = append(b, separator)
	return appendPadded(b, ms, 3)
}

func appendPadded(b []byte, n int64, width int) []byte {
	var digits [20]byte
	s := strconv.AppendInt(digits[:0], n, 10)
	for i := len(s); i < width; i++ {
		b = append(b, '0')
	}
	return append(b, s...)
}

// Subtitles renders SRT or WebVTT captions of at most maxChars code points per caption.
func Subtitles(r *Result, vtt bool, maxChars int) string {
	return string(AppendSubtitles(nil, r, vtt, maxChars))
}

// AppendSubtitles appends the captions Subtitles renders to b.
func AppendSubtitles(b []byte, r *Result, vtt bool, maxChars int) []byte {
	// Words, a separator per word and about 50 bytes of numbering and timestamps per caption.
	b = slices.Grow(b, len(r.Text)+len(r.Words)*12+16)
	if vtt {
		b = append(b, "WEBVTT\n"...)
	}
	n := 0
	eachSegment(r.Words, maxChars, func(words []Word) {
		if n > 0 || vtt {
			b = append(b, '\n')
		}
		n++
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, '\n')
		b = appendTimestamp(b, float64(words[0].Start)/1000, vtt)
		b = append(b, " --> "...)
		b = appendTimestamp(b, float64(words[len(words)-1].End)/1000, vtt)
		b = append(b, '\n')
		// Model text must not inject subtitle markup or cue separators.
		for i, w := range words {
			if i > 0 {
				b = append(b, ' ')
			}
			b = appendEscaped(b, w.Text)
		}
		b = append(b, '\n')
	})
	return b
}

func appendEscaped(b []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		var entity string
		switch s[i] {
		case '&':
			entity = "&amp;"
		case '<':
			entity = "&lt;"
		case '>':
			entity = "&gt;"
		default:
			continue
		}
		b = append(b, s[start:i]...)
		b = append(b, entity...)
		start = i + 1
	}
	return append(b, s[start:]...)
}

// VerboseWord is one word in the OpenAI verbose_json response.
type VerboseWord struct {
	Word  string
	Start float64
	End   float64
}

// Verbose is the OpenAI verbose_json response. Words and Segments are omitted when nil.
type Verbose struct {
	Duration float64
	Text     string
	Words    []VerboseWord
	Segments []Segment
}

// OpenAI builds the verbose_json response for the requested timestamp granularities.
func OpenAI(r *Result, granularities []string) Verbose {
	v := Verbose{Duration: float64(r.AudioDurationMS) / 1000, Text: r.Text}
	for _, g := range granularities {
		switch {
		case g == "word" && v.Words == nil:
			v.Words = make([]VerboseWord, len(r.Words))
			for i, w := range r.Words {
				v.Words[i] = VerboseWord{Word: w.Text, Start: float64(w.Start) / 1000, End: float64(w.End) / 1000}
			}
		case g == "segment" && v.Segments == nil:
			v.Segments = Segments(r, 80)
		}
	}
	return v
}

// MarshalJSON renders the verbose response as the Python API did.
func (v Verbose) MarshalJSON() ([]byte, error) {
	b := []byte(`{"task":"transcribe","duration":`)
	b = AppendFloat(b, v.Duration)
	b = append(b, `,"text":`...)
	b = AppendString(b, v.Text)
	if v.Words != nil {
		b = append(b, `,"words":[`...)
		for i, w := range v.Words {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"word":`...)
			b = AppendString(b, w.Word)
			b = append(b, `,"start":`...)
			b = AppendFloat(b, w.Start)
			b = append(b, `,"end":`...)
			b = AppendFloat(b, w.End)
			b = append(b, '}')
		}
		b = append(b, ']')
	}
	if v.Segments != nil {
		b = append(b, `,"segments":`...)
		b = appendSegments(b, v.Segments)
	}
	return append(b, '}'), nil
}

// TextJSON renders the OpenAI json response {"text": ...}.
func TextJSON(text string) []byte {
	b := []byte(`{"text":`)
	b = AppendString(b, text)
	return append(b, '}')
}
