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
//
// With a component database (package inventory), the panel also looks the
// reading up among the known components and offers to add the part as an
// item (button or the "a" key); the /inventory pages browse and edit the
// database (inventory.go).
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
	"github.com/mhbvr/comp_identifier/inventory"
	"github.com/mhbvr/comp_identifier/preprocess"
	"github.com/mhbvr/comp_identifier/tracker"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"points": points,
	"label":  label,
	"ms":     func(d time.Duration) int64 { return d.Milliseconds() },
	"pct":    func(f float64) float64 { return 100 * f },
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
	inv     *inventory.DB      // nil: no component database
	mux     *http.ServeMux

	mu    sync.Mutex
	ask   ask        // the latest Claude request
	added added      // the latest item added from the camera
	match matchCache // the latest database lookup
}

// added is an item added to the database from one reading.
type added struct {
	id   int // tracker.Reading.ID
	item *inventory.Item
	err  error
}

// matchCache keeps the latest lookup: the panel is rendered several times
// a second for a reading that rarely changes.
type matchCache struct {
	text    string
	claude  *claude.Identification
	gen     uint64
	matches []inventory.Match
	err     error
}

// ask is a Claude identification of one reading.
type ask struct {
	id   int    // tracker.Reading.ID
	text string // text the user read on the part, sent with the photo
	busy bool
	res  *claude.Identification
	err  error
}

// maxUserText caps the text the user can send with a photo.
const maxUserText = 500

// ClaudeView is what the page shows about Claude for the current reading.
type ClaudeView struct {
	ID       int // reading to identify
	Model    string
	Estimate int // approximate input tokens of the request
	Busy     bool
	Text     string // the user's text sent with the request
	Result   *claude.Identification
	Err      error
}

// InventoryView is what the page shows about the component database for
// the current reading.
type InventoryView struct {
	ID       int  // reading the buttons add
	Ready    bool // recognition finished: the reading can be added
	ByClaude bool // the lookup used Claude's answer instead of the local reading
	// Matches are the known components the reading belongs to, best first.
	Matches []inventory.Match
	// New is Claude's part, offered as a new component when no match has
	// its name.
	New   *claude.Identification
	Added *inventory.Item // the item added from this reading
	Err   error
}

// view is the data of the page templates.
type view struct {
	tracker.Snapshot
	Claude       *ClaudeView    // nil when disabled or there is no reading
	Inventory    *InventoryView // nil without a database or a reading
	HasInventory bool           // for the navigation links
	HasClaude    bool           // for the text-for-Claude box
}

// New returns the app's HTTP handler. id may be nil to disable Claude, inv
// nil to run without a component database.
func New(t *tracker.Tracker, frames *camera.Latest, id *claude.Identifier, inv *inventory.DB) *Server {
	s := &Server{tracker: t, frames: frames, claude: id, inv: inv, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.page("index.html"))
	s.mux.HandleFunc("GET /panel", s.page("panel"))
	s.mux.HandleFunc("GET /live.mjpeg", s.live)
	s.mux.HandleFunc("GET /img/{id}/{kind}", s.image)
	s.mux.HandleFunc("POST /claude/{id}", s.identify)
	if inv != nil {
		s.mux.HandleFunc("POST /add/{id}", s.addItem)
		s.inventoryRoutes()
	}
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
	v := view{Snapshot: s.tracker.Snapshot(), HasInventory: s.inv != nil, HasClaude: s.claude != nil}
	r := v.Current
	if r == nil {
		return v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ask ask
	if s.ask.id == r.ID {
		ask = s.ask
	}
	if s.claude != nil {
		v.Claude = &ClaudeView{ID: r.ID, Model: s.claude.Model(), Busy: ask.busy, Text: ask.text, Result: ask.res, Err: ask.err}
		if cfg, err := png.DecodeConfig(bytes.NewReader(r.CropPNG)); err == nil {
			v.Claude.Estimate = claude.EstimateTokens(cfg.Width, cfg.Height)
		}
	}
	if s.inv != nil {
		v.Inventory = s.inventoryView(v.Snapshot, ask.res)
	}
	return v
}

// inventoryView looks the current reading up in the database: Claude's
// answer if there is one (its part number, then its reading of the
// marking), else the local reading. s.mu is held.
func (s *Server) inventoryView(snap tracker.Snapshot, res *claude.Identification) *InventoryView {
	r := snap.Current
	iv := &InventoryView{ID: r.ID, Ready: snap.Status != tracker.Working, ByClaude: res != nil}
	if s.added.id == r.ID {
		iv.Added, iv.Err = s.added.item, s.added.err
	}
	c := &s.match
	if gen := s.inv.Generation(); c.text != r.Text || c.claude != res || c.gen != gen || c.matches == nil && c.err == nil {
		*c = matchCache{text: r.Text, claude: res, gen: gen}
		c.matches, c.err = s.lookup(r.Text, res)
		if c.matches == nil {
			c.matches = []inventory.Match{} // cached "no match"
		}
	}
	iv.Matches = c.matches
	if iv.Err == nil {
		iv.Err = c.err
	}
	if res != nil && strings.TrimSpace(res.PartNumber) != "" {
		iv.New = res
		for _, m := range iv.Matches {
			if strings.EqualFold(m.Name, strings.TrimSpace(res.PartNumber)) {
				iv.New = nil
			}
		}
	}
	return iv
}

func (s *Server) lookup(text string, res *claude.Identification) ([]inventory.Match, error) {
	if res == nil {
		return s.inv.Match(text)
	}
	var out []inventory.Match
	seen := map[int64]bool{}
	if c, err := s.inv.ComponentByName(res.PartNumber); err == nil {
		out = append(out, inventory.Match{Component: *c, By: "part number", Key: c.Name})
		seen[c.ID] = true
	} else if err != inventory.ErrNotFound {
		return nil, err
	}
	ms, err := s.inv.Match(strings.ReplaceAll(res.Marking, " / ", "\n"))
	for _, m := range ms {
		if !seen[m.ID] {
			out = append(out, m)
		}
	}
	return out, err
}

// addItem adds the current reading to the database as an item (POST
// /add/{id}): of the known component given by the form value "component",
// or, with "component=new", of a new component made from Claude's answer.
// The item's photo is the upright crop; its marking is Claude's reading if
// Claude was asked, else the local one. Each reading is added at most once.
func (s *Server) addItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	snap := s.tracker.Snapshot()
	cur := snap.Current
	switch {
	case cur == nil || cur.ID != id:
		http.Error(w, "that component is no longer in view", http.StatusConflict)
		return
	case snap.Status == tracker.Working:
		http.Error(w, "still recognising", http.StatusConflict)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.added.id == id && s.added.item != nil {
		w.WriteHeader(http.StatusNoContent) // already added
		return
	}
	var res *claude.Identification
	if s.ask.id == id {
		res = s.ask.res
	}
	var comp inventory.Component
	if v := r.FormValue("component"); v == "new" {
		if res == nil {
			http.Error(w, "no identification by Claude to add", http.StatusConflict)
			return
		}
		comp = inventory.Component{Name: res.PartNumber, Description: describe(res)}
	} else if comp.ID, _ = strconv.ParseInt(v, 10, 64); comp.ID == 0 {
		http.Error(w, "no component given", http.StatusBadRequest)
		return
	}
	text := cur.Text
	if res != nil && res.Marking != "" {
		text = res.Marking
	}
	img, err := png.Decode(bytes.NewReader(cur.CropPNG))
	if err != nil {
		img = nil
	}
	item, created, err := s.inv.AddItem(comp, text, img)
	s.added = added{id: id, item: item, err: err}
	if err != nil {
		log.Printf("inventory: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("inventory: item %d of %s (component %d, new %v)", item.ID, item.ComponentName, item.ComponentID, created)
	if created && res.DatasheetURL != "" {
		go s.fetchDatasheet(item.ComponentID, res.DatasheetURL)
	}
	w.WriteHeader(http.StatusCreated)
}

// fetchDatasheet downloads a new component's datasheet from the URL Claude
// gave. The URL is unchecked: failures are only logged, and the datasheet
// can be uploaded on the inventory page instead.
func (s *Server) fetchDatasheet(id int64, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.inv.FetchDatasheet(ctx, id, url); err != nil {
		log.Printf("inventory: component %d: %v", id, err)
	}
}

// describe is a component description made from Claude's answer.
func describe(res *claude.Identification) string {
	d := strings.TrimSpace(res.Description)
	if m := strings.TrimSpace(res.Manufacturer); m != "" {
		if d == "" {
			return m
		}
		d += " (" + m + ")"
	}
	return d
}

// identify sends the current reading to Claude (POST /claude/{id}), with
// the form value "text": what the user read on the part themselves, for
// markings the camera can't see. A reading is sent again only after a
// failure or with a different text.
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
	text := strings.TrimSpace(r.FormValue("text"))
	if len(text) > maxUserText {
		http.Error(w, fmt.Sprintf("the text for Claude is longer than %d characters", maxUserText), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ask.id == id && (s.ask.busy || s.ask.res != nil && s.ask.text == text) {
		w.WriteHeader(http.StatusNoContent) // already asked
		return
	}
	s.ask = ask{id: id, text: text, busy: true}
	go func() {
		res, err := s.askClaude(cur, text)
		s.mu.Lock()
		if s.ask.id == id {
			s.ask = ask{id: id, text: text, res: res, err: err}
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

func (s *Server) askClaude(r *tracker.Reading, text string) (*claude.Identification, error) {
	img, err := png.Decode(bytes.NewReader(r.CropPNG))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.claude.Identify(ctx, img, strings.Join(r.Lines, " / "), text)
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
