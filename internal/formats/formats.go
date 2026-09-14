package formats

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Caption grouping limits.
const (
	maxSegmentMS = 6000
	maxGapMS     = 1500
)

// Group splits words into captions. A caption grows while its words joined by spaces stay within
// maxChars characters, it spans at most 6 s and no pause exceeds 1.5 s. A longer single word
// still forms its own caption.
func Group(words []Word, maxChars int) [][]Word {
	var groups [][]Word
	first, chars := 0, 0
	for i, w := range words {
		n := utf8.RuneCountInString(w.Text)
		if i > first && (chars+1+n > maxChars || w.End-words[first].Start > maxSegmentMS || w.Start-words[i-1].End > maxGapMS) {
			groups = append(groups, words[first:i])
			first, chars = i, 0
		}
		if i > first {
			chars++
		}
		chars += n
	}
	if first < len(words) {
		groups = append(groups, words[first:])
	}
	return groups
}

// Timestamp formats milliseconds as HH:MM:SS,mmm (SRT) or HH:MM:SS.mmm (VTT).
func Timestamp(ms int64, vtt bool) string {
	separator := ","
	if vtt {
		separator = "."
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, separator, ms%1000)
}

var cueEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Captions renders SRT or WebVTT captions of at most maxChars characters each.
func Captions(words []Word, vtt bool, maxChars int) string {
	var b strings.Builder
	if vtt {
		b.WriteString("WEBVTT\n")
	}
	for i, group := range Group(words, maxChars) {
		if i > 0 || vtt {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n", i+1, Timestamp(group[0].Start, vtt),
			Timestamp(group[len(group)-1].End, vtt), cueEscaper.Replace(strings.Join(texts(group), " ")))
	}
	return b.String()
}

// Segment is a caption-sized group of words in the OpenAI verbose_json response, in seconds.
type Segment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// VerboseWord is a word in the OpenAI verbose_json response, in seconds.
type VerboseWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Verbose is the OpenAI verbose_json response.
type Verbose struct {
	Task     string        `json:"task"`
	Duration float64       `json:"duration"`
	Text     string        `json:"text"`
	Words    []VerboseWord `json:"words,omitzero"`
	Segments []Segment     `json:"segments,omitzero"`
}

// NewVerbose builds the verbose_json response with words and segments as requested.
func NewVerbose(r *Result, words, segments bool) Verbose {
	v := Verbose{Task: "transcribe", Duration: seconds(r.AudioDurationMS), Text: r.Text}
	if words {
		v.Words = make([]VerboseWord, len(r.Words))
		for i, w := range r.Words {
			v.Words[i] = VerboseWord{Word: w.Text, Start: seconds(w.Start), End: seconds(w.End)}
		}
	}
	if segments {
		v.Segments = []Segment{}
		for i, group := range Group(r.Words, 80) {
			v.Segments = append(v.Segments, Segment{ID: i, Start: seconds(group[0].Start),
				End: seconds(group[len(group)-1].End), Text: strings.Join(texts(group), " ")})
		}
	}
	return v
}

func seconds(ms int64) float64 { return float64(ms) / 1000 }
