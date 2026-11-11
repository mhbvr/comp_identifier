package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // uploaded photos
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mhbvr/comp_identifier/inventory"
)

// The inventory pages browse and edit the component database:
//
//	GET  /inventory                        components (search: ?q=), form to add one
//	POST /inventory/components             add a component (and fetch datasheet_url)
//	POST /inventory/describe               ask Claude about a part number (JSON)
//	GET  /inventory/components/{id}        a component and its items
//	POST /inventory/components/{id}        edit name, description, upload a datasheet
//	POST /inventory/components/{id}/delete delete it with its items
//	POST /inventory/components/{id}/datasheet/delete
//	POST /inventory/components/{id}/items  add an item by hand (marking, photo upload)
//	GET  /inventory/items                  all items (search: ?q=)
//	POST /inventory/items/{id}             edit marking, move to another component, replace photo
//	POST /inventory/items/{id}/delete
//	GET  /db/photos/{file}, /db/datasheets/{file}
//
// They are plain HTML forms: every POST redirects back to a page, with
// ?err= when something went wrong.

func (s *Server) inventoryRoutes() {
	s.mux.HandleFunc("GET /inventory", s.componentsPage)
	s.mux.HandleFunc("POST /inventory/components", s.createComponent)
	s.mux.HandleFunc("POST /inventory/describe", s.describeComponent)
	s.mux.HandleFunc("GET /inventory/components/{id}", s.componentPage)
	s.mux.HandleFunc("POST /inventory/components/{id}", s.editComponent)
	s.mux.HandleFunc("POST /inventory/components/{id}/delete", s.deleteComponent)
	s.mux.HandleFunc("POST /inventory/components/{id}/datasheet/delete", s.deleteDatasheet)
	s.mux.HandleFunc("POST /inventory/components/{id}/items", s.createItem)
	s.mux.HandleFunc("GET /inventory/items", s.itemsPage)
	s.mux.HandleFunc("POST /inventory/items/{id}", s.editItem)
	s.mux.HandleFunc("POST /inventory/items/{id}/delete", s.deleteItem)
	s.mux.Handle("GET /db/photos/", http.StripPrefix("/db/photos/", files(s.inv.PhotosDir())))
	s.mux.Handle("GET /db/datasheets/", http.StripPrefix("/db/datasheets/", files(s.inv.DatasheetsDir())))
}

// files serves the files of a directory, without directory listings.
func files(dir string) http.Handler {
	fs := http.FileServerFS(os.DirFS(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || r.URL.Path[len(r.URL.Path)-1] == '/' {
			http.NotFound(w, r)
			return
		}
		fs.ServeHTTP(w, r)
	})
}

// Page data.
type (
	componentsView struct {
		HasClaude  bool // offer "Ask Claude" for the description
		Query      string
		Components []inventory.Component
		Items      int
		Err        string
	}
	componentView struct {
		C     *inventory.Component
		Items []inventory.Item
		All   []inventory.Component // targets for moving an item
		Err   string
	}
	itemsView struct {
		Query string
		Items []inventory.Item
		Err   string
	}
)

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

// back redirects to page, adding err (if any) for the page to show.
func back(w http.ResponseWriter, r *http.Request, page string, err error) {
	if err != nil {
		page += "?err=" + url.QueryEscape(err.Error())
	}
	http.Redirect(w, r, page, http.StatusSeeOther)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func componentURL(id int64) string { return fmt.Sprintf("/inventory/components/%d", id) }

func (s *Server) componentsPage(w http.ResponseWriter, r *http.Request) {
	v := componentsView{HasClaude: s.claude != nil, Query: r.FormValue("q"), Err: r.FormValue("err")}
	var err error
	if v.Components, err = s.inv.Components(v.Query); err != nil {
		v.Err = err.Error()
	}
	for _, c := range v.Components {
		v.Items += c.Items
	}
	s.render(w, "components.html", v)
}

func (s *Server) componentPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.inv.Component(pathID(r))
	if errors.Is(err, inventory.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	v := componentView{C: c, Err: r.FormValue("err")}
	if err == nil {
		v.Items, err = s.inv.Items(c.ID, "")
	}
	if err == nil {
		v.All, err = s.inv.Components("")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "component.html", v)
}

func (s *Server) itemsPage(w http.ResponseWriter, r *http.Request) {
	v := itemsView{Query: r.FormValue("q"), Err: r.FormValue("err")}
	var err error
	if v.Items, err = s.inv.Items(0, v.Query); err != nil {
		v.Err = err.Error()
	}
	s.render(w, "items.html", v)
}

func (s *Server) createComponent(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		back(w, r, "/inventory", err)
		return
	}
	id, err := s.inv.AddComponent(inventory.Component{Name: r.FormValue("name"), Description: r.FormValue("description")})
	if err != nil {
		back(w, r, "/inventory", err)
		return
	}
	// An uploaded datasheet wins over a URL (filled in by "Ask Claude"),
	// which is downloaded in the background.
	data, name, err := upload(r, "datasheet")
	u := strings.TrimSpace(r.FormValue("datasheet_url"))
	switch {
	case err != nil:
	case data != nil:
		err = s.inv.SetDatasheet(id, name, data)
	case strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://"):
		go s.fetchDatasheet(id, u)
	}
	back(w, r, componentURL(id), err)
}

// describeComponent asks Claude about the part number in the form value
// "name", for the "Ask Claude" button of the new-component form. The
// answer is JSON: part_number, description (with the manufacturer),
// datasheet_url, confidence, model and token counts; or error.
func (s *Server) describeComponent(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	if s.claude == nil {
		reply(http.StatusNotFound, map[string]string{"error": "Claude is disabled"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.claude.Describe(ctx, r.FormValue("name"))
	if err != nil {
		log.Printf("claude: describe %q: %v", r.FormValue("name"), err)
		reply(http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("claude: describe %q: %s (%s), %d input + %d output tokens", r.FormValue("name"), res.PartNumber, res.Model, res.InputTokens, res.OutputTokens)
	reply(http.StatusOK, map[string]any{
		"part_number":   res.PartNumber,
		"description":   describe(res),
		"datasheet_url": res.DatasheetURL,
		"confidence":    res.Confidence,
		"model":         res.Model,
		"input_tokens":  res.InputTokens,
		"output_tokens": res.OutputTokens,
	})
}

func (s *Server) editComponent(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	err := parseForm(w, r)
	if err == nil {
		err = s.inv.UpdateComponent(id, r.FormValue("name"), r.FormValue("description"))
	}
	if err == nil {
		err = s.saveDatasheet(r, id)
	}
	back(w, r, componentURL(id), err)
}

// saveDatasheet stores the uploaded "datasheet" file, if any.
func (s *Server) saveDatasheet(r *http.Request, id int64) error {
	data, name, err := upload(r, "datasheet")
	if err != nil || data == nil {
		return err
	}
	return s.inv.SetDatasheet(id, name, data)
}

func (s *Server) deleteComponent(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := s.inv.DeleteComponent(id); err != nil {
		back(w, r, componentURL(id), err)
		return
	}
	back(w, r, "/inventory", nil)
}

func (s *Server) deleteDatasheet(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	back(w, r, componentURL(id), s.inv.RemoveDatasheet(id))
}

func (s *Server) createItem(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	err := parseForm(w, r)
	var photo image.Image
	if err == nil {
		photo, err = uploadImage(r, "photo")
	}
	if err == nil {
		_, _, err = s.inv.AddItem(inventory.Component{ID: id}, r.FormValue("marking"), photo)
	}
	back(w, r, componentURL(id), err)
}

func (s *Server) editItem(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	it, err := s.inv.Item(id)
	if err != nil {
		back(w, r, "/inventory/items", err)
		return
	}
	page := componentURL(it.ComponentID)
	if err := parseForm(w, r); err != nil {
		back(w, r, page, err)
		return
	}
	comp, _ := strconv.ParseInt(r.FormValue("component"), 10, 64)
	if comp == 0 {
		comp = it.ComponentID
	}
	marking := it.Marking
	if _, ok := r.Form["marking"]; ok {
		marking = r.FormValue("marking")
	}
	err = s.inv.UpdateItem(id, comp, marking)
	var photo image.Image
	if err == nil {
		photo, err = uploadImage(r, "photo")
	}
	if err == nil && photo != nil {
		err = s.inv.SetItemPhoto(id, photo)
	}
	back(w, r, page, err)
}

func (s *Server) deleteItem(w http.ResponseWriter, r *http.Request) {
	it, err := s.inv.Item(pathID(r))
	if err != nil {
		back(w, r, "/inventory/items", err)
		return
	}
	back(w, r, componentURL(it.ComponentID), s.inv.DeleteItem(it.ID))
}

// parseForm parses a form, which may carry file uploads up to the size of
// a datasheet.
func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, inventory.MaxDatasheet+1<<20)
	err := r.ParseMultipartForm(8 << 20)
	if errors.Is(err, http.ErrNotMultipart) {
		return r.ParseForm()
	}
	return err
}

// noFile reports whether FormFile found no file: none chosen, or a form
// that cannot carry files.
func noFile(err error) bool {
	return errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart)
}

// upload returns the contents and name of an uploaded file, or nil if the
// form has none.
func upload(r *http.Request, field string) ([]byte, string, error) {
	f, h, err := r.FormFile(field)
	if noFile(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err == nil && len(data) == 0 {
		return nil, "", nil
	}
	return data, h.Filename, err
}

// uploadImage decodes an uploaded JPEG or PNG, or returns nil if the form
// has none.
func uploadImage(r *http.Request, field string) (image.Image, error) {
	f, _, err := r.FormFile(field)
	if noFile(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("photo: %w", err)
	}
	return img, nil
}
