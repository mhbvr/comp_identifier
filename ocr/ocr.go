// Package ocr reads text in an image with PaddleOCR models run by ONNX
// Runtime, all locally.
//
// Two neural networks are used, both embedded in the binary:
//
//   - a text detector (PP-OCRv3, DB): it outputs, for every pixel, the
//     probability that it belongs to text; connected regions of that map
//     become text-line boxes;
//   - a text recogniser (PP-OCRv5, English): it reads one text line image
//     and outputs, for each horizontal step, a probability for every
//     character of its dictionary, decoded with CTC.
//
// Both work on the colour image as it is: there is no binarisation step
// that could destroy faint markings. Why this replaced Tesseract is
// written up in doc/text-recognition.md.
//
// ONNX Runtime itself is a shared library loaded at run time (cgo); see
// Config.Library.
package ocr

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/mhbvr/comp_identifier/preprocess"
)

// Config configures an Engine.
type Config struct {
	// Library is the path of the ONNX Runtime shared library (version 1.31
	// or newer), e.g. /opt/onnxruntime/lib/libonnxruntime.so.1.31.0.
	// Empty: $ONNXRUNTIME_LIB, else "libonnxruntime.so" (.dylib on macOS),
	// found through the system's library search path.
	Library string
	// Threads is the number of threads ONNX Runtime may use per model run.
	// Zero lets ONNX Runtime decide (all cores). Use 1 when several engines
	// run in parallel.
	Threads int
	// MinConfidence drops text lines whose mean character confidence is
	// below it, in [0,1]. Default 0.5.
	MinConfidence float64
}

func (c Config) withDefaults() Config {
	if c.Library == "" {
		c.Library = os.Getenv("ONNXRUNTIME_LIB")
	}
	if c.Library == "" {
		c.Library = "libonnxruntime.so"
		if runtime.GOOS == "darwin" {
			c.Library = "libonnxruntime.dylib"
		}
	}
	if c.MinConfidence == 0 {
		c.MinConfidence = 0.5
	}
	return c
}

// Line is one recognised text line.
type Line struct {
	Text string
	// Confidence is the mean probability of the line's characters, [0,1].
	Confidence float64
	// Box is where the line was found, in the input image's coordinates.
	Box preprocess.RotatedRect
	// Image is the line as the recogniser saw it (before resizing).
	Image *image.RGBA
}

// Engine runs the detector and the recogniser. It is safe for concurrent
// use.
type Engine struct {
	cfg  Config
	det  *ort.DynamicAdvancedSession
	rec  *ort.DynamicAdvancedSession
	dict []string // index 0 is the CTC blank
}

var (
	initOnce sync.Once
	initErr  error
)

// initRuntime loads ONNX Runtime once per process; the library cannot be
// changed afterwards.
func initRuntime(library string) error {
	initOnce.Do(func() {
		ort.SetSharedLibraryPath(library)
		if err := ort.InitializeEnvironment(); err != nil {
			initErr = fmt.Errorf("ocr: cannot load ONNX Runtime %q (install ONNX Runtime 1.31+ and set -onnxruntime or $ONNXRUNTIME_LIB): %w", library, err)
		}
	})
	return initErr
}

// NewEngine loads ONNX Runtime and the embedded models.
func NewEngine(cfg Config) (*Engine, error) {
	cfg = cfg.withDefaults()
	if err := initRuntime(cfg.Library); err != nil {
		return nil, err
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	if cfg.Threads > 0 {
		if err := opts.SetIntraOpNumThreads(cfg.Threads); err != nil {
			return nil, err
		}
		if err := opts.SetInterOpNumThreads(1); err != nil {
			return nil, err
		}
	}
	e := &Engine{cfg: cfg, dict: loadDict(dictText)}
	if e.det, err = newSession(detModel, opts); err != nil {
		return nil, fmt.Errorf("ocr: detector: %w", err)
	}
	if e.rec, err = newSession(recModel, opts); err != nil {
		e.Close()
		return nil, fmt.Errorf("ocr: recogniser: %w", err)
	}
	return e, nil
}

func newSession(model []byte, opts *ort.SessionOptions) (*ort.DynamicAdvancedSession, error) {
	in, out, err := ort.GetInputOutputInfoWithONNXData(model)
	if err != nil {
		return nil, err
	}
	if len(in) != 1 || len(out) != 1 {
		return nil, errors.New("model must have one input and one output")
	}
	return ort.NewDynamicAdvancedSessionWithONNXData(model, []string{in[0].Name}, []string{out[0].Name}, opts)
}

// loadDict builds the recogniser's class list: CTC blank, the dictionary,
// and a space (the models are trained with use_space_char).
func loadDict(text string) []string {
	chars := []string{""}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		chars = append(chars, strings.TrimRight(l, "\r"))
	}
	return append(chars, " ")
}

// Close releases the models.
func (e *Engine) Close() error {
	if e.det != nil {
		e.det.Destroy()
		e.det = nil
	}
	if e.rec != nil {
		e.rec.Destroy()
		e.rec = nil
	}
	return nil
}

// Recognize finds and reads the text lines in img, in reading order (top
// to bottom, then left to right). Lines below Config.MinConfidence are
// dropped.
func (e *Engine) Recognize(img image.Image) ([]Line, error) {
	src := toRGBA(img)
	boxes, err := e.detect(src)
	if err != nil {
		return nil, err
	}
	var lines []Line
	for _, box := range boxes {
		lineImg := preprocess.ExtractRect(src, box, 1)
		text, conf, err := e.recognize(lineImg)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" || conf < e.cfg.MinConfidence {
			continue
		}
		lines = append(lines, Line{Text: strings.TrimSpace(text), Confidence: conf, Box: box, Image: lineImg})
	}
	sortReadingOrder(lines)
	return lines, nil
}

// sortReadingOrder sorts lines top to bottom; lines whose tops are within
// half a line height of each other are ordered left to right.
func sortReadingOrder(lines []Line) {
	top := func(l Line) float64 { return l.Box.Center.Y - l.Box.H/2 }
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if d := top(a) - top(b); d < -a.Box.H/2 || d > a.Box.H/2 {
			return d < 0
		}
		return a.Box.Center.X < b.Box.Center.X
	})
}
