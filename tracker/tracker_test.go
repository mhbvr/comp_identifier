package tracker

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhbvr/comp_identifier/marking"
	"github.com/mhbvr/comp_identifier/preprocess"
)

// scene renders a 320x240 frame: white paper, optionally with a dark
// "component" whose left edge is at x. (Small, so that the tests stay fast
// under the race detector.)
func scene(component bool, x int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for i := 0; i < 320; i++ {
			c := color.RGBA{240, 242, 242, 255}
			if component && i >= x && i < x+80 && y >= 90 && y < 135 {
				c = color.RGBA{35, 35, 38, 255}
			}
			img.SetRGBA(i, y, c)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

// camera is a scripted frame source.
type camera struct {
	mu    sync.Mutex
	frame []byte
	seq   uint64
}

func (c *camera) show(f []byte) {
	c.mu.Lock()
	c.frame, c.seq = f, c.seq+1
	c.mu.Unlock()
}

func (c *camera) source() ([]byte, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++ // every call sees a "new" frame, like a live stream
	return c.frame, c.seq, nil
}

// fakeRecognizer returns the queued readings in order (the last one
// repeats) and counts calls.
type fakeRecognizer struct {
	calls    atomic.Int32
	readings []*marking.Result
}

func reading(text string, score float64, reliable bool) *marking.Result {
	return &marking.Result{
		Text: text, Lines: []string{text}, Confidence: 90, Reliable: reliable,
		Best:       marking.Candidate{Variant: "clean", Score: score, Image: image.NewGray(image.Rect(0, 0, 4, 4))},
		Preprocess: &preprocess.Result{Crop: image.NewRGBA(image.Rect(0, 0, 4, 4))},
	}
}

func (f *fakeRecognizer) recognize(image.Image) (*marking.Result, error) {
	n := int(f.calls.Add(1)) - 1
	return f.readings[min(n, len(f.readings)-1)], nil
}

func start(t *testing.T, rec *fakeRecognizer) (*Tracker, *camera) {
	t.Helper()
	cam := &camera{}
	cam.show(scene(false, 0))
	tr := New(Config{DetectEvery: 5 * time.Millisecond}, cam.source, nil, rec.recognize)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tr.Run(ctx)
	return tr, cam
}

// waitFor polls the snapshot until cond holds.
func waitFor(t *testing.T, tr *Tracker, what string, cond func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := tr.Snapshot(); cond(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; snapshot %+v", what, tr.Snapshot())
	return Snapshot{}
}

func TestReliableReadingStopsAfterOneAttempt(t *testing.T) {
	rec := &fakeRecognizer{readings: []*marking.Result{reading("LM35", 5, true)}}
	tr, cam := start(t, rec)
	waitFor(t, tr, "empty", func(s Snapshot) bool { return s.Status == Empty && s.FrameW == 320 })

	cam.show(scene(true, 100))
	s := waitFor(t, tr, "reliable", func(s Snapshot) bool { return s.Status == Reliable })
	if s.Rect == nil || s.Current == nil || s.Current.Text != "LM35" || s.Current.Attempts != 1 {
		t.Fatalf("snapshot = %+v, current %+v", s, s.Current)
	}
	if len(tr.Image(s.Current.ID, "crop")) == 0 || len(tr.Image(s.Current.ID, "ocr")) == 0 {
		t.Error("reading images missing")
	}
	time.Sleep(50 * time.Millisecond)
	if n := rec.calls.Load(); n != 1 {
		t.Errorf("recognised %d times, want 1", n)
	}

	// Remove it: back to empty, reading moves to the history.
	cam.show(scene(false, 0))
	s = waitFor(t, tr, "removed", func(s Snapshot) bool { return s.Status == Empty })
	if len(s.History) != 1 || s.History[0].Text != "LM35" || s.Rect != nil || s.Current != nil {
		t.Fatalf("after removal: %+v", s)
	}
}

func TestUnreliableReadingKeepsBestOfAttempts(t *testing.T) {
	rec := &fakeRecognizer{readings: []*marking.Result{
		reading("LM3S", 2, false), reading("LM35", 4, false), reading("XX", 1, false),
	}}
	tr, cam := start(t, rec)
	cam.show(scene(true, 100))
	s := waitFor(t, tr, "unreliable", func(s Snapshot) bool { return s.Status == Unreliable })
	if s.Current.Text != "LM35" || s.Current.Attempts != 3 || s.Current.Reliable {
		t.Errorf("current = %+v", s.Current)
	}
}

func TestMovedComponentIsRecognisedAgain(t *testing.T) {
	rec := &fakeRecognizer{readings: []*marking.Result{reading("A1", 3, true)}}
	tr, cam := start(t, rec)
	cam.show(scene(true, 50))
	first := waitFor(t, tr, "first reading", func(s Snapshot) bool { return s.Status == Reliable })

	cam.show(scene(true, 190)) // replaced / moved far
	s := waitFor(t, tr, "second reading", func(s Snapshot) bool {
		return s.Status == Reliable && s.Current != nil && s.Current.ID != first.Current.ID
	})
	if len(s.History) != 1 || rec.calls.Load() != 2 {
		t.Errorf("history %d entries, %d recognitions; want 1 and 2", len(s.History), rec.calls.Load())
	}
}

func TestMoved(t *testing.T) {
	frame := image.Rect(0, 0, 1000, 750)
	a := preprocess.RotatedRect{Center: preprocess.Point{X: 500, Y: 400}, W: 200, H: 100}
	b := a
	b.Center.X += 10 // jitter
	if moved(a, b, frame) {
		t.Error("10 px jitter counted as a move")
	}
	b.Center.X += 60
	if !moved(a, b, frame) {
		t.Error("70 px shift not counted as a move")
	}
	c := a
	c.Angle = 0.3
	if !moved(a, c, frame) {
		t.Error("17° turn not counted as a move")
	}
}
