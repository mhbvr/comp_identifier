package marking

import (
	"testing"

	"github.com/mhbvr/comp_identifier/ocr"
)

func TestMatchToken(t *testing.T) {
	cases := []struct {
		text, tok string
		want      int
	}{
		{"18ADETM E4\nCD4026BE", "CD4026BE", 0},
		{"cd 4026 be", "CD4026BE", 0},
		{"1BADETME4 / CD4O26BE", "CD4026BE", 1},
		{"TL74CP", "TL714CP", 1},
		{"", "LM35", 4},
		{"anything", "", 0},
	}
	for _, c := range cases {
		if got := MatchToken(c.text, c.tok); got != c.want {
			t.Errorf("MatchToken(%q, %q) = %d, want %d", c.text, c.tok, got, c.want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"31AE9XM\nTL072IP", "31ae9xm tl072ip", 0},
		{"TL072IP", "TL074IP", 1},
		{"TL072IP", "TL072", 2},
		{"", "LM35", 4},
		{"LM35", "", 4},
	}
	for _, c := range cases {
		if got := EditDistance(c.a, c.b); got != c.want {
			t.Errorf("EditDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestScorePrefersConfidentLongLines(t *testing.T) {
	good := []ocr.Line{{Text: "LM35", Confidence: 0.98}, {Text: "DZ", Confidence: 0.95}}
	noise := []ocr.Line{{Text: "S", Confidence: 0.6}, {Text: "J.", Confidence: 0.55}, {Text: "4A", Confidence: 0.5}}
	if score(good) <= score(noise) {
		t.Errorf("score(good)=%.2f <= score(noise)=%.2f", score(good), score(noise))
	}
	if score(nil) != 0 {
		t.Error("no lines should score 0")
	}
}
