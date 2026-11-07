package ocr

import (
	"math"

	"image"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/mhbvr/comp_identifier/preprocess"
)

// Detector settings, as in PaddleOCR / RapidOCR.
const (
	detMinSide    = 736 // the image is scaled up until its short side is at least this
	detThreshold  = 0.3 // pixel text probability threshold
	detBoxScore   = 0.5 // minimum mean probability inside a box
	detUnclip     = 1.6 // how far a box is expanded beyond the text core
	detMinBoxSide = 3
)

var (
	detMean = [3]float32{0.485, 0.456, 0.406}
	detStd  = [3]float32{0.229, 0.224, 0.225}
)

// detect returns the text-line boxes in src, in src coordinates.
func (e *Engine) detect(src *image.RGBA) ([]preprocess.RotatedRect, error) {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	ratio := 1.0
	if s := min(w, h); s < detMinSide {
		ratio = float64(detMinSide) / float64(s)
	}
	// The network needs sides that are multiples of 32.
	rw := max(32, int(math.Round(float64(int(float64(w)*ratio))/32))*32)
	rh := max(32, int(math.Round(float64(int(float64(h)*ratio))/32))*32)
	in, err := ort.NewTensor(ort.NewShape(1, 3, int64(rh), int64(rw)), tensorCHW(resize(src, rw, rh), rw, detMean, detStd))
	if err != nil {
		return nil, err
	}
	defer in.Destroy()
	outs := []ort.Value{nil}
	if err := e.det.Run([]ort.Value{in}, outs); err != nil {
		return nil, err
	}
	defer outs[0].Destroy()
	prob := outs[0].(*ort.Tensor[float32]).GetData() // 1 x 1 x rh x rw

	boxes := boxesFromProbMap(prob, rw, rh)
	// Back to source coordinates.
	sx, sy := float64(w)/float64(rw), float64(h)/float64(rh)
	var out []preprocess.RotatedRect
	for _, b := range boxes {
		var pts []preprocess.Point
		for _, c := range b.Corners() {
			pts = append(pts, preprocess.Point{
				X: math.Min(math.Max(c.X*sx, 0), float64(w)),
				Y: math.Min(math.Max(c.Y*sy, 0), float64(h)),
			})
		}
		r := horizontal(preprocess.MinAreaRect(pts))
		if r.W > detMinBoxSide && r.H > detMinBoxSide {
			out = append(out, r)
		}
	}
	return out, nil
}

// boxesFromProbMap turns the detector's probability map into boxes (the DB
// post-processing): threshold, a 2x2 dilation, connected regions, their
// minimum-area rectangles, a score filter, and an expansion ("unclip") that
// recovers the full text extent from the shrunken text core the network
// marks.
func boxesFromProbMap(prob []float32, w, h int) []preprocess.RotatedRect {
	mask := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Dilation with a 2x2 kernel: a pixel is set if it or its left,
			// upper or upper-left neighbour is above the threshold.
			on := func(x, y int) bool { return x >= 0 && y >= 0 && prob[y*w+x] > detThreshold }
			mask[y*w+x] = on(x, y) || on(x-1, y) || on(x, y-1) || on(x-1, y-1)
		}
	}
	var boxes []preprocess.RotatedRect
	for _, pts := range regions(mask, w, h) {
		r := preprocess.MinAreaRect(pts)
		if min(r.W, r.H) < detMinBoxSide || meanInside(prob, w, h, r) < detBoxScore {
			continue
		}
		// Unclip: grow every side by area * ratio / perimeter.
		d := r.W * r.H * detUnclip / (2 * (r.W + r.H))
		r.W += 2 * d
		r.H += 2 * d
		if min(r.W, r.H) < detMinBoxSide+2 {
			continue
		}
		boxes = append(boxes, r)
	}
	return boxes
}

// regions returns the pixel coordinates of each 8-connected region of mask.
func regions(mask []bool, w, h int) [][]preprocess.Point {
	seen := make([]bool, len(mask))
	var out [][]preprocess.Point
	var stack []int
	for start, on := range mask {
		if !on || seen[start] {
			continue
		}
		var pts []preprocess.Point
		seen[start] = true
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := i%w, i/w
			pts = append(pts, preprocess.Point{X: float64(x), Y: float64(y)})
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					if j := ny*w + nx; mask[j] && !seen[j] {
						seen[j] = true
						stack = append(stack, j)
					}
				}
			}
		}
		out = append(out, pts)
	}
	return out
}

// meanInside is the mean of prob over the pixels inside r.
func meanInside(prob []float32, w, h int, r preprocess.RotatedRect) float64 {
	ux, uy := math.Cos(r.Angle), math.Sin(r.Angle)
	reach := (r.W + r.H) / 2
	var sum float64
	n := 0
	for y := max(0, int(r.Center.Y-reach)); y <= min(h-1, int(r.Center.Y+reach)); y++ {
		for x := max(0, int(r.Center.X-reach)); x <= min(w-1, int(r.Center.X+reach)); x++ {
			dx, dy := float64(x)-r.Center.X, float64(y)-r.Center.Y
			if math.Abs(dx*ux+dy*uy) <= r.W/2+0.5 && math.Abs(-dx*uy+dy*ux) <= r.H/2+0.5 {
				sum += float64(prob[y*w+x])
				n++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// horizontal returns r described with its W side the more horizontal one
// (angle within ±45°), so that a crop along W reads left to right.
func horizontal(r preprocess.RotatedRect) preprocess.RotatedRect {
	for r.Angle > math.Pi/4 {
		r.Angle -= math.Pi / 2
		r.W, r.H = r.H, r.W
	}
	for r.Angle <= -math.Pi/4 {
		r.Angle += math.Pi / 2
		r.W, r.H = r.H, r.W
	}
	return r
}
