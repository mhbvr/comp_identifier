// Package marking reads the text printed on an electronic component from a
// photo. It glues together the preprocess package (find, align, crop and
// enhance the component) and an OCR engine, and resolves the remaining
// orientation ambiguity by running OCR on each 90° rotation of each
// enhanced variant and keeping the most confident reading.
package marking

import (
	"fmt"
	"image"
	"sort"
	"strings"
	"unicode"

	"github.com/mhbvr/comp_identifier/ocr"
	"github.com/mhbvr/comp_identifier/preprocess"
)

// Options configures a Reader.
type Options struct {
	Preprocess preprocess.Options
	OCR        ocr.Config
	// MinWordConfidence drops words below this mean character confidence
	// from the final text (they still count, weakly, towards scoring).
	// Default 30.
	MinWordConfidence float64
	// ReliableConfidence is the mean confidence from which a reading is
	// considered reliable on its own (see Result.Reliable). Default 97.
	ReliableConfidence float64
	// Rotations to try, in clockwise quarter turns. Default {0,1,2,3}.
	Rotations []int
}

// DefaultOptions returns options tuned for component markings.
func DefaultOptions() Options { return Options{}.withDefaults() }

func (o Options) withDefaults() Options {
	if o.MinWordConfidence == 0 {
		o.MinWordConfidence = 30
	}
	if o.ReliableConfidence == 0 {
		o.ReliableConfidence = 97
	}
	if len(o.Rotations) == 0 {
		o.Rotations = []int{0, 1, 2, 3}
	}
	// A zero OCR config means "use the defaults tuned for markings".
	if len(o.OCR.Languages) == 0 && o.OCR.Whitelist == "" && o.OCR.PageSegMode == 0 && !o.OCR.DisableDictionary {
		o.OCR = ocr.DefaultConfig()
	}
	return o
}

// Candidate is one OCR attempt: one variant at one rotation.
type Candidate struct {
	Variant string
	// Rotation applied to the aligned crop before OCR, in degrees
	// clockwise (0, 90, 180, 270).
	Rotation int
	OCR      *ocr.Result
	Score    float64
	// Image is the OCR input (the variant after rotation).
	Image *image.Gray
}

// Result is the outcome of reading one photo.
type Result struct {
	// Text is the recognised marking, one line per marking line.
	Text  string
	Lines []string
	// Words that passed MinWordConfidence.
	Words []ocr.Word
	// Confidence is the mean character confidence of Words, in [0,100].
	Confidence float64
	// Key is the marking normalised for comparing readings: upper case,
	// no spaces, only words of two or more characters.
	Key string
	// Agreement counts the other variants that, at the winning rotation,
	// read exactly the same Key. Independent variants agreeing is a strong
	// sign the reading is right.
	Agreement int
	// Reliable is true when the reading is likely correct: Confidence is
	// at least Options.ReliableConfidence, or another variant agrees.
	// Measured on the sample photos, both rules are right about 9 times
	// out of 10; most correct readings are not marked reliable, though.
	Reliable bool
	// Best is the winning candidate; Candidates holds all of them, best
	// first.
	Best       Candidate
	Candidates []Candidate
	// Preprocess holds the intermediate images (crop, variants, masks).
	Preprocess *preprocess.Result
}

// Reader reads component markings. It is not safe for concurrent use.
type Reader struct {
	engine ocr.Engine
	owned  bool
	opt    Options
}

// NewReader creates a Reader with the default OCR engine for this build.
func NewReader(opt Options) (*Reader, error) {
	opt = opt.withDefaults()
	e, err := ocr.NewEngine(opt.OCR)
	if err != nil {
		return nil, err
	}
	return &Reader{engine: e, owned: true, opt: opt}, nil
}

// NewReaderWithEngine creates a Reader using the given engine. The caller
// keeps ownership of the engine.
func NewReaderWithEngine(e ocr.Engine, opt Options) *Reader {
	return &Reader{engine: e, opt: opt.withDefaults()}
}

// Close releases the OCR engine if the Reader owns it.
func (r *Reader) Close() error {
	if r.owned && r.engine != nil {
		err := r.engine.Close()
		r.engine = nil
		return err
	}
	return nil
}

// Read preprocesses img and recognises the marking on it. It returns
// preprocess.ErrNoComponent if there is no component in the image.
func (r *Reader) Read(img image.Image) (*Result, error) {
	pre, err := preprocess.Process(img, r.opt.Preprocess)
	if err != nil {
		return nil, err
	}
	return r.ReadPreprocessed(pre)
}

// ReadPreprocessed runs the OCR stage on an already preprocessed image.
func (r *Reader) ReadPreprocessed(pre *preprocess.Result) (*Result, error) {
	var cands []Candidate
	for _, v := range pre.Variants {
		for _, q := range r.opt.Rotations {
			im := preprocess.RotateGray(v.Image, q)
			res, err := r.engine.Recognize(im)
			if err != nil {
				return nil, fmt.Errorf("marking: OCR of %s@%d failed: %w", v.Name, q*90, err)
			}
			cands = append(cands, Candidate{
				Variant: v.Name, Rotation: (q%4 + 4) % 4 * 90,
				OCR: res, Score: score(res), Image: im,
			})
		}
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("marking: no variants to OCR")
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })

	best := cands[0]
	out := &Result{Best: best, Candidates: cands, Preprocess: pre}
	var sum float64
	kept := &ocr.Result{}
	for _, w := range best.OCR.Words {
		cc := w.CharConfidence()
		if cc < r.opt.MinWordConfidence || alnumCount(w.Text) == 0 {
			continue
		}
		kept.Words = append(kept.Words, w)
		sum += cc
	}
	out.Words = kept.Words
	out.Lines = kept.Lines()
	out.Text = strings.Join(out.Lines, "\n")
	if len(kept.Words) > 0 {
		out.Confidence = sum / float64(len(kept.Words))
	}

	out.Key = r.key(best.OCR)
	for _, c := range cands[1:] {
		if out.Key != "" && c.Rotation == best.Rotation && r.key(c.OCR) == out.Key {
			out.Agreement++
		}
	}
	out.Reliable = out.Key != "" && (out.Confidence >= r.opt.ReliableConfidence || out.Agreement > 0)
	return out, nil
}

// key normalises a reading for comparison (see Result.Key).
func (r *Reader) key(res *ocr.Result) string {
	var b strings.Builder
	for _, w := range res.Words {
		if alnumCount(w.Text) >= 2 && w.CharConfidence() >= r.opt.MinWordConfidence {
			b.WriteString(w.Text)
		}
	}
	return Normalize(b.String())
}

// score rates an OCR result: it rewards many confidently recognised
// characters. Confidence is cubed so that a few certain characters beat
// many doubtful ones (Tesseract happily reads noise as low-confidence
// text), and very short words, which are usually logos, specks or
// fragments of misread text, are discounted.
func score(res *ocr.Result) float64 {
	var s float64
	for _, w := range res.Words {
		n := alnumCount(w.Text)
		if n == 0 {
			continue
		}
		c := w.CharConfidence() / 100
		wt := float64(n)
		switch n {
		case 1:
			wt = 0.2
		case 2:
			wt = 1
		}
		s += c * c * c * wt
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
