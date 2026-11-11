package web

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/mhbvr/comp_identifier/camera"
	"github.com/mhbvr/comp_identifier/claude"
	"github.com/mhbvr/comp_identifier/inventory"
	"github.com/mhbvr/comp_identifier/marking"
	"github.com/mhbvr/comp_identifier/ocr"
	"github.com/mhbvr/comp_identifier/tracker"
)

// trackedServer is the app with a tracker that sees one component and
// reads text from it reliably at once.
func trackedServer(t *testing.T, text string, id *claude.Identifier, inv *inventory.DB) (*httptest.Server, *tracker.Tracker) {
	t.Helper()
	frame := sceneJPEG()
	var seq atomic.Uint64
	source := func() ([]byte, uint64, error) { return frame, seq.Add(1), nil }
	recognize := func(image.Image) (*marking.Result, error) {
		return &marking.Result{
			Text: text, Lines: strings.Split(text, "\n"), Confidence: 0.99, Reliable: true,
			Upright:   image.NewRGBA(image.Rect(0, 0, 400, 300)),
			TextLines: []ocr.Line{{Text: text, Confidence: 0.99, Image: image.NewRGBA(image.Rect(0, 0, 80, 30))}},
		}, nil
	}
	tr := tracker.New(tracker.Config{DetectEvery: 5 * time.Millisecond}, source, nil, recognize)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tr.Run(ctx)
	srv := httptest.NewServer(New(tr, &camera.Latest{}, id, inv))
	t.Cleanup(srv.Close)
	return srv, tr
}

func openDB(t *testing.T) *inventory.DB {
	t.Helper()
	inv, err := inventory.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inv.Close() })
	return inv
}

func postForm(t *testing.T, u string, form url.Values) *http.Response {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// TestAddKnownComponent: the local reading names a known component; "Add"
// stores an item with the crop as its photo.
func TestAddKnownComponent(t *testing.T) {
	inv := openDB(t)
	cid, _ := inv.AddComponent(inventory.Component{Name: "LM35DZ", Description: "temperature sensor"})
	inv.AddComponent(inventory.Component{Name: "LM35"})
	srv, tr := trackedServer(t, "3AT4\nLM35DZ", nil, inv)

	panel := waitPanel(t, srv.URL, "Add item of LM35DZ")
	lm35, _ := inv.ComponentByName("LM35")
	for _, want := range []string{"temperature sensor", "found by name", "or add to:",
		fmt.Sprintf("component #%d", cid), fmt.Sprintf("LM35 (#%d)", lm35.ID)} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel lacks %q", want)
		}
	}
	if strings.Contains(panel, `data-component="new"`) {
		t.Error("new component offered without Claude")
	}

	id := tr.Snapshot().Current.ID
	add := func(form url.Values) int { return postForm(t, fmt.Sprintf("%s/add/%d", srv.URL, id), form).StatusCode }
	if code := add(url.Values{"component": {"new"}}); code != http.StatusConflict {
		t.Errorf("new component without Claude: %d", code)
	}
	if code := add(url.Values{"component": {fmt.Sprint(cid)}}); code != http.StatusCreated {
		t.Fatalf("add: %d", code)
	}
	waitPanel(t, srv.URL, fmt.Sprintf("Component ID <strong>#%d</strong>", cid))
	if code := add(url.Values{"component": {fmt.Sprint(cid)}}); code != http.StatusNoContent {
		t.Errorf("second add of the same reading: %d", code)
	}
	items, _ := inv.Items(cid, "")
	if len(items) != 1 || items[0].Marking != "3AT4\nLM35DZ" || items[0].Photo == "" {
		t.Fatalf("items %+v", items)
	}
	// The photo is served.
	if code, body := get(t, srv.URL+"/db/photos/"+items[0].Photo); code != 200 || !strings.HasPrefix(body, "\xff\xd8") {
		t.Errorf("photo: %d", code)
	}
	if code, _ := get(t, srv.URL+"/db/photos/"); code != 404 {
		t.Errorf("directory listing: %d", code)
	}
	if code := postForm(t, srv.URL+"/add/999", url.Values{"component": {fmt.Sprint(cid)}}).StatusCode; code != http.StatusConflict {
		t.Errorf("stale reading: %d", code)
	}
}

// TestAddNewComponentFromClaude: the part is unknown; after Claude names
// it, "Add new component" creates the component (with the datasheet from
// Claude's URL) and the item.
func TestAddNewComponentFromClaude(t *testing.T) {
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "%PDF-1.5 LM35")
	}))
	defer ds.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		answer := fmt.Sprintf(`{"marking":"3AT4 / LM35DZ","part_number":"LM35DZ","manufacturer":"Texas Instruments","description":"temperature sensor, TO-92","datasheet_url":"%s/lm35.pdf","confidence":"high"}`, ds.URL)
		fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn",
		 "content":[{"type":"text","text":%q}],"usage":{"input_tokens":480,"output_tokens":120}}`, answer)
	}))
	defer api.Close()

	inv := openDB(t)
	inv.AddComponent(inventory.Component{Name: "NE555P"})
	srv, tr := trackedServer(t, "3AT4\nLN35D2", claude.New("", option.WithBaseURL(api.URL), option.WithAPIKey("test")), inv)
	waitPanel(t, srv.URL, "Not in the database. Move the part to read it again, or identify it with Claude.")
	id := tr.Snapshot().Current.ID
	if code := postForm(t, fmt.Sprintf("%s/claude/%d", srv.URL, id), nil).StatusCode; code != http.StatusAccepted {
		t.Fatalf("POST /claude: %d", code)
	}
	waitPanel(t, srv.URL, "Add new component LM35DZ with this item")
	if code := postForm(t, fmt.Sprintf("%s/add/%d", srv.URL, id), url.Values{"component": {"new"}}).StatusCode; code != http.StatusCreated {
		t.Fatalf("add: %d", code)
	}
	c, err := inv.ComponentByName("LM35DZ")
	if err != nil {
		t.Fatal(err)
	}
	waitPanel(t, srv.URL, fmt.Sprintf("Component ID <strong>#%d</strong>", c.ID))
	if c.Description != "temperature sensor, TO-92 (Texas Instruments)" || c.Items != 1 {
		t.Errorf("component %+v", c)
	}
	items, _ := inv.Items(c.ID, "")
	if len(items) != 1 || items[0].Marking != "3AT4\nLM35DZ" {
		t.Errorf("items %+v (marking should be Claude's)", items)
	}
	// The datasheet is downloaded in the background.
	for i := 0; i < 200 && c.Datasheet == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		c, _ = inv.Component(c.ID)
	}
	if data, err := os.ReadFile(filepath.Join(inv.DatasheetsDir(), c.Datasheet)); err != nil || string(data) != "%PDF-1.5 LM35" {
		t.Errorf("datasheet %q: %v", c.Datasheet, err)
	}

	// With the component known, the next identical answer finds it by
	// Claude's part number.
	srv2, tr2 := trackedServer(t, "garbage", claude.New("", option.WithBaseURL(api.URL), option.WithAPIKey("test")), inv)
	waitPanel(t, srv2.URL, "Not in the database")
	postForm(t, fmt.Sprintf("%s/claude/%d", srv2.URL, tr2.Snapshot().Current.ID), nil)
	panel := waitPanel(t, srv2.URL, "Add item of LM35DZ")
	if !strings.Contains(panel, "found by part number") || strings.Contains(panel, "Add new component") {
		t.Errorf("panel: %s", panel)
	}
}

func TestNoDatabase(t *testing.T) {
	srv, _ := trackedServer(t, "LM35", nil, nil)
	panel := waitPanel(t, srv.URL, "LM35")
	if strings.Contains(panel, `class="inventory"`) {
		t.Error("inventory section without a database")
	}
	if code, _ := get(t, srv.URL+"/inventory"); code != 404 {
		t.Errorf("/inventory without a database: %d", code)
	}
	if _, body := get(t, srv.URL+"/"); strings.Contains(body, `href="/inventory"`) {
		t.Error("navigation to the inventory without a database")
	}
}

func TestInventoryPages(t *testing.T) {
	inv := openDB(t)
	srv := httptest.NewServer(New(tracker.New(tracker.Config{}, nil, nil, nil), &camera.Latest{}, nil, inv))
	defer srv.Close()

	if code, body := get(t, srv.URL+"/inventory"); code != 200 || !strings.Contains(body, "The database is empty") {
		t.Errorf("empty list: %d", code)
	}
	resp := postForm(t, srv.URL+"/inventory/components", url.Values{"name": {"TL072IP"}, "description": {"dual op-amp"}})
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/inventory/components/") {
		t.Fatalf("create: %d %q", resp.StatusCode, loc)
	}
	resp = postForm(t, srv.URL+"/inventory/components", url.Values{"name": {"tl072ip"}})
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Errorf("duplicate name accepted: %q", resp.Header.Get("Location"))
	}
	c, _ := inv.ComponentByName("TL072IP")

	postForm(t, srv.URL+loc+"/items", url.Values{"marking": {"31AE9XM\nTL072IP"}})
	code, body := get(t, srv.URL+loc)
	for _, want := range []string{"<h1>TL072IP</h1>", "dual op-amp", "31AE9XM\nTL072IP</textarea>", "1 item", "search datasheet"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("component page (%d) lacks %q", code, want)
		}
	}
	_, body = get(t, srv.URL+"/inventory?q=op-amp")
	if !strings.Contains(body, `href="/inventory/components/`+fmt.Sprint(c.ID)+`"`) {
		t.Error("search by description")
	}
	if !strings.Contains(body, fmt.Sprintf(`<td class="num id">%d</td>`, c.ID)) {
		t.Error("components table lacks the ID column")
	}
	_, body = get(t, srv.URL+"/inventory/items?q=31AE")
	if !strings.Contains(body, "TL072IP</a>") {
		t.Error("items search")
	}
	if !strings.Contains(body, fmt.Sprintf("TL072IP</a></td>\n      <td class=\"num id\">%d</td>", c.ID)) {
		t.Error("items table lacks the component ID after the name")
	}

	// Edit the component and the item, move the item, delete.
	postForm(t, srv.URL+loc, url.Values{"name": {"TL072CP"}, "description": {"op-amp"}})
	if c, _ = inv.Component(c.ID); c.Name != "TL072CP" {
		t.Errorf("rename: %+v", c)
	}
	other, _ := inv.AddComponent(inventory.Component{Name: "TL074"})
	items, _ := inv.Items(c.ID, "")
	postForm(t, fmt.Sprintf("%s/inventory/items/%d", srv.URL, items[0].ID), url.Values{"marking": {"TL074"}, "component": {fmt.Sprint(other)}})
	if it, _ := inv.Item(items[0].ID); it.ComponentID != other || it.Marking != "TL074" {
		t.Errorf("edit item: %+v", it)
	}
	postForm(t, fmt.Sprintf("%s/inventory/items/%d/delete", srv.URL, items[0].ID), nil)
	if n, _ := inv.Items(0, ""); len(n) != 0 {
		t.Errorf("items after delete: %+v", n)
	}
	postForm(t, srv.URL+loc+"/delete", nil)
	if code, _ := get(t, srv.URL+loc); code != 404 {
		t.Errorf("deleted component: %d", code)
	}
}

// fakeClaude is a Claude API that answers every request with answer (the
// JSON the model would write) and records the requests' user content.
func fakeClaude(t *testing.T, answer string) (*claude.Identifier, *atomic.Value, *atomic.Int32) {
	t.Helper()
	var last atomic.Value
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		last.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn",
		 "content":[{"type":"text","text":%q}],"usage":{"input_tokens":300,"output_tokens":80}}`, answer)
	}))
	t.Cleanup(api.Close)
	return claude.New("", option.WithBaseURL(api.URL), option.WithAPIKey("test")), &last, &calls
}

// TestClaudeWithUserText: text typed for Claude goes with the photo; asking
// again with the same text sends nothing, with another text a new request.
func TestClaudeWithUserText(t *testing.T) {
	id, last, calls := fakeClaude(t, `{"marking":"470uF 25V","part_number":"EEU-FR1E471","manufacturer":"Panasonic","description":"electrolytic capacitor 470 uF 25 V, radial","datasheet_url":"","confidence":"medium"}`)
	srv, tr := trackedServer(t, "", id, openDB(t))
	if _, page := get(t, srv.URL+"/"); !strings.Contains(page, `id="note-text"`) {
		t.Fatal("no text box for Claude")
	}
	waitPanel(t, srv.URL, "Identify with Claude")
	u := fmt.Sprintf("%s/claude/%d", srv.URL, tr.Snapshot().Current.ID)
	ask := func(text string) int { return postForm(t, u, url.Values{"text": {text}}).StatusCode }

	if code := ask(" 470uF 25V "); code != http.StatusAccepted {
		t.Fatalf("ask: %d", code)
	}
	panel := waitPanel(t, srv.URL, "Claude: EEU-FR1E471")
	if !strings.Contains(panel, "with your text: <span class=\"mono\">470uF 25V</span>") {
		t.Errorf("panel does not show the text sent: %s", panel)
	}
	if req := last.Load().(string); !strings.Contains(req, "Text the user read on the part: 470uF 25V") {
		t.Errorf("request lacks the user's text: %s", req)
	}
	if code := ask("470uF 25V"); code != http.StatusNoContent || calls.Load() != 1 {
		t.Errorf("same text again: %d, %d calls", code, calls.Load())
	}
	if code := ask("470uF 35V"); code != http.StatusAccepted {
		t.Errorf("new text: %d", code)
	}
	// The new request replaced the old answer before POST returned, so the
	// next answer shown is the new one.
	panel = waitPanel(t, srv.URL, "Claude: EEU-FR1E471")
	if !strings.Contains(panel, "470uF 35V</span>") || calls.Load() != 2 {
		t.Errorf("%d calls, want 2; panel: %s", calls.Load(), panel)
	}
	if code := ask(strings.Repeat("x", maxUserText+1)); code != http.StatusBadRequest {
		t.Errorf("text too long: %d", code)
	}
}

// TestDescribeComponent: "Ask Claude" on the new-component form, and the
// datasheet URL it fills in is downloaded after the component is added.
func TestDescribeComponent(t *testing.T) {
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "%PDF-1.4 NE555")
	}))
	defer ds.Close()
	id, last, _ := fakeClaude(t, fmt.Sprintf(`{"part_number":"NE555P","manufacturer":"Texas Instruments","description":"precision timer, DIP-8","datasheet_url":"%s/ne555.pdf","confidence":"high"}`, ds.URL))
	inv := openDB(t)
	srv := httptest.NewServer(New(tracker.New(tracker.Config{}, nil, nil, nil), &camera.Latest{}, id, inv))
	defer srv.Close()

	if _, page := get(t, srv.URL+"/inventory"); !strings.Contains(page, `id="ask-claude"`) {
		t.Error("no Ask Claude button")
	}
	resp, err := http.PostForm(srv.URL+"/inventory/describe", url.Values{"name": {"ne555"}})
	if err != nil {
		t.Fatal(err)
	}
	var a map[string]any
	json.NewDecoder(resp.Body).Decode(&a)
	resp.Body.Close()
	if resp.StatusCode != 200 || a["part_number"] != "NE555P" || a["description"] != "precision timer, DIP-8 (Texas Instruments)" || a["datasheet_url"] != ds.URL+"/ne555.pdf" {
		t.Errorf("describe: %d %v", resp.StatusCode, a)
	}
	if req := last.Load().(string); !strings.Contains(req, "Part number: ne555") {
		t.Errorf("request: %s", req)
	}

	postForm(t, srv.URL+"/inventory/components", url.Values{"name": {"NE555P"}, "description": {a["description"].(string)}, "datasheet_url": {ds.URL + "/ne555.pdf"}})
	c, err := inv.ComponentByName("NE555P")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && c.Datasheet == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		c, _ = inv.Component(c.ID)
	}
	if data, err := os.ReadFile(filepath.Join(inv.DatasheetsDir(), c.Datasheet)); err != nil || string(data) != "%PDF-1.4 NE555" {
		t.Errorf("datasheet %q: %v", c.Datasheet, err)
	}

	// Without Claude: no button, and the endpoint says so.
	srv2 := httptest.NewServer(New(tracker.New(tracker.Config{}, nil, nil, nil), &camera.Latest{}, nil, inv))
	defer srv2.Close()
	if _, page := get(t, srv2.URL+"/inventory"); strings.Contains(page, `id="ask-claude"`) {
		t.Error("Ask Claude button without Claude")
	}
	if code := postForm(t, srv2.URL+"/inventory/describe", url.Values{"name": {"x"}}).StatusCode; code != http.StatusNotFound {
		t.Errorf("describe without Claude: %d", code)
	}
	if _, page := get(t, srv2.URL+"/"); strings.Contains(page, `id="note-text"`) {
		t.Error("text box for Claude without Claude")
	}
}
