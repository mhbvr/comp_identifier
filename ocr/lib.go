//go:build cgo

package ocr

/*
#cgo pkg-config: tesseract lept
#include <stdlib.h>
#include <tesseract/capi.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"strconv"
	"strings"
	"unsafe"
)

// LibEngine runs Tesseract in-process through its official C API (cgo).
// Recognition, layout and per-character confidences come from a single
// pass, and pixels are handed over without encoding.
type LibEngine struct {
	api *C.TessBaseAPI
}

// NewLibEngine creates an in-process Tesseract engine.
func NewLibEngine(cfg Config) (*LibEngine, error) {
	cfg = cfg.withDefaults()
	api := C.TessBaseAPICreate()
	if api == nil {
		return nil, errors.New("ocr: TessBaseAPICreate failed")
	}
	e := &LibEngine{api: api}

	// Init-only variables have to be passed to Init4.
	vars := map[string]string{"debug_file": "/dev/null"}
	if cfg.DisableDictionary {
		vars["load_system_dawg"] = "0"
		vars["load_freq_dawg"] = "0"
	}
	var keys, vals []*C.char
	for k, v := range vars {
		keys = append(keys, C.CString(k))
		vals = append(vals, C.CString(v))
	}
	defer func() {
		for i := range keys {
			C.free(unsafe.Pointer(keys[i]))
			C.free(unsafe.Pointer(vals[i]))
		}
	}()

	var datapath *C.char
	if cfg.TessdataPrefix != "" {
		datapath = C.CString(cfg.TessdataPrefix)
		defer C.free(unsafe.Pointer(datapath))
	}
	lang := C.CString(strings.Join(cfg.Languages, "+"))
	defer C.free(unsafe.Pointer(lang))

	rc := C.TessBaseAPIInit4(api, datapath, lang, C.OEM_LSTM_ONLY, nil, 0,
		&keys[0], &vals[0], C.size_t(len(keys)), C.FALSE)
	if rc != 0 {
		e.Close()
		return nil, fmt.Errorf("ocr: Tesseract init failed for language %q (is the traineddata installed?)", strings.Join(cfg.Languages, "+"))
	}
	C.TessBaseAPISetPageSegMode(api, C.TessPageSegMode(cfg.PageSegMode))
	e.setVar("user_defined_dpi", strconv.Itoa(cfg.DPI))
	if cfg.Whitelist != "" {
		e.setVar("tessedit_char_whitelist", cfg.Whitelist)
	}
	return e, nil
}

func (e *LibEngine) setVar(k, v string) {
	ck, cv := C.CString(k), C.CString(v)
	C.TessBaseAPISetVariable(e.api, ck, cv)
	C.free(unsafe.Pointer(ck))
	C.free(unsafe.Pointer(cv))
}

// Recognize implements Engine.
func (e *LibEngine) Recognize(img image.Image) (*Result, error) {
	if e.api == nil {
		return nil, errors.New("ocr: engine is closed")
	}
	g := toGray(img)
	w, h := g.Rect.Dx(), g.Rect.Dy()
	if w == 0 || h == 0 {
		return &Result{}, nil
	}
	buf := C.CBytes(g.Pix)
	defer C.free(buf)
	C.TessBaseAPISetImage(e.api, (*C.uchar)(buf), C.int(w), C.int(h), 1, C.int(g.Stride))
	defer C.TessBaseAPIClear(e.api)
	if C.TessBaseAPIRecognize(e.api, nil) != 0 {
		return nil, errors.New("ocr: recognition failed")
	}
	it := C.TessBaseAPIGetIterator(e.api)
	res := &Result{}
	if it == nil {
		return res, nil
	}
	defer C.TessResultIteratorDelete(it)
	pit := C.TessResultIteratorGetPageIterator(it)

	line := -1
	var cur *Word
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.Text) != "" {
			res.Words = append(res.Words, *cur)
		}
		cur = nil
	}
	for {
		if C.TessPageIteratorIsAtBeginningOf(pit, C.RIL_TEXTLINE) != 0 {
			line++
		}
		if C.TessPageIteratorIsAtBeginningOf(pit, C.RIL_WORD) != 0 {
			flush()
			cur = &Word{
				Text:       iterText(it, C.RIL_WORD),
				Confidence: float64(C.TessResultIteratorConfidence(it, C.RIL_WORD)),
				Box:        iterBox(pit, C.RIL_WORD),
				Line:       line,
			}
		}
		if cur != nil {
			if s := iterText(it, C.RIL_SYMBOL); s != "" {
				cur.Chars = append(cur.Chars, Char{
					Text:       s,
					Confidence: float64(C.TessResultIteratorConfidence(it, C.RIL_SYMBOL)),
				})
			}
		}
		if C.TessResultIteratorNext(it, C.RIL_SYMBOL) == 0 {
			break
		}
	}
	flush()
	return res, nil
}

func iterText(it *C.TessResultIterator, level C.TessPageIteratorLevel) string {
	cs := C.TessResultIteratorGetUTF8Text(it, level)
	if cs == nil {
		return ""
	}
	defer C.TessDeleteText(cs)
	return strings.TrimSpace(C.GoString(cs))
}

func iterBox(pit *C.TessPageIterator, level C.TessPageIteratorLevel) image.Rectangle {
	var l, t, r, b C.int
	if C.TessPageIteratorBoundingBox(pit, level, &l, &t, &r, &b) == 0 {
		return image.Rectangle{}
	}
	return image.Rect(int(l), int(t), int(r), int(b))
}

func toGray(img image.Image) *image.Gray {
	if g, ok := img.(*image.Gray); ok && g.Rect.Min == (image.Point{}) {
		return g
	}
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(g, g.Rect, img, b.Min, draw.Src)
	return g
}

// Close releases the Tesseract instance.
func (e *LibEngine) Close() error {
	if e.api != nil {
		C.TessBaseAPIEnd(e.api)
		C.TessBaseAPIDelete(e.api)
		e.api = nil
	}
	return nil
}

// NewEngine returns the preferred engine for this build: in-process
// libtesseract when cgo is available.
func NewEngine(cfg Config) (Engine, error) { return NewLibEngine(cfg) }
