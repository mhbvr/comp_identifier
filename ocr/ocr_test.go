package ocr

import (
	"image"
	_ "image/jpeg"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/mhbvr/comp_identifier/preprocess"
)

func TestLoadDict(t *testing.T) {
	d := loadDict(dictText)
	// blank + 436 characters + space = the recogniser's 438 classes.
	if len(d) != 438 || d[0] != "" || d[1] != "0" || d[len(d)-1] != " " {
		t.Errorf("dict: %d entries, first %q %q, last %q", len(d), d[0], d[1], d[len(d)-1])
	}
}

func TestCTCDecode(t *testing.T) {
	dict := []string{"", "A", "B"}
	// Steps: A A blank A B B -> "AAB" (repeats merge, a blank separates).
	steps := [][]float32{
		{0.1, 0.8, 0.1}, {0.2, 0.7, 0.1}, {0.9, 0.05, 0.05},
		{0.1, 0.6, 0.3}, {0.1, 0.1, 0.8}, {0.0, 0.0, 1.0},
	}
	var probs []float32
	for _, s := range steps {
		probs = append(probs, s...)
	}
	text, conf := ctcDecode(probs, len(steps), 3, dict)
	if text != "AAB" || math.Abs(conf-(0.8+0.6+0.8)/3) > 1e-6 {
		t.Errorf("got %q %.3f", text, conf)
	}
	if text, conf := ctcDecode([]float32{1, 0, 0}, 1, 3, dict); text != "" || conf != 0 {
		t.Errorf("all-blank: %q %v", text, conf)
	}
}

func TestBoxesFromProbMap(t *testing.T) {
	// Two text cores: 60x10 and 30x8 pixels of high probability.
	const w, h = 120, 60
	prob := make([]float32, w*h)
	fill := func(x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				prob[y*w+x] = 0.9
			}
		}
	}
	fill(10, 10, 70, 20)
	fill(20, 40, 50, 48)
	prob[5*w+100] = 0.9 // a single speck: too small
	boxes := boxesFromProbMap(prob, w, h)
	if len(boxes) != 2 {
		t.Fatalf("got %d boxes, want 2: %+v", len(boxes), boxes)
	}
	b := horizontal(boxes[0])
	// The core is unclipped by area*1.6/perimeter on every side.
	core := preprocess.MinAreaRect([]preprocess.Point{{X: 10, Y: 10}, {X: 70, Y: 21}})
	if b.W <= core.W || b.H <= core.H || math.Abs(b.Center.Y-15) > 1.5 {
		t.Errorf("box %+v not an expansion of the first core", b)
	}
}

func TestHorizontal(t *testing.T) {
	r := horizontal(preprocess.RotatedRect{W: 10, H: 50, Angle: math.Pi / 2})
	if r.W != 50 || r.H != 10 || math.Abs(r.Angle) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestSortReadingOrder(t *testing.T) {
	line := func(text string, x, y float64) Line {
		return Line{Text: text, Box: preprocess.RotatedRect{Center: preprocess.Point{X: x, Y: y}, W: 40, H: 20}}
	}
	lines := []Line{line("C", 50, 80), line("B", 120, 22), line("A", 50, 20)}
	sortReadingOrder(lines)
	if got := lines[0].Text + lines[1].Text + lines[2].Text; got != "ABC" {
		t.Errorf("order %s, want ABC", got)
	}
}

// TestRecognizeRealCrop reads a real chip photo. It needs the ONNX Runtime
// library and is skipped without it.
func TestRecognizeRealCrop(t *testing.T) {
	e, err := NewEngine(Config{})
	if err != nil {
		t.Skip("ONNX Runtime not available:", err)
	}
	defer e.Close()
	f, err := os.Open("../preprocess/testdata/shadow_tlc3702.jpg")
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	pre, err := preprocess.Process(img, preprocess.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := e.Recognize(pre.Crop)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.Text)
		if l.Image == nil || l.Confidence < 0.5 || l.Confidence > 1 {
			t.Errorf("line %+v", l)
		}
	}
	if got := strings.Join(texts, " / "); !strings.Contains(got, "TLC3702CP") {
		t.Errorf("read %q, want TLC3702CP", got)
	}
}
