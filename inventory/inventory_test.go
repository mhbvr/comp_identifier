package inventory

import (
	"context"
	"errors"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddComponent(Component{Name: "LM35DZ"}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for _, p := range []string{DBFile, PhotosDir, DatasheetsDir} {
		if !exists(filepath.Join(dir, p)) {
			t.Errorf("%s missing", p)
		}
	}
	// Reopening keeps the data.
	d, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if c, err := d.ComponentByName(" lm35dz "); err != nil || c.Name != "LM35DZ" {
		t.Errorf("ComponentByName = %+v, %v", c, err)
	}
}

func TestComponentsAndItems(t *testing.T) {
	d := open(t)
	id, err := d.AddComponent(Component{Name: "TL072IP", Description: "dual op-amp, DIP-8"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddComponent(Component{Name: "tl072ip"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := d.AddComponent(Component{Name: "  "}); !errors.Is(err, ErrNoName) {
		t.Errorf("empty name: %v", err)
	}
	gen := d.Generation()

	photo := image.NewRGBA(image.Rect(0, 0, 40, 30))
	it, created, err := d.AddItem(Component{ID: id}, "31AE9XM / TL072IP ", photo)
	if err != nil || created {
		t.Fatalf("AddItem: %v, created %v", err, created)
	}
	if it.Marking != "31AE9XM\nTL072IP" || it.Photo == "" || it.ComponentName != "TL072IP" {
		t.Errorf("item %+v", it)
	}
	if !exists(filepath.Join(d.PhotosDir(), it.Photo)) {
		t.Error("photo not written")
	}
	if d.Generation() == gen {
		t.Error("generation unchanged")
	}

	// By name: existing component (any case), then a new one.
	if _, created, err := d.AddItem(Component{Name: "TL072ip"}, "TL072IP", nil); err != nil || created {
		t.Errorf("AddItem by existing name: %v, created %v", err, created)
	}
	it2, created, err := d.AddItem(Component{Name: "NE555P", Description: "timer"}, "NE555P", nil)
	if err != nil || !created {
		t.Fatalf("AddItem with new component: %v, created %v", err, created)
	}
	if _, _, err := d.AddItem(Component{ID: 999}, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddItem to unknown component: %v", err)
	}

	comps, err := d.Components("")
	if err != nil || len(comps) != 2 || comps[0].Name != "NE555P" || comps[1].Items != 2 || comps[1].Photo != it.Photo {
		t.Errorf("Components = %+v, %v", comps, err)
	}
	if comps, _ := d.Components("op-AMP"); len(comps) != 1 {
		t.Errorf("search by description: %+v", comps)
	}
	if items, _ := d.Items(0, "31ae"); len(items) != 1 || items[0].ID != it.ID {
		t.Errorf("search items by marking: %+v", items)
	}
	if items, _ := d.Items(id, ""); len(items) != 2 {
		t.Errorf("items of component: %+v", items)
	}

	// Move an item, then delete a component with its items and files.
	if err := d.UpdateItem(it.ID, it2.ComponentID, "NE555P\n"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateItem(it.ID, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("move to unknown component: %v", err)
	}
	if err := d.UpdateComponent(id, "NE555P", ""); !errors.Is(err, ErrDuplicate) {
		t.Errorf("rename to an existing name: %v", err)
	}
	if err := d.SetDatasheet(it2.ComponentID, "x.PDF", []byte("%PDF-1.4")); err != nil {
		t.Fatal(err)
	}
	c, _ := d.Component(it2.ComponentID)
	if c.Datasheet != "NE555P.pdf" {
		t.Errorf("datasheet name %q", c.Datasheet)
	}
	if err := d.DeleteComponent(it2.ComponentID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(d.PhotosDir(), it.Photo), filepath.Join(d.DatasheetsDir(), "NE555P.pdf")} {
		if exists(p) {
			t.Errorf("%s not deleted", p)
		}
	}
	if _, err := d.Item(it.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("item of deleted component: %v", err)
	}
	if err := d.DeleteItem(12345); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteItem unknown: %v", err)
	}
}

func TestDatasheets(t *testing.T) {
	d := open(t)
	a, _ := d.AddComponent(Component{Name: "A/B"})
	b, _ := d.AddComponent(Component{Name: "A_B"})
	if err := d.SetDatasheet(a, "ds.pdf", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDatasheet(b, "ds.pdf", []byte("two")); err != nil {
		t.Fatal(err)
	}
	ca, _ := d.Component(a)
	cb, _ := d.Component(b)
	if ca.Datasheet != "A_B.pdf" || cb.Datasheet == ca.Datasheet {
		t.Errorf("names %q %q", ca.Datasheet, cb.Datasheet)
	}
	// Replacing with another extension removes the old file.
	if err := d.SetDatasheet(a, "ds.txt", []byte("three")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(d.DatasheetsDir(), "A_B.pdf")) {
		t.Error("old datasheet kept")
	}
	if err := d.RemoveDatasheet(a); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(d.DatasheetsDir(), "A_B.txt")) {
		t.Error("datasheet not removed")
	}
}

func TestFetchDatasheet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ds.pdf":
			io.WriteString(w, "%PDF-1.7 datasheet")
		case "/page":
			io.WriteString(w, "<html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := open(t)
	id, _ := d.AddComponent(Component{Name: "LM35DZ"})
	ctx := context.Background()
	if err := d.FetchDatasheet(ctx, id, srv.URL+"/page"); err == nil {
		t.Error("HTML page accepted as datasheet")
	}
	if err := d.FetchDatasheet(ctx, id, srv.URL+"/missing"); err == nil {
		t.Error("404 accepted")
	}
	if err := d.FetchDatasheet(ctx, id, srv.URL+"/ds.pdf"); err != nil {
		t.Fatal(err)
	}
	c, _ := d.Component(id)
	data, err := os.ReadFile(filepath.Join(d.DatasheetsDir(), c.Datasheet))
	if err != nil || string(data) != "%PDF-1.7 datasheet" {
		t.Errorf("datasheet %q: %q, %v", c.Datasheet, data, err)
	}
}

func TestMatch(t *testing.T) {
	d := open(t)
	add := func(name string, markings ...string) {
		for _, m := range markings {
			if _, _, err := d.AddItem(Component{Name: name}, m, nil); err != nil {
				t.Fatal(err)
			}
		}
		if len(markings) == 0 {
			d.AddComponent(Component{Name: name})
		}
	}
	add("TLC555IP")
	add("TLC555")
	add("380N")
	add("LM35DZ/NOPB", "LM35DZ\n3AT4")
	add("CD4026BE", "18ADETM E4\nCD4026BE")

	cases := []struct {
		text string
		want []string // component names, best first
	}{
		{"TI 3AK\nTLC555IP", []string{"TLC555IP", "TLC555"}},
		{"TLC555CP", []string{"TLC555", "TLC555IP"}}, // exact short name beats a one-edit long one
		{"380N", []string{"380N"}},
		{"38ON", nil},                             // no edits forgiven in short names
		{"LM35DZ\n2BX1", []string{"LM35DZ/NOPB"}}, // through an item's marking line
		{"18ADETM E4\nCD4O26BE", []string{"CD4026BE"}},
		{"", nil},
		{"XYZ", nil},
	}
	for _, c := range cases {
		ms, err := d.Match(c.text)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range ms {
			got = append(got, m.Name)
		}
		if len(got) != len(c.want) {
			t.Errorf("Match(%q) = %v, want %v", c.text, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Match(%q) = %v, want %v", c.text, got, c.want)
				break
			}
		}
	}
	ms, _ := d.Match("18ADETM E4\nCD4026BE")
	if len(ms) != 1 || ms[0].By != "marking" || !ms[0].Exact() {
		t.Errorf("same marking as an item: %+v", ms)
	}
}

func TestWriteMarkdown(t *testing.T) {
	var b strings.Builder
	err := WriteMarkdown(&b, []Component{
		{ID: 7, Name: "NE555P", Description: "timer | DIP-8\nprecision", Items: 3, Datasheet: "NE555P.pdf"},
		{ID: 2, Name: "TL072IP", Description: `dual op-amp, 18 V \ DIP-8`},
		{ID: 3, Name: "380N"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Components\n\n3 components.\n\n" +
		"| ID | Name | Description |\n|---:|---|---|\n" +
		"| 2 | TL072IP | dual op-amp, 18 V \\\\ DIP-8 |\n" +
		"| 3 | 380N |  |\n" +
		"| 7 | NE555P | timer \\| DIP-8 precision |\n"
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	WriteMarkdown(&b, nil)
	if b.String() != "# Components\n\n0 components.\n\n" {
		t.Errorf("empty: %q", b.String())
	}
}
