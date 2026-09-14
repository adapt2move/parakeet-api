package api

import (
	"testing"
)

func TestParseOptions(t *testing.T) {
	type fields = map[string][]string
	for _, tc := range []struct {
		name    string
		fields  fields
		hasFile bool
		want    transcriptionOptions
		err     error
	}{
		{"defaults", fields{}, true, transcriptionOptions{format: "json", segments: true}, nil},
		{"all fields", fields{"model": {"whisper-1"}, "language": {"de"}, "temperature": {"0.0"}, "response_format": {"verbose_json"},
			"timestamp_granularities[]": {"word", "word"}}, true, transcriptionOptions{format: "verbose_json", words: true, language: "de"}, nil},
		{"both granularities", fields{"timestamp_granularities[]": {"segment", "word"}}, true,
			transcriptionOptions{format: "json", words: true, segments: true}, nil},
		{"empty language", fields{"language": {""}}, true, transcriptionOptions{format: "json", segments: true}, nil},
		{"unknown field", fields{"prompt": {"x"}, "model": {"a", "b"}}, true, transcriptionOptions{}, errUnsupported},
		{"duplicate", fields{"model": {"parakeet", "parakeet"}}, true, transcriptionOptions{}, errDuplicate},
		{"model", fields{"model": {"gpt-4o-transcribe"}}, true, transcriptionOptions{}, errModel},
		{"temperature", fields{"temperature": {"0.2"}}, true, transcriptionOptions{}, errTemperature},
		{"format", fields{"response_format": {"diarized_json"}}, true, transcriptionOptions{}, errFormat},
		{"granularity", fields{"timestamp_granularities[]": {"char"}}, true, transcriptionOptions{}, errFormat},
		{"language", fields{"language": {"EN"}}, true, transcriptionOptions{}, errInvalidOptions},
		{"no file", fields{}, false, transcriptionOptions{}, errNoFile},
		{"plain file field", fields{"file": {"audio"}}, true, transcriptionOptions{}, errNoFile},
	} {
		got, err := parseOptions(tc.fields, tc.hasFile)
		if err != tc.err || (err == nil && got != tc.want) {
			t.Errorf("%s: %+v %v", tc.name, got, err)
		}
	}
}
