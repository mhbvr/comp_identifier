package web

import (
	"bufio"
	"context"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhbvr/comp_identifier/camera"
	"github.com/mhbvr/comp_identifier/marking"
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
	srv := httptest.NewServer(New(tr, frames))
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
