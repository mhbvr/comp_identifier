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

func TestScorePrefersConfidentLongWords(t *testing.T) {
	good := &ocr.Result{Words: []ocr.Word{{Text: "LM35", Confidence: 90}, {Text: "DZ", Confidence: 85}}}
	noise := &ocr.Result{Words: []ocr.Word{
		{Text: "S", Confidence: 60}, {Text: "J", Confidence: 55}, {Text: "4A", Confidence: 40},
		{Text: "Z", Confidence: 70}, {Text: "WE", Confidence: 45},
	}}
	if score(good) <= score(noise) {
		t.Errorf("score(good)=%.2f <= score(noise)=%.2f", score(good), score(noise))
	}
	if score(&ocr.Result{}) != 0 {
		t.Error("empty result should score 0")
	}
}
