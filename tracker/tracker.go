// Package tracker watches the live camera stream, detects when a component
// is placed, moved or removed, and runs marking recognition on it.
//
// Lifecycle of one component:
//
//	nothing detected ──place──► Working (orange) ──recognised──► Reliable (green)
//	                                 ▲      │                    or Unreliable (red)
//	                                 │      └─ up to MaxAttempts captures
//	         moved / replaced ───────┘   (MoveFrames detections elsewhere)
//	removed (MissFrames detections without it) ──► Empty
//
// Detection is cheap (segmentation of a downscaled stream frame) and runs
// several times a second. Recognition is expensive (OCR, about a second)
// and starts only once the component has stayed still for StableFrames
// detections. Each attempt uses a new full-resolution capture and the best
// reading wins; attempts stop early once a reading is reliable (see
// marking.Result.Reliable).
package tracker

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"sync"
	"time"

	"github.com/mhbvr/comp_identifier/marking"
	"github.com/mhbvr/comp_identifier/preprocess"
)

// Status of the scene, as shown by the colour of the rectangle.
type Status string

const (
	Empty      Status = "empty"      // no component
	Working    Status = "working"    // detected, recognition in progress (orange)
	Reliable   Status = "reliable"   // recognised with high confidence (green)
	Unreliable Status = "unreliable" // recognised with low confidence (red)
)

// Config tunes the tracker. Zero fields take the defaults.
type Config struct {
	DetectEvery  time.Duration // detection period; default 200 ms
	StableFrames int           // still detections before recognising; default 3
	MissFrames   int           // empty detections before "removed"; default 3
	MoveFrames   int           // consecutive detections elsewhere before "moved"; default 2
	MaxAttempts  int           // recognition attempts per component; default 3
	Detect       preprocess.Options
}

func (c Config) withDefaults() Config {
	if c.DetectEvery <= 0 {
		c.DetectEvery = 200 * time.Millisecond
	}
	if c.StableFrames <= 0 {
		c.StableFrames = 3
	}
	if c.MissFrames <= 0 {
		c.MissFrames = 3
	}
	if c.MoveFrames <= 0 {
		c.MoveFrames = 2
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	return c
}

// Source provides stream frames: Seq increases with every new frame (0 =
// none yet). camera.Latest satisfies it through a small adapter in main.
type Source func() (jpeg []byte, seq uint64, err error)

// Capture returns a high-resolution JPEG for recognition.
type Capture func(ctx context.Context) ([]byte, error)

// Recognize reads the marking in an image (marking.Reader.Read).
type Recognize func(img image.Image) (*marking.Result, error)

// Reading is the recognition result for one component.
type Reading struct {
	ID         int
	Text       string
	Lines      []string
	Confidence float64
	Reliable   bool
	Attempts   int
	Rotation   int           // degrees clockwise the crop was turned to read it
	Elapsed    time.Duration // time spent recognising, all attempts
	At         time.Time
	CropPNG    []byte // colour crop of the component, turned so the text reads upright
	OCRPNG     []byte // the text lines the recogniser read, stacked (nil if none)
}

// Snapshot is the state shown to the user.
type Snapshot struct {
	Status         Status
	FrameW, FrameH int                     // stream frame size
	Rect           *preprocess.RotatedRect // detected component, frame coordinates
	Current        *Reading                // reading of the component in view (may be partial)
	Err            string                  // camera or recognition problem
}

// Tracker runs detection and recognition. Create with New, start with Run.
type Tracker struct {
	cfg       Config
	source    Source
	capture   Capture
	recognize Recognize

	mu   sync.Mutex
	snap Snapshot

	// Owned by the Run goroutine.
	cur         *component
	misses      int
	lastSeq     uint64
	recognizing bool // the recognizer is single-threaded: one attempt at a time
	nextID      int
	camErr      error // stream problem, cleared when frames arrive again
	recogErr    error // last recognition failure, cleared by a success
}

// component is the one currently in view.
type component struct {
	id       int
	anchor   preprocess.RotatedRect // where it was first detected
	rect     preprocess.RotatedRect // latest detection, for display
	stable   int                    // detections at the anchor
	away     int                    // consecutive detections elsewhere
	attempts int
	best     *marking.Result
	reading  *Reading
	reliable bool
	done     bool
	elapsed  time.Duration
}

// New creates a tracker. capture may be nil, in which case the latest
// stream frame is used for recognition.
func New(cfg Config, source Source, capture Capture, recognize Recognize) *Tracker {
	return &Tracker{
		cfg: cfg.withDefaults(), source: source, capture: capture, recognize: recognize,
		snap: Snapshot{Status: Empty, FrameW: 4, FrameH: 3},
	}
}

// Snapshot returns the current state. It is safe to call from any goroutine.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap
}

// Image returns a PNG of the current reading ("crop" or "ocr"), or nil if
// id is not the current reading.
func (t *Tracker) Image(id int, kind string) []byte {
	r := t.Snapshot().Current
	switch {
	case r == nil || r.ID != id:
		return nil
	case kind == "crop":
		return r.CropPNG
	default:
		return r.OCRPNG
	}
}

type attempt struct {
	id      int
	res     *marking.Result
	err     error
	elapsed time.Duration
}

// Run detects and recognises until ctx is cancelled.
func (t *Tracker) Run(ctx context.Context) {
	tick := time.NewTicker(t.cfg.DetectEvery)
	defer tick.Stop()
	done := make(chan attempt)
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-done:
			t.recognizing = false
			t.finishAttempt(a)
		case <-tick.C:
			t.detect()
			if c := t.cur; c != nil && !c.done && !t.recognizing && c.stable >= t.cfg.StableFrames {
				t.recognizing = true
				go t.runAttempt(ctx, c.id, done)
			}
		}
		t.publish()
	}
}

// detect segments the newest stream frame and updates the component.
func (t *Tracker) detect() {
	data, seq, err := t.source()
	t.camErr = err
	if seq == 0 || seq == t.lastSeq {
		return // no new frame
	}
	t.lastSeq = seq
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.camErr = err
		return
	}
	t.mu.Lock()
	t.snap.FrameW, t.snap.FrameH = img.Bounds().Dx(), img.Bounds().Dy()
	t.mu.Unlock()

	seg, err := preprocess.Segment(img, t.cfg.Detect)
	if err != nil {
		t.misses++
		if t.cur != nil && t.misses >= t.cfg.MissFrames {
			t.cur = nil // removed
		}
		return
	}
	t.misses = 0
	if c := t.cur; c != nil {
		if samePlace(c.anchor, seg.Rect) {
			c.rect, c.away = seg.Rect, 0
			c.stable++
			return
		}
		// A single odd detection (glare, a hand passing) is ignored.
		if c.away++; c.away < t.cfg.MoveFrames {
			return
		}
	}
	// New, moved or replaced component: start over.
	t.nextID++
	t.cur = &component{id: t.nextID, anchor: seg.Rect, rect: seg.Rect, stable: 1}
}

// samePlace reports whether two detections are the same placement of a
// component: most of one rectangle lies inside the other. Lighting changes
// make the detected outline gain or lose the shadow and the leads, which
// changes its size by up to ~75% and, for nearly square outlines, swaps
// width and height; containment is insensitive to both. A moved or
// replaced component overlaps little.
func samePlace(a, b preprocess.RotatedRect) bool {
	return max(inside(a, b), inside(b, a)) >= 0.5
}

// inside is the fraction of rectangle a that lies inside rectangle b,
// estimated on a 9x9 grid of points.
func inside(a, b preprocess.RotatedRect) float64 {
	const n = 9
	ua := [2]float64{math.Cos(a.Angle), math.Sin(a.Angle)}
	ub := [2]float64{math.Cos(b.Angle), math.Sin(b.Angle)}
	in := 0
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			u := ((float64(i)+0.5)/n - 0.5) * a.W
			v := ((float64(j)+0.5)/n - 0.5) * a.H
			x := a.Center.X + u*ua[0] - v*ua[1] - b.Center.X
			y := a.Center.Y + u*ua[1] + v*ua[0] - b.Center.Y
			if math.Abs(x*ub[0]+y*ub[1]) <= b.W/2 && math.Abs(-x*ub[1]+y*ub[0]) <= b.H/2 {
				in++
			}
		}
	}
	return float64(in) / (n * n)
}

// runAttempt captures an image and recognises it (in its own goroutine).
func (t *Tracker) runAttempt(ctx context.Context, id int, done chan<- attempt) {
	start := time.Now()
	a := attempt{id: id}
	var data []byte
	if t.capture != nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		data, a.err = t.capture(cctx)
		cancel()
	}
	if t.capture == nil || a.err != nil {
		data, _, _ = t.source() // fall back to the stream frame
	}
	var img image.Image
	if img, a.err = jpeg.Decode(bytes.NewReader(data)); a.err == nil {
		a.res, a.err = t.recognize(img)
	}
	a.elapsed = time.Since(start)
	select {
	case done <- a:
	case <-ctx.Done():
	}
}

// finishAttempt merges a recognition attempt into the current component.
func (t *Tracker) finishAttempt(a attempt) {
	c := t.cur
	if c == nil || c.id != a.id {
		return // the component left or changed meanwhile
	}
	c.attempts++
	c.elapsed += a.elapsed
	t.recogErr = nil
	switch {
	case errors.Is(a.err, preprocess.ErrNoComponent):
		// Capture missed the component (e.g. a hand in the way): retry.
	case a.err != nil:
		t.recogErr = a.err
	default:
		if better(a.res, c.best) {
			c.best = a.res
			c.reliable = a.res.Reliable
		}
	}
	c.done = c.reliable || c.attempts >= t.cfg.MaxAttempts
	if c.best != nil {
		c.reading = newReading(c)
	}
}

// better reports whether reading a beats b: a reliable reading beats an
// unreliable one, otherwise the higher OCR score wins.
func better(a, b *marking.Result) bool {
	if b == nil || a.Reliable != b.Reliable {
		return b == nil || a.Reliable
	}
	return a.Score > b.Score
}

func newReading(c *component) *Reading {
	res := c.best
	r := &Reading{
		ID: c.id, Text: res.Text, Lines: res.Lines, Confidence: res.Confidence,
		Reliable: c.reliable, Attempts: c.attempts,
		Rotation: res.Rotation, Elapsed: c.elapsed, At: time.Now(),
		CropPNG: encodePNG(res.Upright),
	}
	if lines := res.LinesImage(); lines != nil {
		r.OCRPNG = encodePNG(lines)
	}
	return r
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	if img == nil || png.Encode(&b, img) != nil {
		return nil
	}
	return b.Bytes()
}

// publish copies the Run goroutine's state into the snapshot.
func (t *Tracker) publish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snap.Status, t.snap.Rect, t.snap.Current, t.snap.Err = Empty, nil, nil, ""
	if err := errors.Join(t.camErr, t.recogErr); err != nil {
		t.snap.Err = err.Error()
	}
	c := t.cur
	if c == nil {
		return
	}
	r := c.rect
	t.snap.Rect, t.snap.Current = &r, c.reading
	switch {
	case !c.done:
		t.snap.Status = Working
	case c.reliable:
		t.snap.Status = Reliable
	default:
		t.snap.Status = Unreliable
	}
}
