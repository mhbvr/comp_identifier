// Command compid reads the marking on photos of electronic components.
//
// Usage:
//
//	compid [flags] photo.jpg [photo2.jpg ...]
//	compid -eval components_photos/labels.csv
//
// For every photo it prints the recognised text. With -debug DIR it also
// writes the intermediate images (detected rectangle, aligned crop, body
// mask, every OCR variant and the winning OCR input) to DIR.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mhbvr/comp_identifier/marking"
	"github.com/mhbvr/comp_identifier/ocr"
	"github.com/mhbvr/comp_identifier/preprocess"
)

type flags struct {
	debugDir   string
	jsonOut    bool
	all        bool
	eval       string
	workers    int
	useCLI     bool
	tessdata   string
	lang       string
	psm        int
	whitelist  string
	charHeight int
}

func main() {
	ocr.LimitOpenMPThreads()

	var f flags
	flag.StringVar(&f.debugDir, "debug", "", "write intermediate images to this directory")
	flag.BoolVar(&f.jsonOut, "json", false, "print results as JSON lines")
	flag.BoolVar(&f.all, "all", false, "also print every OCR candidate")
	flag.StringVar(&f.eval, "eval", "", "evaluate against a labels CSV (file,expected-token per line; photos are resolved relative to the CSV)")
	flag.IntVar(&f.workers, "workers", runtime.NumCPU(), "photos processed in parallel")
	flag.BoolVar(&f.useCLI, "cli", false, "use the tesseract executable instead of libtesseract")
	flag.StringVar(&f.tessdata, "tessdata", "", "tessdata directory (e.g. one holding tessdata_best models)")
	flag.StringVar(&f.lang, "lang", "eng", "Tesseract language(s), '+'-separated")
	flag.IntVar(&f.psm, "psm", int(ocr.PSMSingleBlock), "Tesseract page segmentation mode")
	flag.StringVar(&f.whitelist, "whitelist", ocr.DefaultWhitelist, "characters Tesseract may output (empty: no restriction)")
	flag.IntVar(&f.charHeight, "char-height", 0, "character height in pixels for OCR images (0: default)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] photo... | -eval labels.csv\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	var err error
	switch {
	case f.eval != "":
		err = runEval(f)
	case flag.NArg() > 0:
		err = runPhotos(f, flag.Args())
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "compid:", err)
		os.Exit(1)
	}
}

func (f flags) options() marking.Options {
	opt := marking.DefaultOptions()
	opt.OCR.Languages = strings.Split(f.lang, "+")
	opt.OCR.TessdataPrefix = f.tessdata
	opt.OCR.PageSegMode = ocr.PageSegMode(f.psm)
	opt.OCR.Whitelist = f.whitelist
	opt.Preprocess.Enhance.CharHeight = f.charHeight
	return opt
}

func (f flags) newReader() (*marking.Reader, error) {
	opt := f.options()
	if !f.useCLI {
		return marking.NewReader(opt)
	}
	e, err := ocr.NewCLIEngine(opt.OCR)
	if err != nil {
		return nil, err
	}
	return marking.NewReaderWithEngine(e, opt), nil
}

type outcome struct {
	path string
	res  *marking.Result
	err  error
	dur  time.Duration
}

// process reads all paths with a pool of readers, calling done in input
// order.
func process(f flags, paths []string, done func(outcome)) error {
	n := max(1, min(f.workers, len(paths)))
	results := make([]outcome, len(paths))
	ready := make([]chan struct{}, len(paths))
	for i := range ready {
		ready[i] = make(chan struct{})
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		r, err := f.newReader()
		if err != nil {
			close(jobs)
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
			for i := range jobs {
				start := time.Now()
				res, err := readFile(r, paths[i])
				results[i] = outcome{path: paths[i], res: res, err: err, dur: time.Since(start)}
				close(ready[i])
			}
		}()
	}
	go func() {
		for i := range paths {
			jobs <- i
		}
		close(jobs)
	}()
	for i := range paths {
		<-ready[i]
		done(results[i])
	}
	wg.Wait()
	return nil
}

func readFile(r *marking.Reader, path string) (*marking.Result, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r.Read(img)
}

type jsonResult struct {
	File       string   `json:"file"`
	Text       string   `json:"text"`
	Lines      []string `json:"lines"`
	Confidence float64  `json:"confidence"`
	Variant    string   `json:"variant,omitempty"`
	Rotation   int      `json:"rotation"`
	Polarity   string   `json:"polarity,omitempty"`
	Millis     int64    `json:"ms"`
	Error      string   `json:"error,omitempty"`
}

func runPhotos(f flags, paths []string) error {
	if f.debugDir != "" {
		if err := os.MkdirAll(f.debugDir, 0o755); err != nil {
			return err
		}
	}
	enc := json.NewEncoder(os.Stdout)
	failed := 0
	err := process(f, paths, func(o outcome) {
		if o.err != nil {
			failed++
		}
		if f.debugDir != "" && o.res != nil {
			if err := writeDebug(f.debugDir, o.path, o.res); err != nil {
				fmt.Fprintln(os.Stderr, "compid: debug output:", err)
			}
		}
		if f.jsonOut {
			jr := jsonResult{File: o.path, Millis: o.dur.Milliseconds()}
			if o.err != nil {
				jr.Error = o.err.Error()
			} else {
				jr.Text, jr.Lines, jr.Confidence = o.res.Text, o.res.Lines, o.res.Confidence
				jr.Variant, jr.Rotation = o.res.Best.Variant, o.res.Best.Rotation
				jr.Polarity = o.res.Preprocess.Polarity.String()
			}
			enc.Encode(jr)
			return
		}
		if o.err != nil {
			fmt.Printf("%s: error: %v\n", o.path, o.err)
			return
		}
		r := o.res
		fmt.Printf("%s: %q (conf %.0f, %s@%d°, %v)\n", o.path, strings.ReplaceAll(r.Text, "\n", " / "),
			r.Confidence, r.Best.Variant, r.Best.Rotation, o.dur.Round(time.Millisecond))
		if f.all {
			for _, c := range r.Candidates {
				fmt.Printf("    %-10s %3d° score %5.1f  %q\n", c.Variant, c.Rotation, c.Score, strings.ReplaceAll(c.OCR.Text(), "\n", " / "))
			}
		}
	})
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d photos failed", failed, len(paths))
	}
	return nil
}

func runEval(f flags) error {
	fh, err := os.Open(f.eval)
	if err != nil {
		return err
	}
	defer fh.Close()
	base := filepath.Dir(f.eval)
	var paths, want []string
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		file, tok, ok := strings.Cut(line, ",")
		if !ok {
			return fmt.Errorf("bad labels line %q", line)
		}
		paths = append(paths, filepath.Join(base, strings.TrimSpace(file)))
		want = append(want, strings.TrimSpace(tok))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("no labels found")
	}
	if f.debugDir != "" {
		if err := os.MkdirAll(f.debugDir, 0o755); err != nil {
			return err
		}
	}
	start := time.Now()
	i, exact, near := 0, 0, 0
	err = process(f, paths, func(o outcome) {
		tok := want[i]
		i++
		if f.debugDir != "" && o.res != nil {
			writeDebug(f.debugDir, o.path, o.res)
		}
		if o.err != nil {
			fmt.Printf("ERR   %-28s %-14s %v\n", filepath.Base(o.path), tok, o.err)
			return
		}
		m := marking.MatchToken(o.res.Text, tok)
		status := "MISS"
		switch {
		case m == 0:
			status = "ok"
			exact++
			near++
		case m == 1:
			status = "near"
			near++
		}
		fmt.Printf("%-5s %-28s %-14s %q\n", status, filepath.Base(o.path), tok, strings.ReplaceAll(o.res.Text, "\n", " / "))
	})
	if err != nil {
		return err
	}
	n := len(paths)
	fmt.Printf("\nexact: %d/%d (%.0f%%)  within 1 edit: %d/%d (%.0f%%)  time: %v (%v/photo)\n",
		exact, n, 100*float64(exact)/float64(n), near, n, 100*float64(near)/float64(n),
		time.Since(start).Round(time.Millisecond), (time.Since(start) / time.Duration(n)).Round(time.Millisecond))
	return nil
}

func writeDebug(dir, path string, r *marking.Result) error {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	p := r.Preprocess
	save := func(suffix string, img image.Image) error {
		fh, err := os.Create(filepath.Join(dir, base+"_"+suffix+".png"))
		if err != nil {
			return err
		}
		defer fh.Close()
		return png.Encode(fh, img)
	}
	if err := save("1_crop", p.Crop); err != nil {
		return err
	}
	save("2_body", p.BodyMask.ToGray())
	if p.Text != nil {
		save("3_text", p.Text.Mask.ToGray())
	}
	for _, v := range p.Variants {
		save("4_var_"+v.Name, v.Image)
	}
	if r.Best.Image != nil {
		save("5_best", r.Best.Image)
	}
	if src, err := loadImage(path); err == nil {
		save("0_detect", overlay(src, p.Rect))
	}
	return nil
}

func loadImage(path string) (image.Image, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	return img, err
}

// overlay draws the detected rectangle on a copy of the photo.
func overlay(src image.Image, r preprocess.RotatedRect) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Rect, src, b.Min, draw.Src)
	red := color.RGBA{255, 0, 0, 255}
	c := r.Corners()
	for i := 0; i < 4; i++ {
		a, e := c[i], c[(i+1)%4]
		n := int(max(abs(a.X-e.X), abs(a.Y-e.Y))) + 1
		for s := 0; s <= n; s++ {
			x := int(a.X + (e.X-a.X)*float64(s)/float64(n))
			y := int(a.Y + (e.Y-a.Y)*float64(s)/float64(n))
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					out.Set(x+dx, y+dy, red)
				}
			}
		}
	}
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
