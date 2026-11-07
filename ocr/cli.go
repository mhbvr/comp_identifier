package ocr

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CLIEngine runs the `tesseract` executable for each image. It needs no
// cgo, only the binary on PATH (or at Path).
type CLIEngine struct {
	// Path to the tesseract binary; default "tesseract".
	Path string
	// Timeout per invocation; default 30s.
	Timeout time.Duration
	cfg     Config
}

// NewCLIEngine creates an engine that shells out to tesseract.
func NewCLIEngine(cfg Config) (*CLIEngine, error) {
	e := &CLIEngine{Path: "tesseract", Timeout: 30 * time.Second, cfg: cfg.withDefaults()}
	if _, err := exec.LookPath(e.Path); err != nil {
		return nil, fmt.Errorf("ocr: tesseract binary not found: %w", err)
	}
	return e, nil
}

func (e *CLIEngine) args(in string) []string {
	a := []string{in, "stdout",
		"-l", strings.Join(e.cfg.Languages, "+"),
		"--psm", strconv.Itoa(int(e.cfg.PageSegMode)),
		"--dpi", strconv.Itoa(e.cfg.DPI),
	}
	if e.cfg.TessdataPrefix != "" {
		a = append(a, "--tessdata-dir", e.cfg.TessdataPrefix)
	}
	if e.cfg.Whitelist != "" {
		a = append(a, "-c", "tessedit_char_whitelist="+e.cfg.Whitelist)
	}
	if e.cfg.DisableDictionary {
		a = append(a, "-c", "load_system_dawg=F", "-c", "load_freq_dawg=F")
	}
	return append(a, "tsv")
}

// Recognize implements Engine.
func (e *CLIEngine) Recognize(img image.Image) (*Result, error) {
	f, err := os.CreateTemp("", "compid-ocr-*.png")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Path, e.args(f.Name())...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ocr: tesseract failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseTSV(out)
}

// Close implements Engine.
func (e *CLIEngine) Close() error { return nil }

// parseTSV parses Tesseract's TSV output (level 5 rows are words).
func parseTSV(data []byte) (*Result, error) {
	res := &Result{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) < 12 || cols[0] != "5" {
			continue
		}
		text := strings.TrimSpace(cols[11])
		if text == "" {
			continue
		}
		n := make([]int, 10)
		for i := 0; i < 10; i++ {
			n[i], _ = strconv.Atoi(cols[i])
		}
		conf, _ := strconv.ParseFloat(cols[10], 64)
		left, top, w, h := n[6], n[7], n[8], n[9]
		res.Words = append(res.Words, Word{
			Text:       text,
			Confidence: conf,
			Box:        image.Rect(left, top, left+w, top+h),
			Line:       lineKey(n[2], n[3], n[4]),
		})
	}
	return res, sc.Err()
}
