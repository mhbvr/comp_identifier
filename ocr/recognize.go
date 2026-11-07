package ocr

import (
	"image"
	"math"
	"strings"

	ort "github.com/yalue/onnxruntime_go"
)

// The recogniser reads line images scaled to 48 px height and at least
// 320 px width (shorter lines are padded).
const (
	recH    = 48
	recMinW = 320
)

var (
	recMean = [3]float32{0.5, 0.5, 0.5}
	recStd  = [3]float32{0.5, 0.5, 0.5}
)

// recognize reads one text line image and returns its text and mean
// character confidence.
func (e *Engine) recognize(line *image.RGBA) (string, float64, error) {
	w, h := line.Rect.Dx(), line.Rect.Dy()
	if w == 0 || h == 0 {
		return "", 0, nil
	}
	ratio := float64(w) / float64(h)
	tw := int(recH * math.Max(float64(recMinW)/recH, ratio))
	rw := min(tw, int(math.Ceil(recH*ratio)))
	in, err := ort.NewTensor(ort.NewShape(1, 3, recH, int64(tw)), tensorCHW(resize(line, rw, recH), tw, recMean, recStd))
	if err != nil {
		return "", 0, err
	}
	defer in.Destroy()
	outs := []ort.Value{nil}
	if err := e.rec.Run([]ort.Value{in}, outs); err != nil {
		return "", 0, err
	}
	defer outs[0].Destroy()
	t := outs[0].(*ort.Tensor[float32])
	shape := t.GetShape() // 1 x steps x classes
	text, conf := ctcDecode(t.GetData(), int(shape[1]), int(shape[2]), e.dict)
	return text, conf, nil
}

// ctcDecode is greedy CTC decoding: take the most likely class at every
// step, merge repeats, drop blanks (class 0). The confidence is the mean
// probability of the kept characters.
func ctcDecode(probs []float32, steps, classes int, dict []string) (string, float64) {
	var b strings.Builder
	var sum float64
	n, prev := 0, -1
	for s := 0; s < steps; s++ {
		row := probs[s*classes : (s+1)*classes]
		best := 0
		for c := range row {
			if row[c] > row[best] {
				best = c
			}
		}
		if best != 0 && best != prev && best < len(dict) {
			b.WriteString(dict[best])
			sum += float64(row[best])
			n++
		}
		prev = best
	}
	if n == 0 {
		return "", 0
	}
	return b.String(), sum / float64(n)
}
