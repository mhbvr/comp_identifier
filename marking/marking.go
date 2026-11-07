// Package marking reads the text printed on an electronic component from a
// photo. It finds and aligns the component (package preprocess), reads the
// aligned crop with the local OCR engine (package ocr) at each 90°
// rotation, and keeps the most confident reading: after alignment the text
// can still be upside down or run along the short side.
package marking

import (
	"image"
	"image/color"
	"image/draw"
	"strings"
	"unicode"

	"github.com/mhbvr/comp_identifier/ocr"
	"github.com/mhbvr/comp_identifier/preprocess"
)

// Options configures a Reader.
type Options struct {
	Preprocess preprocess.Options
	OCR        ocr.Config
	// ReliableConfidence is the mean character confidence, in [0,1], from
	// which a reading counts as reliable (Result.Reliable). Default 0.93:
	// on the sample photos about 95% of the readings above it are exact.
	ReliableConfidence float64
}

func (o Options) withDefaults() Options {
	if o.ReliableConfidence == 0 {
		o.ReliableConfidence = 0.93
	}
	return o
}

// Candidate is the reading of the crop at one rotation.
type Candidate struct {
	// Rotation applied to the aligned crop, in degrees clockwise.
	Rotation int
	Lines    []ocr.Line
	Score    float64
}

// Result is the outcome of reading one photo.
type Result struct {
	// Text is the recognised marking, one line per marking line.
	Text  string
	Lines []string
	// Confidence is the mean character confidence of the text, in [0,1].
	Confidence float64
	// Key is the marking normalised for comparing readings: upper case,
	// no spaces, only lines of two or more characters.
	Key string
	// Reliable is true when Confidence reaches Options.ReliableConfidence.
	Reliable bool
	// Rotation is the winning rotation, in degrees clockwise, and Upright
	// the crop turned by it, so that the text reads normally.
	Rotation int
	Upright  *image.RGBA
	// TextLines are the winning lines, with the images the recogniser read.
	TextLines []ocr.Line
	// Score of the winning rotation; Candidates holds all four.
	Score      float64
	Candidates []Candidate
	Preprocess *preprocess.Result
}

// Reader reads component markings. It is safe for concurrent use.
type Reader struct {
	engine *ocr.Engine
	owned  bool
	opt    Options
}

// NewReader creates a Reader with its own OCR engine.
func NewReader(opt Options) (*Reader, error) {
	e, err := ocr.NewEngine(opt.OCR)
	if err != nil {
		return nil, err
	}
	return &Reader{engine: e, owned: true, opt: opt.withDefaults()}, nil
}

// NewReaderWithEngine creates a Reader that uses (and does not close) e.
func NewReaderWithEngine(e *ocr.Engine, opt Options) *Reader {
	return &Reader{engine: e, opt: opt.withDefaults()}
}

// Close releases the OCR engine if the Reader owns it.
func (r *Reader) Close() error {
	if r.owned {
		return r.engine.Close()
	}
	return nil
}

// Read finds the component in img and reads its marking. It returns
// preprocess.ErrNoComponent if there is no component in the image.
func (r *Reader) Read(img image.Image) (*Result, error) {
	pre, err := preprocess.Process(img, r.opt.Preprocess)
	if err != nil {
		return nil, err
	}
	return r.ReadCrop(pre)
}

// ReadCrop reads an already preprocessed component.
func (r *Reader) ReadCrop(pre *preprocess.Result) (*Result, error) {
	out := &Result{Preprocess: pre}
	var best *image.RGBA
	for q := 0; q < 4; q++ {
		img := preprocess.RotateRGBA(pre.Crop, q)
		lines, err := r.engine.Recognize(img)
		if err != nil {
			return nil, err
		}
		c := Candidate{Rotation: q * 90, Lines: lines, Score: score(lines)}
		out.Candidates = append(out.Candidates, c)
		if best == nil || c.Score > out.Score {
			best, out.Score, out.Rotation, out.TextLines = img, c.Score, c.Rotation, lines
		}
	}
	out.Upright = best

	var sum, n float64
	var key strings.Builder
	for _, l := range out.TextLines {
		out.Lines = append(out.Lines, l.Text)
		k := float64(alnumCount(l.Text))
		sum += l.Confidence * k
		n += k
		if k >= 2 {
			key.WriteString(l.Text)
		}
	}
	out.Text = strings.Join(out.Lines, "\n")
	if n > 0 {
		out.Confidence = sum / n
	}
	out.Key = Normalize(key.String())
	out.Reliable = out.Key != "" && out.Confidence >= r.opt.ReliableConfidence
	return out, nil
}

// score rates a reading: the sum over lines of confidence times the number
// of letters and digits. Reading the crop at a wrong rotation gives fewer,
// less confident characters.
func score(lines []ocr.Line) float64 {
	var s float64
	for _, l := range lines {
		s += l.Confidence * float64(alnumCount(l.Text))
	}
	return s
}

func alnumCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

// LinesImage stacks the images of the recognised lines (what the
// recogniser actually read) on a white background, one under another. It
// returns nil when no text was found.
func (res *Result) LinesImage() *image.RGBA {
	const gap = 6
	w, h := 0, gap
	for _, l := range res.TextLines {
		w = max(w, l.Image.Rect.Dx())
		h += l.Image.Rect.Dy() + gap
	}
	if w == 0 {
		return nil
	}
	out := image.NewRGBA(image.Rect(0, 0, w+2*gap, h))
	draw.Draw(out, out.Rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	y := gap
	for _, l := range res.TextLines {
		r := l.Image.Rect
		draw.Draw(out, image.Rect(gap, y, gap+r.Dx(), y+r.Dy()), l.Image, r.Min, draw.Src)
		y += r.Dy() + gap
	}
	return out
}
