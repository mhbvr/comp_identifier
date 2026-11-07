// Package web serves the component identifier UI.
//
// The page is rendered with html/template. The live video is a plain
// <img> showing an MJPEG stream relayed from the camera, so it needs no
// JavaScript. Everything that changes (the rectangle over the video and
// the result panel) is the "panel" template, which the page re-fetches a
// few times a second with a few lines of inline JavaScript.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mhbvr/comp_identifier/camera"
	"github.com/mhbvr/comp_identifier/preprocess"
	"github.com/mhbvr/comp_identifier/tracker"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"points": points,
	"label":  label,
	"ms":     func(d time.Duration) int64 { return d.Milliseconds() },
}).ParseFS(templateFS, "templates/*.html"))

// Server is the HTTP handler of the app.
type Server struct {
	tracker *tracker.Tracker
	frames  *camera.Latest
	mux     *http.ServeMux
}

// New returns the app's HTTP handler.
func New(t *tracker.Tracker, frames *camera.Latest) *Server {
	s := &Server{tracker: t, frames: frames, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.page("index.html"))
	s.mux.HandleFunc("GET /panel", s.page("panel"))
	s.mux.HandleFunc("GET /live.mjpeg", s.live)
	s.mux.HandleFunc("GET /img/{id}/{kind}", s.image)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// page renders a template with the tracker's current snapshot.
func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := templates.ExecuteTemplate(w, name, s.tracker.Snapshot()); err != nil {
			log.Printf("template %s: %v", name, err)
		}
	}
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
