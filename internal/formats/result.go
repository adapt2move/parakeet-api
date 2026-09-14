package formats

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrResultOrError matches the 400 "Provide exactly one of result or error" response.
var ErrResultOrError = errors.New("formats: provide exactly one of result or error")

// Limits enforced on worker results.
const (
	MaxWordText        = 4096
	MaxResultText      = 2_000_000
	MaxWords           = 100_000
	MaxAudioDurationMS = 10_800_000
	MaxWorkerError     = 200
	MaxAudioURL        = 8192
)

// Word is one decoder-aligned word in milliseconds. Speaker and channel are always null.
type Word struct {
	Text       string
	Start      int64
	End        int64
	Confidence float64
}

// Result is a validated worker transcript. Chunks and SeamFallbacks saturate at math.MaxInt64;
// Python accepted arbitrarily large values but never exposed them.
type Result struct {
	Text            string
	Words           []Word
	AudioDurationMS int64
	Chunks          int64
	SeamFallbacks   int64
}

// Completion is the body of POST /internal/jobs/{id}/complete.
type Completion struct {
	Result *Result
	Error  *string
	Retry  bool
}

// Submission is the body of POST /v2/transcript. LanguageCode is nil for null or absent.
type Submission struct {
	AudioURL     string
	LanguageCode *string
}

// DecodeCompletion decodes and validates a completion body the way the Python API did. Errors
// are ErrBodyParse (400), ErrInvalid (422) or ErrResultOrError (400), checked in that order.
func DecodeCompletion(contentType string, body []byte) (Completion, error) {
	v, err := decodeRequest(contentType, body)
	if err != nil {
		return Completion{}, err
	}
	var f [3]*value
	if !model(&v, completionFields, f[:]) {
		return Completion{}, ErrInvalid
	}
	var c Completion
	var ok bool
	if !isNull(f[0]) {
		if c.Result, ok = resultFrom(f[0]); !ok {
			return Completion{}, ErrInvalid
		}
	}
	if !isNull(f[1]) {
		s, ok := stringFrom(f[1], 0, MaxWorkerError)
		if !ok {
			return Completion{}, ErrInvalid
		}
		c.Error = &s
	}
	if f[2] != nil {
		if c.Retry, ok = boolFrom(f[2]); !ok {
			return Completion{}, ErrInvalid
		}
	}
	if (c.Result == nil) == (c.Error == nil) {
		return Completion{}, ErrResultOrError
	}
	return c, nil
}

// DecodeSubmission decodes a /v2/transcript body. Errors are ErrBodyParse or ErrInvalid; the
// language is not checked here (see ValidLanguage).
func DecodeSubmission(contentType string, body []byte) (Submission, error) {
	v, err := decodeRequest(contentType, body)
	if err != nil {
		return Submission{}, err
	}
	var f [2]*value
	if !model(&v, submissionFields, f[:]) || f[0] == nil {
		return Submission{}, ErrInvalid
	}
	var s Submission
	var ok bool
	if s.AudioURL, ok = stringFrom(f[0], 0, MaxAudioURL); !ok {
		return Submission{}, ErrInvalid
	}
	if !isNull(f[1]) {
		code, ok := stringFrom(f[1], 0, -1)
		if !ok {
			return Submission{}, ErrInvalid
		}
		s.LanguageCode = &code
	}
	return s, nil
}

// DecodeResult parses a stored or submitted Result document (UTF-8 JSON) and validates it.
func DecodeResult(raw []byte) (*Result, error) {
	text, surrogates, err := decodeUTF8(raw)
	if err != nil {
		return nil, err
	}
	v, err := parseText(text, surrogates)
	if err != nil {
		return nil, err
	}
	r, ok := resultFrom(&v)
	if !ok {
		return nil, ErrInvalid
	}
	return r, nil
}

// model maps an object's members onto names, leaving nil for absent ones. It fails for
// non-objects and unknown members. Duplicate keys resolve to the last occurrence.
func model(v *value, names []string, out []*value) bool {
	if v.kind != kindObject {
		return false
	}
	for i := range v.items {
		member := &v.items[i]
		known := false
		for n, name := range names {
			if member.key == name {
				out[n], known = member, true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

var (
	completionFields = []string{"result", "error", "retry"}
	submissionFields = []string{"audio_url", "language_code"}
	resultFields     = []string{"text", "words", "audio_duration_ms", "chunks", "seam_fallbacks"}
	wordFields       = []string{"text", "start", "end", "confidence", "speaker", "channel"}
)

func isNull(v *value) bool { return v == nil || v.kind == kindNull }

func resultFrom(v *value) (*Result, bool) {
	var f [5]*value
	if !model(v, resultFields, f[:]) || f[0] == nil || f[1] == nil || f[2] == nil || f[3] == nil || f[4] == nil {
		return nil, false
	}
	text, words, duration, chunks, seams := f[0], f[1], f[2], f[3], f[4]
	var ok bool
	r := &Result{}
	if r.Text, ok = stringFrom(text, 0, MaxResultText); !ok {
		return nil, false
	}
	if r.AudioDurationMS, ok = intFrom(duration); !ok || r.AudioDurationMS <= 0 || r.AudioDurationMS > MaxAudioDurationMS {
		return nil, false
	}
	if r.Chunks, ok = intFrom(chunks); !ok || r.Chunks < 1 {
		return nil, false
	}
	if r.SeamFallbacks, ok = intFrom(seams); !ok || r.SeamFallbacks < 0 {
		return nil, false
	}
	if words.kind != kindArray || len(words.items) > MaxWords {
		return nil, false
	}
	r.Words = make([]Word, len(words.items))
	for i := range words.items {
		if r.Words[i], ok = wordFrom(&words.items[i]); !ok {
			return nil, false
		}
	}
	// Result.aligned: words sit inside the audio, never move backwards, and spell the text.
	prevStart, prevEnd := int64(-1), int64(-1)
	for _, w := range r.Words {
		if !(w.Start < w.End && w.End <= r.AudioDurationMS) {
			return nil, false
		}
		if w.Start < prevStart || w.End < prevEnd {
			return nil, false
		}
		prevStart, prevEnd = w.Start, w.End
	}
	if !textMatches(r.Text, r.Words) {
		return nil, false
	}
	return r, true
}

func textMatches(text string, words []Word) bool {
	for i, w := range words {
		if i > 0 {
			if !strings.HasPrefix(text, " ") {
				return false
			}
			text = text[1:]
		}
		if !strings.HasPrefix(text, w.Text) {
			return false
		}
		text = text[len(w.Text):]
	}
	return text == ""
}

func wordFrom(v *value) (Word, bool) {
	var f [6]*value
	if !model(v, wordFields, f[:]) || f[0] == nil || f[1] == nil || f[2] == nil || f[3] == nil || !isNull(f[4]) || !isNull(f[5]) {
		return Word{}, false
	}
	var w Word
	var ok bool
	if w.Text, ok = stringFrom(f[0], 1, MaxWordText); !ok {
		return Word{}, false
	}
	if w.Start, ok = intFrom(f[1]); !ok || w.Start < 0 {
		return Word{}, false
	}
	if w.End, ok = intFrom(f[2]); !ok || w.End <= 0 {
		return Word{}, false
	}
	if w.Confidence, ok = floatFrom(f[3]); !ok || !(w.Confidence >= 0 && w.Confidence <= 1) {
		return Word{}, false
	}
	return w, true
}

// stringFrom accepts only JSON strings, with lengths in code points; max < 0 means unbounded.
func stringFrom(v *value, min, max int) (string, bool) {
	if v.kind != kindString {
		return "", false
	}
	n := utf8.RuneCountInString(v.s)
	return v.s, n >= min && (max < 0 || n <= max)
}

// intFrom follows pydantic's lax int: integers, integral floats inside int64, booleans, and
// strings holding a decimal integer with an optional all-zero fraction.
func intFrom(v *value) (int64, bool) {
	switch v.kind {
	case kindBool:
		if v.b {
			return 1, true
		}
		return 0, true
	case kindInt:
		return saturate(v.s), true
	case kindFloat:
		f, ok := finite(v.s)
		if !ok || f != math.Trunc(f) || f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
			return 0, false
		}
		return int64(f), true
	case kindString:
		s := strings.TrimFunc(v.s, unicode.IsSpace)
		sign := ""
		if s != "" && (s[0] == '+' || s[0] == '-') {
			sign, s = s[:1], s[1:]
		}
		whole, fraction, dotted := strings.Cut(s, ".")
		if whole == "" || !allDigits(whole) || (dotted && (fraction == "" || strings.Trim(fraction, "0") != "")) {
			return 0, false
		}
		if len(strings.TrimLeft(whole, "0")) > maxIntDigits {
			return 0, false
		}
		if sign == "+" {
			sign = ""
		}
		return saturate(sign + whole), true
	}
	return 0, false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// saturate parses a decimal integer literal, clamping to the int64 range. Bounds checks stay
// correct because every clamped value keeps its sign and exceeds all limits.
func saturate(lit string) int64 {
	n, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		if strings.HasPrefix(lit, "-") {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return n
}

func finite(lit string) (float64, bool) {
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, !math.IsInf(f, 0) && !math.IsNaN(f)
}

// floatFrom follows pydantic's lax float with allow_inf_nan=False.
func floatFrom(v *value) (float64, bool) {
	switch v.kind {
	case kindBool:
		if v.b {
			return 1, true
		}
		return 0, true
	case kindInt:
		// Python ints have no negative zero; huge values overflow and fail the bounds anyway.
		f, err := strconv.ParseFloat(v.s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, false
		}
		if f == 0 {
			f = 0
		}
		return f, !math.IsInf(f, 0)
	case kindFloat:
		return finite(v.s)
	case kindString:
		s := strings.TrimFunc(v.s, unicode.IsSpace)
		if !floatSyntax(s) {
			return 0, false
		}
		return finite(s)
	}
	return 0, false
}

// floatSyntax matches [+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?
func floatSyntax(s string) bool {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	mantissa, exponent, hasExp := strings.Cut(strings.ReplaceAll(s, "E", "e"), "e")
	whole, fraction, _ := strings.Cut(mantissa, ".")
	if !allDigits(whole) || !allDigits(fraction) || whole+fraction == "" {
		return false
	}
	if hasExp {
		if exponent != "" && (exponent[0] == '+' || exponent[0] == '-') {
			exponent = exponent[1:]
		}
		if exponent == "" || !allDigits(exponent) {
			return false
		}
	}
	return true
}

// boolFrom follows pydantic's lax bool.
func boolFrom(v *value) (bool, bool) {
	switch v.kind {
	case kindBool:
		return v.b, true
	case kindInt:
		switch saturate(v.s) {
		case 0:
			return false, true
		case 1:
			return true, true
		}
	case kindFloat:
		f, ok := finite(v.s)
		if ok && f == 0 {
			return false, true
		}
		if ok && f == 1 {
			return true, true
		}
	case kindString:
		switch strings.ToLower(v.s) {
		case "0", "off", "f", "false", "n", "no":
			return false, true
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		}
	}
	return false, false
}
