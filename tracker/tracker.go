// Package tracker watches the live camera stream, detects when a component
// is placed, moved or removed, and runs marking recognition on it.
//
// Lifecycle of one component:
//
//	nothing detected ──place──► Working (orange) ──recognised──► Reliable (green)
//	                                 ▲      │                    or Unreliable (red)
//	                                 │      └─ up to MaxAttempts captures
//	         moved / replaced ───────┘
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
	Variant    string
	Rotation   int
	Elapsed    time.Duration // time spent recognising, all attempts
	At         time.Time
	CropPNG    []byte // colour crop of the component, turned so the text reads upright
	OCRPNG     []byte // the image the winning text was read from
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
	rect     preprocess.RotatedRect
	stable   int
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
	if t.cur != nil && !moved(t.cur.rect, seg.Rect, img.Bounds()) {
		t.cur.rect = seg.Rect
		t.cur.stable++
		return
	}
	// New, moved or replaced component: start over.
	t.nextID++
	t.cur = &component{id: t.nextID, rect: seg.Rect, stable: 1}
}

// moved reports whether b is a different placement than a: the centre
// shifted by more than 3% of the frame diagonal, the size changed by more
// than 15%, or the rectangle turned by more than 8°.
func moved(a, b preprocess.RotatedRect, frame image.Rectangle) bool {
	diag := math.Hypot(float64(frame.Dx()), float64(frame.Dy()))
	shift := math.Hypot(a.Center.X-b.Center.X, a.Center.Y-b.Center.Y)
	size := math.Abs(a.W*a.H-b.W*b.H) / math.Max(a.W*a.H, 1)
	turn := math.Mod(math.Abs(a.Angle-b.Angle), math.Pi)
	turn = math.Min(turn, math.Pi-turn)
	// Nearly square bodies have no stable long axis; ignore their angle.
	if math.Max(a.W/a.H, b.W/b.H) < 1.15 {
		turn = 0
	}
	return shift > 0.03*diag || size > 0.15 || turn > 8*math.Pi/180
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
	return a.Best.Score > b.Best.Score
}

func newReading(c *component) *Reading {
	res := c.best
	return &Reading{
		ID: c.id, Text: res.Text, Lines: res.Lines, Confidence: res.Confidence,
		Reliable: c.reliable, Attempts: c.attempts,
		Variant: res.Best.Variant, Rotation: res.Best.Rotation,
		Elapsed: c.elapsed, At: time.Now(),
		CropPNG: encodePNG(preprocess.RotateRGBA(res.Preprocess.Crop, res.Best.Rotation/90)),
		OCRPNG:  encodePNG(res.Best.Image),
	}
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
