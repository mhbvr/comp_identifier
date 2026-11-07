// Package ocr wraps a local Tesseract installation behind a small,
// engine-agnostic interface.
//
// Two backends are provided:
//
//   - LibEngine uses libtesseract in-process through Tesseract's C API
//     (cgo). It is the default when the package is built with cgo enabled
//     and needs the Tesseract and Leptonica development files and
//     pkg-config at build time (Debian/Ubuntu: libtesseract-dev
//     libleptonica-dev pkg-config).
//   - CLIEngine runs the `tesseract` executable. It needs no cgo and is
//     used as the fallback when cgo is disabled.
//
// Both need trained data for the requested language (tesseract-ocr-eng).
package ocr

import (
	"image"
	"sort"
	"strings"
)

// PageSegMode mirrors Tesseract's page segmentation modes that are useful
// for component markings.
type PageSegMode int

const (
	PSMAuto        PageSegMode = 3  // fully automatic page segmentation
	PSMSingleBlock PageSegMode = 6  // a single uniform block of text
	PSMSingleLine  PageSegMode = 7  // a single text line
	PSMSparseText  PageSegMode = 11 // as much text as possible, no order
)

// DefaultWhitelist covers what is printed on IC and transistor packages:
// upper-case letters, digits and a few separators. Restricting the
// character set noticeably improves accuracy on short part numbers.
const DefaultWhitelist = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-/.+"

// Config configures an Engine.
type Config struct {
	// Languages, default ["eng"].
	Languages []string
	// Whitelist of characters; empty disables the whitelist. Use
	// DefaultWhitelist for component markings.
	Whitelist string
	// PageSegMode, default PSMSingleBlock.
	PageSegMode PageSegMode
	// TessdataPrefix overrides the tessdata directory if non-empty.
	TessdataPrefix string
	// DisableDictionary turns off Tesseract's word lists, which only hurt
	// on part numbers and date codes. Default true via DefaultConfig.
	DisableDictionary bool
	// DPI hint passed to Tesseract (avoids "invalid resolution" guesses).
	DPI int
}

// DefaultConfig returns a configuration tuned for component markings.
func DefaultConfig() Config {
	return Config{
		Languages:         []string{"eng"},
		Whitelist:         DefaultWhitelist,
		PageSegMode:       PSMSingleBlock,
		DisableDictionary: true,
		DPI:               300,
	}
}

func (c Config) withDefaults() Config {
	if len(c.Languages) == 0 {
		c.Languages = []string{"eng"}
	}
	if c.PageSegMode == 0 {
		c.PageSegMode = PSMSingleBlock
	}
	if c.DPI <= 0 {
		c.DPI = 300
	}
	return c
}

// Char is a single recognised character.
type Char struct {
	Text string
	// Confidence in [0,100].
	Confidence float64
}

// Word is a single recognised word.
type Word struct {
	Text string
	// Confidence in [0,100]. For Tesseract's LSTM engine this is close to
	// the confidence of the worst character, so one doubtful character
	// zeroes an otherwise good word; see CharConfidence.
	Confidence float64
	Box        image.Rectangle
	// Line identifies the text line the word belongs to; words with equal
	// Line are on the same line. Lines are numbered in reading order.
	Line int
	// Chars holds per-character results when the engine provides them
	// (LibEngine does, CLIEngine does not).
	Chars []Char
}

// CharConfidence returns the mean per-character confidence, falling back
// to the word confidence when character data is unavailable.
func (w Word) CharConfidence() float64 {
	if len(w.Chars) == 0 {
		return w.Confidence
	}
	var s float64
	for _, c := range w.Chars {
		s += c.Confidence
	}
	return s / float64(len(w.Chars))
}

// Result is the output of one OCR pass over one image.
type Result struct {
	Words []Word
}

// Lines groups words into text lines in reading order.
func (r *Result) Lines() []string {
	byLine := map[int][]Word{}
	var keys []int
	for _, w := range r.Words {
		if _, ok := byLine[w.Line]; !ok {
			keys = append(keys, w.Line)
		}
		byLine[w.Line] = append(byLine[w.Line], w)
	}
	sort.Ints(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		ws := byLine[k]
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].Box.Min.X < ws[j].Box.Min.X })
		parts := make([]string, len(ws))
		for i, w := range ws {
			parts[i] = w.Text
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// Text returns all lines joined by newlines.
func (r *Result) Text() string { return strings.Join(r.Lines(), "\n") }

// Engine recognises text in an image. Implementations are not safe for
// concurrent use; create one engine per goroutine.
type Engine interface {
	Recognize(img image.Image) (*Result, error)
	Close() error
}

// lineKey builds a unique, ordered line number from Tesseract's
// block/paragraph/line hierarchy.
func lineKey(block, par, line int) int { return block*1_000_000 + par*1_000 + line }
