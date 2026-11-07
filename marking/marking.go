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
	// MinWordConfidence drops words below this Tesseract confidence from
	// the final text (they still count, weakly, towards scoring). Default 30.
	MinWordConfidence float64
	// Rotations to try, in clockwise quarter turns. Default {0,1,2,3}.
	Rotations []int
	// Variants restricts which preprocess variants are OCR'd (by name:
	// "clean", "soft", "tophat", and the same with an "-inv" suffix when
	// the marking polarity was ambiguous). Empty means all.
	Variants []string
	// KeepImages stores the OCR input image in every Candidate.
	KeepImages bool
}

// DefaultOptions returns options tuned for component markings.
func DefaultOptions() Options {
	return Options{
		OCR:               ocr.DefaultConfig(),
		MinWordConfidence: 30,
		Rotations:         []int{0, 1, 2, 3},
	}
}

// Candidate is one OCR attempt.
type Candidate struct {
	Variant string
	// Rotation applied to the aligned crop before OCR, in degrees
	// clockwise (0, 90, 180, 270).
	Rotation int
	OCR      *ocr.Result
	Score    float64
	Image    *image.Gray // only with Options.KeepImages
}

// Result is the outcome of reading one photo.
type Result struct {
	// Text is the recognised marking, one line per marking line.
	Text  string
	Lines []string
	// Words that passed MinWordConfidence.
	Words []ocr.Word
	// Confidence is the mean confidence of Words, in [0,100].
	Confidence float64
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

func (o Options) withDefaults() Options {
	if o.MinWordConfidence == 0 {
		o.MinWordConfidence = 30
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

// Close releases the OCR engine if the Reader owns it.
func (r *Reader) Close() error {
	if r.owned && r.engine != nil {
		err := r.engine.Close()
		r.engine = nil
		return err
	}
	return nil
}

// Read preprocesses img and recognises the marking on it.
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
		if !r.wantVariant(v.Name) {
			continue
		}
		for _, q := range r.opt.Rotations {
			im := preprocess.RotateGray(v.Image, q)
			res, err := r.engine.Recognize(im)
			if err != nil {
				return nil, fmt.Errorf("marking: OCR of %s@%d failed: %w", v.Name, q*90, err)
			}
			c := Candidate{Variant: v.Name, Rotation: ((q % 4) + 4) % 4 * 90, OCR: res, Score: score(res)}
			if r.opt.KeepImages {
				c.Image = im
			}
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("marking: no variants to OCR")
	}
	sortCandidates(cands)
	best := cands[0]

	out := &Result{Best: best, Candidates: cands, Preprocess: pre}
	filtered := &ocr.Result{}
	var sum float64
	for _, w := range best.OCR.Words {
		cc := w.CharConfidence()
		if cc < r.opt.MinWordConfidence || alnumCount(w.Text) == 0 {
			continue
		}
		filtered.Words = append(filtered.Words, w)
		sum += cc
	}
	out.Words = filtered.Words
	out.Lines = filtered.Lines()
	out.Text = strings.Join(out.Lines, "\n")
	if len(filtered.Words) > 0 {
		out.Confidence = sum / float64(len(filtered.Words))
	}
	return out, nil
}

func (r *Reader) wantVariant(name string) bool {
	if len(r.opt.Variants) == 0 {
		return true
	}
	for _, v := range r.opt.Variants {
		if v == name {
			return true
		}
	}
	return false
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

func sortCandidates(c []Candidate) {
	sort.SliceStable(c, func(i, j int) bool { return c[i].Score > c[j].Score })
}
