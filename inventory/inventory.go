// Package inventory is the component database: which components (part
// models) are known and which items (physical parts) are in stock.
//
// A database is a directory:
//
//	inventory.db   SQLite database, tables components and items
//	photos/        one cropped photo per item (item<ID>.jpg)
//	datasheets/    datasheets of components (any file name)
//
// Terminology: a component is a part model with a name (the part number),
// a description and a datasheet; an item is one physical instance of a
// component, with the marking read from it and its photo. A component can
// have any number of items.
package inventory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/mattn/go-sqlite3"
)

// File and directory names inside a database directory.
const (
	DBFile        = "inventory.db"
	PhotosDir     = "photos"
	DatasheetsDir = "datasheets"
)

// MaxDatasheet is the largest datasheet accepted, in bytes.
const MaxDatasheet = 64 << 20

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("a component with that name already exists")
	ErrNoName    = errors.New("the component name is empty")
)

const schema = `
CREATE TABLE IF NOT EXISTS components (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
	description TEXT NOT NULL DEFAULT '',
	datasheet   TEXT NOT NULL DEFAULT ''   -- file name in datasheets/, '' if none
);
CREATE TABLE IF NOT EXISTS items (
	id           INTEGER PRIMARY KEY,
	component_id INTEGER NOT NULL REFERENCES components(id),
	marking      TEXT NOT NULL DEFAULT '', -- lines separated by "\n"
	photo        TEXT NOT NULL DEFAULT ''  -- file name in photos/, '' if none
);
CREATE INDEX IF NOT EXISTS items_component ON items(component_id);
`

// Component is a part model.
type Component struct {
	ID          int64
	Name        string // part number, unique regardless of case
	Description string
	Datasheet   string // file name in the datasheets directory, "" if none

	// Filled in by Components and Component.
	Items int    // number of items
	Photo string // photo of its newest item with one, "" if none
}

// Item is one physical part.
type Item struct {
	ID          int64
	ComponentID int64
	Marking     string // text on the part, lines separated by "\n"
	Photo       string // file name in the photos directory, "" if none

	ComponentName string // filled in by queries
}

// DB is an open database directory. It is safe for concurrent use.
type DB struct {
	dir string
	sql *sql.DB
	gen atomic.Uint64
}

// Open opens the database in dir, creating the directory, its
// subdirectories and the tables as needed.
func Open(dir string) (*DB, error) {
	for _, d := range []string{dir, filepath.Join(dir, PhotosDir), filepath.Join(dir, DatasheetsDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, DBFile)+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // a small local database: no lock contention
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("inventory %s: %w", dir, err)
	}
	return &DB{dir: dir, sql: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Dir returns the database directory.
func (d *DB) Dir() string { return d.dir }

// PhotosDir and DatasheetsDir return the file directories.
func (d *DB) PhotosDir() string     { return filepath.Join(d.dir, PhotosDir) }
func (d *DB) DatasheetsDir() string { return filepath.Join(d.dir, DatasheetsDir) }

// Generation increases with every change, so callers can cache lookups.
func (d *DB) Generation() uint64 { return d.gen.Load() }

func (d *DB) changed() { d.gen.Add(1) }

const componentColumns = `c.id, c.name, c.description, c.datasheet,
	(SELECT COUNT(*) FROM items WHERE component_id = c.id),
	COALESCE((SELECT photo FROM items WHERE component_id = c.id AND photo <> '' ORDER BY id DESC LIMIT 1), '')`

func scanComponent(row interface{ Scan(...any) error }) (Component, error) {
	var c Component
	err := row.Scan(&c.ID, &c.Name, &c.Description, &c.Datasheet, &c.Items, &c.Photo)
	return c, err
}

// Components lists the components whose name or description contains
// query (all of them if query is empty), by name.
func (d *DB) Components(query string) ([]Component, error) {
	rows, err := d.sql.Query(`SELECT `+componentColumns+` FROM components c
		WHERE ?1 = '' OR instr(lower(c.name), lower(?1)) > 0 OR instr(lower(c.description), lower(?1)) > 0
		ORDER BY c.name`, strings.TrimSpace(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Component
	for rows.Next() {
		c, err := scanComponent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Component returns one component, or ErrNotFound.
func (d *DB) Component(id int64) (*Component, error) {
	c, err := scanComponent(d.sql.QueryRow(`SELECT `+componentColumns+` FROM components c WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// ComponentByName returns the component with that name, ignoring case and
// surrounding spaces, or ErrNotFound.
func (d *DB) ComponentByName(name string) (*Component, error) {
	c, err := scanComponent(d.sql.QueryRow(`SELECT `+componentColumns+` FROM components c WHERE c.name = ?`, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// AddComponent adds a component (its ID and Datasheet are ignored) and
// returns its ID.
func (d *DB) AddComponent(c Component) (int64, error) {
	id, err := insertComponent(d.sql, c)
	if err == nil {
		d.changed()
	}
	return id, err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertComponent(db execer, c Component) (int64, error) {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return 0, ErrNoName
	}
	res, err := db.Exec(`INSERT INTO components (name, description) VALUES (?, ?)`, name, strings.TrimSpace(c.Description))
	if err != nil {
		return 0, sqlError(err)
	}
	return res.LastInsertId()
}

// UpdateComponent changes a component's name and description.
func (d *DB) UpdateComponent(id int64, name, description string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrNoName
	}
	res, err := d.sql.Exec(`UPDATE components SET name = ?, description = ? WHERE id = ?`, name, strings.TrimSpace(description), id)
	return d.affected(res, sqlError(err))
}

// DeleteComponent deletes a component, its items, their photos and its
// datasheet.
func (d *DB) DeleteComponent(id int64) error {
	c, err := d.Component(id)
	if err != nil {
		return err
	}
	items, err := d.Items(id, "")
	if err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM items WHERE component_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM components WHERE id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.changed()
	for _, it := range items {
		d.removeFile(PhotosDir, it.Photo)
	}
	d.removeFile(DatasheetsDir, c.Datasheet)
	return nil
}

// Items lists items, newest first: those of one component, or all with
// componentID 0. A non-empty query keeps the items whose marking or
// component name contains it.
func (d *DB) Items(componentID int64, query string) ([]Item, error) {
	rows, err := d.sql.Query(`SELECT i.id, i.component_id, i.marking, i.photo, c.name
		FROM items i JOIN components c ON c.id = i.component_id
		WHERE (?1 = 0 OR i.component_id = ?1)
		  AND (?2 = '' OR instr(lower(i.marking), lower(?2)) > 0 OR instr(lower(c.name), lower(?2)) > 0)
		ORDER BY i.id DESC`, componentID, strings.TrimSpace(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ComponentID, &it.Marking, &it.Photo, &it.ComponentName); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Item returns one item, or ErrNotFound.
func (d *DB) Item(id int64) (*Item, error) {
	var it Item
	err := d.sql.QueryRow(`SELECT i.id, i.component_id, i.marking, i.photo, c.name
		FROM items i JOIN components c ON c.id = i.component_id WHERE i.id = ?`, id).
		Scan(&it.ID, &it.ComponentID, &it.Marking, &it.Photo, &it.ComponentName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &it, err
}

// AddItem adds an item of component c with its marking and photo (nil for
// none; it is stored as JPEG). If c.ID is 0, the component named c.Name is
// used, and created first if there is none; created reports that. Both
// happen in one transaction.
func (d *DB) AddItem(c Component, marking string, photo image.Image) (it *Item, created bool, err error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	switch {
	case c.ID != 0:
		err = tx.QueryRow(`SELECT name FROM components WHERE id = ?`, c.ID).Scan(&c.Name)
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
	default:
		err = tx.QueryRow(`SELECT id, name FROM components WHERE name = ?`, strings.TrimSpace(c.Name)).Scan(&c.ID, &c.Name)
		if errors.Is(err, sql.ErrNoRows) {
			created = true
			c.ID, err = insertComponent(tx, c)
		}
	}
	if err != nil {
		return nil, false, err
	}
	it = &Item{ComponentID: c.ID, ComponentName: c.Name, Marking: cleanMarking(marking)}
	res, err := tx.Exec(`INSERT INTO items (component_id, marking) VALUES (?, ?)`, c.ID, it.Marking)
	if err != nil {
		return nil, false, err
	}
	if it.ID, err = res.LastInsertId(); err != nil {
		return nil, false, err
	}
	if photo != nil {
		if it.Photo, err = d.writePhoto(it.ID, photo); err != nil {
			return nil, false, err
		}
		if _, err := tx.Exec(`UPDATE items SET photo = ? WHERE id = ?`, it.Photo, it.ID); err != nil {
			d.removeFile(PhotosDir, it.Photo)
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		d.removeFile(PhotosDir, it.Photo)
		return nil, false, err
	}
	d.changed()
	return it, created, nil
}

// UpdateItem changes an item's marking and component.
func (d *DB) UpdateItem(id, componentID int64, marking string) error {
	res, err := d.sql.Exec(`UPDATE items SET component_id = ?, marking = ? WHERE id = ?`, componentID, cleanMarking(marking), id)
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		err = fmt.Errorf("component %d: %w", componentID, ErrNotFound)
	}
	return d.affected(res, err)
}

// SetItemPhoto replaces an item's photo.
func (d *DB) SetItemPhoto(id int64, photo image.Image) error {
	it, err := d.Item(id)
	if err != nil {
		return err
	}
	name, err := d.writePhoto(id, photo) // same name: overwrites the old one
	if err != nil {
		return err
	}
	if it.Photo != "" && it.Photo != name {
		d.removeFile(PhotosDir, it.Photo)
	}
	_, err = d.sql.Exec(`UPDATE items SET photo = ? WHERE id = ?`, name, id)
	if err == nil {
		d.changed()
	}
	return err
}

// DeleteItem deletes an item and its photo.
func (d *DB) DeleteItem(id int64) error {
	it, err := d.Item(id)
	if err != nil {
		return err
	}
	res, err := d.sql.Exec(`DELETE FROM items WHERE id = ?`, id)
	if err = d.affected(res, err); err == nil {
		d.removeFile(PhotosDir, it.Photo)
	}
	return err
}

// SetDatasheet stores data as the component's datasheet, replacing any
// previous one. The file is named after the component; filename (the
// uploaded file's name) only gives the extension.
func (d *DB) SetDatasheet(id int64, filename string, data []byte) error {
	c, err := d.Component(id)
	if err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || len(ext) > 8 {
		ext = ".pdf"
	}
	name := safeName(c.Name) + ext
	if name != c.Datasheet && exists(filepath.Join(d.DatasheetsDir(), name)) {
		name = fmt.Sprintf("%s-%d%s", safeName(c.Name), c.ID, ext) // another component's file
	}
	if err := writeFile(filepath.Join(d.DatasheetsDir(), name), data); err != nil {
		return err
	}
	if _, err := d.sql.Exec(`UPDATE components SET datasheet = ? WHERE id = ?`, name, id); err != nil {
		return err
	}
	if c.Datasheet != "" && c.Datasheet != name {
		d.removeFile(DatasheetsDir, c.Datasheet)
	}
	d.changed()
	return nil
}

// RemoveDatasheet deletes the component's datasheet.
func (d *DB) RemoveDatasheet(id int64) error {
	c, err := d.Component(id)
	if err != nil {
		return err
	}
	if _, err := d.sql.Exec(`UPDATE components SET datasheet = '' WHERE id = ?`, id); err != nil {
		return err
	}
	d.removeFile(DatasheetsDir, c.Datasheet)
	d.changed()
	return nil
}

// FetchDatasheet downloads a PDF from url and stores it as the component's
// datasheet, unless the component got one meanwhile.
func (d *DB) FetchDatasheet(ctx context.Context, id int64, url string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("datasheet %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxDatasheet+1))
	switch {
	case err != nil:
		return err
	case len(data) > MaxDatasheet:
		return fmt.Errorf("datasheet %s: larger than %d MB", url, MaxDatasheet>>20)
	case !bytes.HasPrefix(data, []byte("%PDF-")):
		return fmt.Errorf("datasheet %s: not a PDF (%s)", url, resp.Header.Get("Content-Type"))
	}
	c, err := d.Component(id)
	if err != nil {
		return err
	}
	if c.Datasheet != "" {
		return nil
	}
	return d.SetDatasheet(id, ".pdf", data)
}

func (d *DB) writePhoto(id int64, img image.Image) (string, error) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		return "", err
	}
	name := fmt.Sprintf("item%d.jpg", id)
	return name, writeFile(filepath.Join(d.PhotosDir(), name), b.Bytes())
}

func (d *DB) removeFile(sub, name string) {
	if name != "" {
		os.Remove(filepath.Join(d.dir, sub, filepath.Base(name)))
	}
}

// affected turns "no row changed" into ErrNotFound and counts a change.
func (d *DB) affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	d.changed()
	return nil
}

// writeFile writes atomically: a reader never sees a partial file.
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sqlError(err error) error {
	var se sqlite3.Error
	if errors.As(err, &se) && se.ExtendedCode == sqlite3.ErrConstraintUnique {
		return ErrDuplicate
	}
	return err
}

// cleanMarking trims every line and drops empty ones. Claude separates
// lines with " / "; they are stored one per line.
func cleanMarking(s string) string {
	s = strings.ReplaceAll(s, " / ", "\n")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// safeName makes a component name usable as a file name.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.+", r)) {
			return r
		}
		return '_'
	}, strings.TrimSpace(s))
	s = strings.TrimLeft(s, ".")
	if s == "" {
		return "datasheet"
	}
	return s
}
