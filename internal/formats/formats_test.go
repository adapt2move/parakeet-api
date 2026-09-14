package formats

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func words(spec ...any) []Word {
	var out []Word
	for i := 0; i < len(spec); i += 3 {
		out = append(out, Word{Text: spec[i].(string), Start: int64(spec[i+1].(int)), End: int64(spec[i+2].(int)), Confidence: 0.5})
	}
	return out
}

func TestValidate(t *testing.T) {
	valid := func() *Result {
		return &Result{Text: "Hello world.", Words: words("Hello", 120, 610, "world.", 900, 1700), AudioDurationMS: 2100, Chunks: 1}
	}
	long := strings.Repeat("a", MaxWordChars)
	for _, tc := range []struct {
		name   string
		change func(*Result)
		ok     bool
	}{
		{"valid", func(*Result) {}, true},
		{"no words", func(r *Result) { r.Text, r.Words = "", []Word{} }, true},
		{"equal timings", func(r *Result) { r.Words[1].Start, r.Words[1].End = 120, 610 }, true},
		{"bounds", func(r *Result) { r.Words[0].Confidence, r.Words[1].Confidence, r.Words[1].End = 0, 1, 2100 }, true},
		{"longest word", func(r *Result) { r.Text, r.Words = long, words(long, 0, 10) }, true},
		{"multibyte word", func(r *Result) {
			r.Text, r.Words = strings.Repeat("é", MaxWordChars), words(strings.Repeat("é", MaxWordChars), 0, 10)
		}, true},
		{"text mismatch", func(r *Result) { r.Text = "Hello  world." }, false},
		{"trailing text", func(r *Result) { r.Text += " " }, false},
		{"empty word", func(r *Result) { r.Text, r.Words = "", words("", 0, 10) }, false},
		{"word too long", func(r *Result) { r.Text, r.Words = long+"a", words(long+"a", 0, 10) }, false},
		{"negative start", func(r *Result) { r.Words[0].Start = -1 }, false},
		{"zero length", func(r *Result) { r.Words[0].End = 120 }, false},
		{"end after audio", func(r *Result) { r.Words[1].End = 2101 }, false},
		{"start moves back", func(r *Result) { r.Words[1].Start = 100 }, false},
		{"end moves back", func(r *Result) { r.Words[0].End, r.Words[1].End = 1800, 1700 }, false},
		{"confidence above one", func(r *Result) { r.Words[0].Confidence = 1.01 }, false},
		{"negative confidence", func(r *Result) { r.Words[0].Confidence = -0.01 }, false},
		{"speaker", func(r *Result) { r.Words[0].Speaker = &struct{}{} }, false},
		{"zero duration", func(r *Result) { r.Text, r.Words, r.AudioDurationMS = "", []Word{}, 0 }, false},
		{"duration above limit", func(r *Result) { r.AudioDurationMS = MaxAudioMS + 1 }, false},
		{"zero chunks", func(r *Result) { r.Chunks = 0 }, false},
		{"negative seam fallbacks", func(r *Result) { r.SeamFallbacks = -1 }, false},
		{"too many words", func(r *Result) {
			r.Words = make([]Word, MaxWords+1)
			for i := range r.Words {
				r.Words[i] = Word{Text: "a", Start: 0, End: 1}
			}
			r.Text = strings.TrimSpace(strings.Repeat("a ", MaxWords+1))
		}, false},
		{"text too long", func(r *Result) {
			r.Words = make([]Word, MaxTextChars/MaxWordChars+1)
			for i := range r.Words {
				r.Words[i] = Word{Text: long, Start: 0, End: 1}
			}
			r.Text = strings.Join(texts(r.Words), " ")
		}, false},
	} {
		r := valid()
		tc.change(r)
		if r.valid() != tc.ok {
			t.Errorf("%s: valid() = %v", tc.name, !tc.ok)
		}
	}
}

func TestDecodeResult(t *testing.T) {
	word := `{"text":"a","start":0,"end":1,"confidence":0.5}`
	result := func(words string) string {
		return `{"text":"a","words":[` + words + `],"audio_duration_ms":1,"chunks":1,"seam_fallbacks":0}`
	}
	for body, ok := range map[string]bool{
		result(word): true,
		result(`{"text":"a","start":0,"end":1,"confidence":1,"speaker":null,"channel":null}`): true,
		result(`{"text":"a","start":0,"end":1,"confidence":0.5,"speaker":{}}`):                false,
		result(`{"text":"a","start":0,"end":1,"confidence":0.5,"channel":1}`):                 false,
		result(`{"text":"a","start":0,"end":1,"confidence":null}`):                            false,
		result(`{"text":"a","start":0,"end":1}`):                                              false,
		result(`{"TEXT":"a","start":0,"end":1,"confidence":0.5}`):                             false,
		result(`{"text":"a","start":1.0,"end":2,"confidence":0.5}`):                           false,
		result(word + `,` + word):                                                             false,
		`{"text":"a","words":[` + word + `],"audio_duration_ms":1,"chunks":1}`:                false,
		`{"text":null,"words":[],"audio_duration_ms":1,"chunks":1,"seam_fallbacks":0}`:        false,
		`{"Text":"","words":[],"audio_duration_ms":1,"chunks":1,"seam_fallbacks":0}`:          false,
		`{"text":"","words":null,"audio_duration_ms":1,"chunks":1,"seam_fallbacks":0}`:        false,
		`null`: false,
		`[]`:   false,
	} {
		var r Result
		if err := json.Unmarshal([]byte(body), &r); (err == nil) != ok {
			t.Errorf("%s: %v", body, err)
		}
	}
	out, _ := json.Marshal(Word{Text: "a", Start: 1, End: 2, Confidence: 0.25})
	if string(out) != `{"text":"a","start":1,"end":2,"confidence":0.25,"speaker":null,"channel":null}` {
		t.Errorf("marshal: %s", out)
	}
}

// A body of many tiny invalid words must not allocate a word for each of them.
func TestDecodeResultBoundsWords(t *testing.T) {
	body := []byte(`{"words":[` + strings.Repeat(`{},`, 16<<20/3) + `{}]}`)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var r Result
	err := json.Unmarshal(body, &r)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; err == nil || allocated > 256<<20 {
		t.Fatalf("%v after %d MiB", err, allocated>>20)
	}
}

func TestGroup(t *testing.T) {
	text := func(groups [][]Word) []string {
		var out []string
		for _, g := range groups {
			out = append(out, strings.Join(texts(g), " "))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		words []Word
		chars int
		want  []string
	}{
		{"empty", nil, 80, nil},
		{"fits", words("one", 0, 100, "two", 200, 300), 7, []string{"one two"}},
		{"chars", words("one", 0, 100, "two", 200, 300), 6, []string{"one", "two"}},
		{"characters not bytes", words("éé", 0, 100, "éé", 200, 300), 5, []string{"éé éé"}},
		{"long word alone", words("a", 0, 100, "toolong", 200, 300, "b", 400, 500), 3, []string{"a", "toolong", "b"}},
		{"span of 6 s", words("a", 0, 100, "b", 1500, 6000), 80, []string{"a b"}},
		{"span above 6 s", words("a", 0, 100, "b", 1500, 6001), 80, []string{"a", "b"}},
		{"gap of 1.5 s", words("a", 0, 100, "b", 1600, 1700), 80, []string{"a b"}},
		{"gap above 1.5 s", words("a", 0, 100, "b", 1601, 1700), 80, []string{"a", "b"}},
	} {
		if got := text(Group(tc.words, tc.chars)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
}

func TestCaptions(t *testing.T) {
	w := words("Hello", 0, 400, "a<b", 500, 1000, "&", 3000, 3500, "c>", 3_723_004, 3_723_500)
	srt := "1\n00:00:00,000 --> 00:00:01,000\nHello a&lt;b\n\n" +
		"2\n00:00:03,000 --> 00:00:03,500\n&amp;\n\n" +
		"3\n01:02:03,004 --> 01:02:03,500\nc&gt;\n"
	vtt := "WEBVTT\n\n1\n00:00:00.000 --> 00:00:01.000\nHello a&lt;b\n\n" +
		"2\n00:00:03.000 --> 00:00:03.500\n&amp;\n\n" +
		"3\n01:02:03.004 --> 01:02:03.500\nc&gt;\n"
	if got := Captions(w, false, 80); got != srt {
		t.Errorf("srt:\n%s", got)
	}
	if got := Captions(w, true, 80); got != vtt {
		t.Errorf("vtt:\n%s", got)
	}
	if got := Captions(nil, false, 80) + "|" + Captions(nil, true, 80); got != "|WEBVTT\n" {
		t.Errorf("empty: %q", got)
	}
}

func TestVerbose(t *testing.T) {
	r := &Result{Text: "Hello world. Later", Words: words("Hello", 120, 610, "world.", 900, 1700, "Later", 3300, 3500), AudioDurationMS: 3600, Chunks: 1}
	for _, tc := range []struct {
		words, segments bool
		want            string
	}{
		{false, true, `{"task":"transcribe","duration":3.6,"text":"Hello world. Later","segments":[` +
			`{"id":0,"start":0.12,"end":1.7,"text":"Hello world."},{"id":1,"start":3.3,"end":3.5,"text":"Later"}]}`},
		{true, false, `{"task":"transcribe","duration":3.6,"text":"Hello world. Later","words":[` +
			`{"word":"Hello","start":0.12,"end":0.61},{"word":"world.","start":0.9,"end":1.7},{"word":"Later","start":3.3,"end":3.5}]}`},
	} {
		out, _ := json.Marshal(NewVerbose(r, tc.words, tc.segments))
		if string(out) != tc.want {
			t.Errorf("words=%v segments=%v:\n%s", tc.words, tc.segments, out)
		}
	}
	empty, _ := json.Marshal(NewVerbose(&Result{Words: []Word{}, AudioDurationMS: 1000}, true, true))
	if string(empty) != `{"task":"transcribe","duration":1,"text":"","words":[],"segments":[]}` {
		t.Errorf("empty: %s", empty)
	}
}
