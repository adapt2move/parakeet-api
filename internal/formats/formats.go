// Package formats validates worker results and renders them in the AssemblyAI and OpenAI response
// shapes, byte-compatible with the Python API it replaces.
//
// JSON produced here matches Starlette's JSONResponse (compact separators, ensure_ascii=False,
// Python float repr). Write the bytes from MarshalJSON directly: encoding/json would re-escape
// <, > and & and change the output.
package formats

import (
	"math"
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
	b := []byte(`{"text":`)
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
	b := []byte(`{"id":`)
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
	var current []Word
	chars := 0 // code points in the current words joined by spaces
	flush := func() {
		texts := make([]string, len(current))
		for i, w := range current {
			texts[i] = w.Text
		}
		segments = append(segments, Segment{
			ID:    len(segments),
			Start: float64(current[0].Start) / 1000,
			End:   float64(current[len(current)-1].End) / 1000,
			Text:  strings.Join(texts, " "),
		})
	}
	for _, w := range r.Words {
		length := utf8.RuneCountInString(w.Text)
		if len(current) > 0 && (chars+length+1 > maxChars ||
			w.End-current[0].Start > 6000 ||
			w.Start-current[len(current)-1].End > 1500) {
			flush()
			current, chars = nil, 0
		}
		if len(current) > 0 {
			chars++
		}
		chars += length
		current = append(current, w)
	}
	if len(current) > 0 {
		flush()
	}
	return segments
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
	ms := int64(math.RoundToEven(seconds * 1000))
	hours, ms := ms/3600000, ms%3600000
	minutes, ms := ms/60000, ms%60000
	secs, ms := ms/1000, ms%1000
	separator := ","
	if vtt {
		separator = "."
	}
	return pad(hours, 2) + ":" + pad(minutes, 2) + ":" + pad(secs, 2) + separator + pad(ms, 3)
}

func pad(n int64, width int) string {
	s := strconv.FormatInt(n, 10)
	if len(s) < width {
		s = strings.Repeat("0", width-len(s)) + s
	}
	return s
}

var subtitleEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Subtitles renders SRT or WebVTT captions of at most maxChars code points per caption.
func Subtitles(r *Result, vtt bool, maxChars int) string {
	var blocks []string
	if vtt {
		blocks = append(blocks, "WEBVTT\n")
	}
	for i, s := range Segments(r, maxChars) {
		// Model text must not inject subtitle markup or cue separators.
		blocks = append(blocks, strconv.Itoa(i+1)+"\n"+Timestamp(s.Start, vtt)+" --> "+Timestamp(s.End, vtt)+"\n"+subtitleEscaper.Replace(s.Text)+"\n")
	}
	return strings.Join(blocks, "\n")
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
