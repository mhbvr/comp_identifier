// Package web serves the component identifier UI.
//
// The page is rendered with html/template. The live video is a plain
// <img> showing an MJPEG stream relayed from the camera, so it needs no
// JavaScript. Everything that changes (the rectangle over the video and
// the result panel) is the "panel" template, which the page re-fetches a
// few times a second with a few lines of inline JavaScript.
//
// Identification with the Claude API is optional and runs only when the
// user asks for it (button or the "i" key) for the reading on screen.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"image/png"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhbvr/comp_identifier/camera"
	"github.com/mhbvr/comp_identifier/claude"
	"github.com/mhbvr/comp_identifier/preprocess"
	"github.com/mhbvr/comp_identifier/tracker"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"points": points,
	"label":  label,
	"ms":     func(d time.Duration) int64 { return d.Milliseconds() },
	// searchURL is a free, always valid datasheet link for a part number.
	"searchURL": func(part string) string {
		return "https://duckduckgo.com/?q=" + url.QueryEscape(part+" datasheet")
	},
}).ParseFS(templateFS, "templates/*.html"))

// Server is the HTTP handler of the app.
type Server struct {
	tracker *tracker.Tracker
	frames  *camera.Latest
	claude  *claude.Identifier // nil: Claude identification disabled
	mux     *http.ServeMux

	mu  sync.Mutex
	ask ask // the latest Claude request
}

// ask is a Claude identification of one reading.
type ask struct {
	id   int // tracker.Reading.ID
	busy bool
	res  *claude.Identification
	err  error
}

// ClaudeView is what the page shows about Claude for the current reading.
type ClaudeView struct {
	ID       int // reading to identify
	Model    string
	Estimate int // approximate input tokens of the request
	Busy     bool
	Result   *claude.Identification
	Err      error
}

// view is the data of the page templates.
type view struct {
	tracker.Snapshot
	Claude *ClaudeView // nil when disabled or there is no reading
}

// New returns the app's HTTP handler. id may be nil to disable Claude.
func New(t *tracker.Tracker, frames *camera.Latest, id *claude.Identifier) *Server {
	s := &Server{tracker: t, frames: frames, claude: id, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.page("index.html"))
	s.mux.HandleFunc("GET /panel", s.page("panel"))
	s.mux.HandleFunc("GET /live.mjpeg", s.live)
	s.mux.HandleFunc("GET /img/{id}/{kind}", s.image)
	s.mux.HandleFunc("POST /claude/{id}", s.identify)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// page renders a template with the tracker's current snapshot.
func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := templates.ExecuteTemplate(w, name, s.view()); err != nil {
			log.Printf("template %s: %v", name, err)
		}
	}
}

func (s *Server) view() view {
	v := view{Snapshot: s.tracker.Snapshot()}
	r := v.Current
	if s.claude == nil || r == nil {
		return v
	}
	v.Claude = &ClaudeView{ID: r.ID, Model: s.claude.Model()}
	if cfg, err := png.DecodeConfig(bytes.NewReader(r.CropPNG)); err == nil {
		v.Claude.Estimate = claude.EstimateTokens(cfg.Width, cfg.Height)
	}
	s.mu.Lock()
	if s.ask.id == r.ID {
		v.Claude.Busy, v.Claude.Result, v.Claude.Err = s.ask.busy, s.ask.res, s.ask.err
	}
	s.mu.Unlock()
	return v
}

// identify sends the current reading to Claude (POST /claude/{id}). Each
// reading is sent at most once, unless the previous attempt failed.
func (s *Server) identify(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	cur := s.tracker.Snapshot().Current
	switch {
	case s.claude == nil:
		http.Error(w, "Claude identification is disabled", http.StatusNotFound)
		return
	case cur == nil || cur.ID != id:
		http.Error(w, "that component is no longer in view", http.StatusConflict)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ask.id == id && (s.ask.busy || s.ask.res != nil) {
		w.WriteHeader(http.StatusNoContent) // already asked
		return
	}
	s.ask = ask{id: id, busy: true}
	go func() {
		res, err := s.askClaude(cur)
		s.mu.Lock()
		if s.ask.id == id {
			s.ask = ask{id: id, res: res, err: err}
		}
		s.mu.Unlock()
		if err != nil {
			log.Printf("claude: %v", err)
		} else {
			log.Printf("claude: %s (%s), %d input + %d output tokens", res.PartNumber, res.Model, res.InputTokens, res.OutputTokens)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) askClaude(r *tracker.Reading) (*claude.Identification, error) {
	img, err := png.Decode(bytes.NewReader(r.CropPNG))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.claude.Identify(ctx, img, strings.Join(r.Lines, " / "))
}

// live relays the camera's MJPEG stream: every new frame, unchanged.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	const boundary = "frame"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	var seq uint64
	for {
		f, err := s.frames.Next(r.Context(), seq)
		if err != nil {
			return
		}
		seq = f.Seq
		fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(f.JPEG))
		w.Write(f.JPEG)
		if _, err := w.Write([]byte("\r\n")); err != nil || rc.Flush() != nil {
			return
		}
	}
}

// image serves the crop or OCR input of a reading as PNG. The current
// reading can improve between attempts, so the page adds the attempt
// number to the URL; a given URL therefore never changes and is cacheable.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	kind := strings.TrimSuffix(r.PathValue("kind"), ".png")
	data := s.tracker.Image(id, kind)
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Write(data)
}

// points formats a rectangle's corners for an SVG polygon.
func points(r *preprocess.RotatedRect) string {
	var b strings.Builder
	for _, p := range r.Corners() {
		fmt.Fprintf(&b, "%.0f,%.0f ", p.X, p.Y)
	}
	return strings.TrimSpace(b.String())
}

// label is the human-readable status line.
func label(s tracker.Status) string {
	switch s {
	case tracker.Working:
		return "Recognising…"
	case tracker.Reliable:
		return "Recognised (high confidence)"
	case tracker.Unreliable:
		return "Recognised (low confidence)"
	}
	return "Place a component under the camera"
}
