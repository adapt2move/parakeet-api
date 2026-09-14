package formats

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Fixtures come from testdata/generate.py running the Python reference implementation.

func load(t *testing.T, name string, v any) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(name, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = gz
	}
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// same reports the first difference between Python's and Go's bytes.
func same(t *testing.T, label string, want string, got []byte) {
	t.Helper()
	if want == string(got) {
		return
	}
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	from := max(0, i-40)
	t.Errorf("%s differs at byte %d:\npython: %q\ngo:     %q", label, i, want[from:min(len(want), i+40)], got[from:min(len(got), i+40)])
}

type requestCase struct {
	Name        string  `json:"name"`
	ContentType *string `json:"content_type"`
	Status      int     `json:"status"`
	Body        string  `json:"body"`
	BodyText    *string `json:"body_text"`
	BodyB64     *string `json:"body_b64"`
}

func (c requestCase) raw(t *testing.T) []byte {
	if c.BodyText != nil {
		return []byte(*c.BodyText)
	}
	raw, err := base64.StdEncoding.DecodeString(*c.BodyB64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (c requestCase) contentType() string {
	if c.ContentType == nil {
		return ""
	}
	return *c.ContentType
}

// expectError maps a Python response to the error Go must return.
func expectError(status int, body string) error {
	switch {
	case status == 422:
		return ErrInvalid
	case status == 400 && strings.Contains(body, "There was an error parsing the body"):
		return ErrBodyParse
	case status == 400 && strings.Contains(body, "Provide exactly one of result or error"):
		return ErrResultOrError
	}
	return nil
}

type completionCase struct {
	requestCase
	Stored       *string `json:"stored"`
	StoredSHA256 string  `json:"stored_sha256"`
	Error        *string `json:"error"`
	Retry        bool    `json:"retry"`
	BodySHA256   string  `json:"body_sha256"`
	Recipe       struct {
		Kind     string `json:"kind"`
		UnitJSON string `json:"unit_json"`
		Repeat   int    `json:"repeat"`
		Count    int    `json:"count"`
		Length   int    `json:"length"`
		Last     int    `json:"last"`
	} `json:"recipe"`
}

func checkCompletion(t *testing.T, c completionCase, raw []byte) {
	t.Helper()
	got, err := DecodeCompletion(c.contentType(), raw)
	if c.Status != 200 {
		want := expectError(c.Status, c.Body)
		if want == nil {
			t.Fatalf("%s: unexpected Python status %d %s", c.Name, c.Status, c.Body)
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: python %d %s, go error %v", c.Name, c.Status, c.Body, err)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: python accepted, go error %v", c.Name, err)
		return
	}
	if got.Retry != c.Retry || (got.Error == nil) != (c.Error == nil) || (got.Error != nil && *got.Error != *c.Error) {
		t.Errorf("%s: error/retry = %v/%v, python %v/%v", c.Name, got.Error, got.Retry, c.Error, c.Retry)
	}
	if (got.Result == nil) != (c.Stored == nil && c.StoredSHA256 == "") {
		t.Errorf("%s: result presence differs", c.Name)
		return
	}
	if got.Result == nil {
		return
	}
	stored, _ := got.Result.MarshalJSON()
	if c.Stored != nil {
		same(t, c.Name+" stored", *c.Stored, stored)
	} else if sum := sha256.Sum256(stored); hex.EncodeToString(sum[:]) != c.StoredSHA256 {
		t.Errorf("%s: stored result hash differs", c.Name)
	}
}

func TestCompletionMatchesPython(t *testing.T) {
	var cases []completionCase
	load(t, "completion.json", &cases)
	for _, c := range cases {
		checkCompletion(t, c, c.raw(t))
	}
}

func TestLargeCompletionMatchesPython(t *testing.T) {
	var cases []completionCase
	load(t, "completion_large.json", &cases)
	for _, c := range cases {
		var b strings.Builder
		switch c.Recipe.Kind {
		case "word":
			text := `"` + strings.Repeat(c.Recipe.UnitJSON, c.Recipe.Repeat) + `"`
			fmt.Fprintf(&b, `{"result": {"text": %s, "audio_duration_ms": 1000, "chunks": 1, "seam_fallbacks": 0, "words": [{"text": %s, "start": 0, "end": 10, "confidence": 0.5}]}}`, text, text)
		case "words":
			texts := make([]string, c.Recipe.Count)
			for i := range texts {
				texts[i] = strings.Repeat("x", c.Recipe.Length)
			}
			texts[len(texts)-1] = strings.Repeat("y", c.Recipe.Last)
			fmt.Fprintf(&b, `{"result": {"text": "%s", "audio_duration_ms": 10800000, "chunks": 1, "seam_fallbacks": 0, "words": [`, strings.Join(texts, " "))
			for i, text := range texts {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, `{"text": "%s", "start": %d, "end": %d, "confidence": 0.5}`, text, i*100, i*100+50)
			}
			b.WriteString("]}}")
		}
		raw := []byte(b.String())
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != c.BodySHA256 {
			t.Fatalf("%s: rebuilt body differs from Python's", c.Name)
		}
		checkCompletion(t, c, raw)
	}
}

func TestSubmissionMatchesPython(t *testing.T) {
	var cases []struct {
		requestCase
		AudioURL     *string `json:"audio_url"`
		LanguageCode *string `json:"language_code"`
	}
	load(t, "submission.json", &cases)
	for _, c := range cases {
		got, err := DecodeSubmission(c.contentType(), c.raw(t))
		if want := expectError(c.Status, c.Body); want != nil {
			if !errors.Is(err, want) {
				t.Errorf("%s: python %d %s, go error %v", c.Name, c.Status, c.Body, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: python accepted, go error %v", c.Name, err)
			continue
		}
		// The API only forwards a non-empty language_code.
		var language *string
		if got.LanguageCode != nil && *got.LanguageCode != "" {
			language = got.LanguageCode
		}
		if (language == nil) != (c.LanguageCode == nil) || (language != nil && *language != *c.LanguageCode) {
			t.Errorf("%s: language %v, python %v", c.Name, language, c.LanguageCode)
		}
		switch {
		case c.Status == 400 && strings.Contains(c.Body, "Invalid transcription options"):
			if language == nil || ValidLanguage(*language) {
				t.Errorf("%s: python rejected the language", c.Name)
			}
		case c.Status == 418:
			if language != nil && !ValidLanguage(*language) {
				t.Errorf("%s: python accepted the language", c.Name)
			}
			if c.AudioURL == nil || got.AudioURL != *c.AudioURL {
				t.Errorf("%s: audio_url %q, python %v", c.Name, got.AudioURL, c.AudioURL)
			}
		default:
			t.Errorf("%s: unexpected Python status %d %s", c.Name, c.Status, c.Body)
		}
	}
}

func TestOutputsMatchPython(t *testing.T) {
	var cases []struct {
		Name string `json:"name"`
		Job  struct {
			ID           string  `json:"id"`
			Status       string  `json:"status"`
			Error        *string `json:"error"`
			UploadID     string  `json:"upload_id"`
			LanguageCode *string `json:"language_code"`
			Result       *string `json:"result"`
		} `json:"job"`
		PublicURL string            `json:"public_url"`
		Assembly  string            `json:"assembly"`
		Deleted   string            `json:"deleted"`
		OpenAI    map[string]string `json:"openai"`
		Segments  map[string]string `json:"segments"`
		SRT       map[string]string `json:"srt"`
		VTT       map[string]string `json:"vtt"`
		TextJSON  string            `json:"text_json"`
	}
	load(t, "outputs.json.gz", &cases)
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range cases {
		job := Job{ID: c.Job.ID, Status: c.Job.Status, Error: c.Job.Error, UploadID: c.Job.UploadID, LanguageCode: c.Job.LanguageCode}
		if c.Job.Result != nil {
			r, err := DecodeResult([]byte(*c.Job.Result))
			if err != nil {
				t.Fatalf("%s: stored result rejected: %v", c.Name, err)
			}
			stored, _ := r.MarshalJSON()
			same(t, c.Name+" stored", *c.Job.Result, stored)
			job.Result = r
		}
		transcript := Assembly(job, c.PublicURL)
		got, _ := transcript.MarshalJSON()
		same(t, c.Name+" assembly", c.Assembly, got)
		got, _ = transcript.Deleted().MarshalJSON()
		same(t, c.Name+" deleted", c.Deleted, got)
		if job.Result == nil {
			continue
		}
		for key, want := range c.OpenAI {
			granularities := strings.Split(key, ",")
			if key == "" {
				granularities = nil
			}
			got, _ := OpenAI(job.Result, granularities).MarshalJSON()
			same(t, c.Name+" openai "+key, want, got)
		}
		for n, want := range c.Segments {
			chars, _ := strconv.Atoi(n)
			same(t, c.Name+" segments "+n, want, appendSegments(nil, Segments(job.Result, chars)))
		}
		for n, want := range c.SRT {
			chars, _ := strconv.Atoi(n)
			same(t, c.Name+" srt "+n, want, []byte(Subtitles(job.Result, false, chars)))
		}
		for n, want := range c.VTT {
			chars, _ := strconv.Atoi(n)
			same(t, c.Name+" vtt "+n, want, []byte(Subtitles(job.Result, true, chars)))
		}
		if c.TextJSON != "" {
			same(t, c.Name+" text", c.TextJSON, TextJSON(job.Result.Text))
		}
	}
}

func TestEncodingMatchesPython(t *testing.T) {
	var fixture struct {
		Floats []struct {
			Hex  string `json:"hex"`
			JSON string `json:"json"`
		} `json:"floats"`
		Strings []struct {
			Value string `json:"value"`
			JSON  string `json:"json"`
		} `json:"strings"`
		Timestamps []struct {
			Hex string `json:"hex"`
			SRT string `json:"srt"`
			VTT string `json:"vtt"`
		} `json:"timestamps"`
		Languages []struct {
			Code  string `json:"code"`
			Valid bool   `json:"valid"`
		} `json:"languages"`
	}
	load(t, "encoding.json", &fixture)
	parse := func(s string) float64 {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, c := range fixture.Floats {
		same(t, "float "+c.Hex, c.JSON, AppendFloat(nil, parse(c.Hex)))
	}
	for _, c := range fixture.Strings {
		same(t, fmt.Sprintf("string %q", c.Value), c.JSON, AppendString(nil, c.Value))
	}
	for _, c := range fixture.Timestamps {
		f := parse(c.Hex)
		if got := Timestamp(f, false); got != c.SRT {
			t.Errorf("Timestamp(%v, srt) = %s, python %s", f, got, c.SRT)
		}
		if got := Timestamp(f, true); got != c.VTT {
			t.Errorf("Timestamp(%v, vtt) = %s, python %s", f, got, c.VTT)
		}
	}
	for _, c := range fixture.Languages {
		if ValidLanguage(c.Code) != c.Valid {
			t.Errorf("ValidLanguage(%q) = %v", c.Code, !c.Valid)
		}
	}
}

func TestMarshalOutputIsNotHTMLEscaped(t *testing.T) {
	r := &Result{Text: "<a> & b", Words: []Word{{Text: "<a>", Start: 0, End: 1, Confidence: 1}, {Text: "&", Start: 1, End: 2}, {Text: "b", Start: 2, End: 3}}, AudioDurationMS: 3, Chunks: 1}
	got, _ := Assembly(Job{ID: "id", Status: "completed", UploadID: "u", Result: r}, "http://x").MarshalJSON()
	if !bytes.Contains(got, []byte(`"text":"<a> & b"`)) || !bytes.Contains(got, []byte(`"confidence":0.3333333333333333`)) {
		t.Fatalf("unexpected output %s", got)
	}
	if !json.Valid(got) {
		t.Fatal("invalid JSON")
	}
}

func TestSaturatedCounts(t *testing.T) {
	r, err := DecodeResult([]byte(`{"text": "", "words": [], "audio_duration_ms": 1, "chunks": 1e18, "seam_fallbacks": 99999999999999999999}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Chunks != 1_000_000_000_000_000_000 || r.SeamFallbacks != math.MaxInt64 {
		t.Fatalf("got %d %d", r.Chunks, r.SeamFallbacks)
	}
}
