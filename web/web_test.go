package web

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/mhbvr/comp_identifier/camera"
	"github.com/mhbvr/comp_identifier/claude"
	"github.com/mhbvr/comp_identifier/marking"
	"github.com/mhbvr/comp_identifier/ocr"
	"github.com/mhbvr/comp_identifier/tracker"
)

func newServer(t *testing.T) (*httptest.Server, *camera.Latest) {
	t.Helper()
	frames := &camera.Latest{}
	source := func() ([]byte, uint64, error) {
		f, err := frames.Get()
		return f.JPEG, f.Seq, err
	}
	never := func(image.Image) (*marking.Result, error) { panic("not called") }
	tr := tracker.New(tracker.Config{}, source, nil, never)
	srv := httptest.NewServer(New(tr, frames, nil))
	t.Cleanup(srv.Close)
	return srv, frames
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPages(t *testing.T) {
	srv, _ := newServer(t)
	code, body := get(t, srv.URL+"/")
	for _, want := range []string{`<img src="/live.mjpeg"`, `id="overlay"`, `id="result"`, "Place a component", "fetch(\"/panel\")"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET / (%d) lacks %q", code, want)
		}
	}
	code, body = get(t, srv.URL+"/panel")
	if code != 200 || !strings.Contains(body, `id="overlay"`) || !strings.Contains(body, `id="result"`) || strings.Contains(body, "<html") {
		t.Errorf("GET /panel = %d %q", code, body)
	}
	if code, _ := get(t, srv.URL+"/img/1/crop.png"); code != 404 {
		t.Errorf("unknown image: status %d, want 404", code)
	}
	if code, _ := get(t, srv.URL+"/nothing"); code != 404 {
		t.Errorf("unknown path: status %d, want 404", code)
	}
}

func TestLiveRelaysFrames(t *testing.T) {
	srv, frames := newServer(t)
	frames.Put([]byte("JPEG1"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live.mjpeg", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/x-mixed-replace") {
		t.Fatalf("Content-Type %q", ct)
	}
	// Read until the first frame has arrived.
	r := bufio.NewReader(resp.Body)
	var got strings.Builder
	for !strings.Contains(got.String(), "JPEG1") {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v (got %q)", err, got.String())
		}
		got.WriteString(line)
	}
	if !strings.Contains(got.String(), "Content-Length: 5") {
		t.Errorf("part header missing: %q", got.String())
	}
}

// TestClaudeIdentify drives the whole button flow: a tracked reading, POST
// /claude/{id} to a fake Claude API, and the answer in the panel.
func TestClaudeIdentify(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn",
		 "content":[{"type":"text","text":"{\"marking\":\"LM35DZ\",\"part_number\":\"LM35DZ\",\"manufacturer\":\"Texas Instruments\",\"description\":\"temperature sensor, TO-92\",\"datasheet_url\":\"https://www.ti.com/lit/ds/symlink/lm35.pdf\",\"confidence\":\"high\"}"}],
		 "usage":{"input_tokens":480,"output_tokens":120}}`)
	}))
	defer api.Close()

	// A tracker that sees one component and "recognises" it instantly.
	frame := sceneJPEG()
	var seq atomic.Uint64
	source := func() ([]byte, uint64, error) { return frame, seq.Add(1), nil }
	recognize := func(image.Image) (*marking.Result, error) {
		return &marking.Result{
			Text: "LM35", Lines: []string{"LM35"}, Confidence: 90,
			Upright:   image.NewRGBA(image.Rect(0, 0, 400, 300)),
			TextLines: []ocr.Line{{Text: "LM35", Confidence: 0.9, Image: image.NewRGBA(image.Rect(0, 0, 80, 30))}},
		}, nil
	}
	tr := tracker.New(tracker.Config{DetectEvery: 5 * time.Millisecond}, source, nil, recognize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.Run(ctx)

	id := claude.New("", option.WithBaseURL(api.URL), option.WithAPIKey("test"))
	srv := httptest.NewServer(New(tr, &camera.Latest{}, id))
	defer srv.Close()

	// The 400x300 crop is sent as is: 160 image tokens + the prompt.
	waitPanel(t, srv.URL, fmt.Sprintf("about %d input tokens", claude.EstimateTokens(400, 300)))
	cur := tr.Snapshot().Current
	post := func() int {
		resp, err := http.Post(fmt.Sprintf("%s/claude/%d", srv.URL, cur.ID), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(); code != http.StatusAccepted {
		t.Fatalf("POST /claude: %d", code)
	}
	panel := waitPanel(t, srv.URL, "Claude: LM35DZ")
	for _, want := range []string{"temperature sensor, TO-92", `href="https://www.ti.com/lit/ds/symlink/lm35.pdf"`, "480 input + 120 output tokens", `href="https://duckduckgo.com/?q=LM35DZ`} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel lacks %q", want)
		}
	}
	// A second request for the same reading is not sent again.
	if code := post(); code != http.StatusNoContent || calls.Load() != 1 {
		t.Errorf("second POST: %d, API calls %d", code, calls.Load())
	}
	// Another reading ID is rejected.
	resp, _ := http.Post(srv.URL+"/claude/999", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("POST for a stale reading: %d", resp.StatusCode)
	}
}

// waitPanel polls /panel until it contains want.
func waitPanel(t *testing.T, base, want string) string {
	t.Helper()
	var body string
	for i := 0; i < 500; i++ {
		_, body = get(t, base+"/panel")
		if strings.Contains(body, want) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("panel never showed %q; last: %s", want, body)
	return ""
}

// sceneJPEG is white paper with a dark component.
func sceneJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for i := range img.Pix {
		img.Pix[i] = 240
	}
	for y := 90; y < 135; y++ {
		for x := 100; x < 180; x++ {
			img.SetRGBA(x, y, color.RGBA{35, 35, 38, 255})
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}
