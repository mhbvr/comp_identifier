package ocr

import (
	"image"
	"testing"
)

const sampleTSV = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
	"1\t1\t0\t0\t0\t0\t0\t0\t300\t200\t-1\t\n" +
	"4\t1\t1\t1\t1\t0\t10\t10\t200\t40\t-1\t\n" +
	"5\t1\t1\t1\t1\t1\t10\t10\t90\t40\t96.5\tCD4026BE\n" +
	"5\t1\t1\t1\t2\t1\t120\t60\t50\t40\t88\tE4\n" +
	"5\t1\t1\t1\t2\t2\t10\t60\t100\t40\t91\t18ADETM\n"

func TestParseTSV(t *testing.T) {
	res, err := parseTSV([]byte(sampleTSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Words) != 3 {
		t.Fatalf("got %d words, want 3", len(res.Words))
	}
	w := res.Words[0]
	if w.Text != "CD4026BE" || w.Confidence != 96.5 || w.Box != image.Rect(10, 10, 100, 50) {
		t.Errorf("first word = %+v", w)
	}
	// Words on line 2 are re-ordered left to right.
	if got := res.Text(); got != "CD4026BE\n18ADETM E4" {
		t.Errorf("Text() = %q", got)
	}
}

func TestCharConfidence(t *testing.T) {
	w := Word{Confidence: 0, Chars: []Char{{"A", 90}, {"B", 70}}}
	if c := w.CharConfidence(); c != 80 {
		t.Errorf("CharConfidence = %v, want 80", c)
	}
	if c := (Word{Confidence: 55}).CharConfidence(); c != 55 {
		t.Errorf("fallback CharConfidence = %v, want 55", c)
	}
}
