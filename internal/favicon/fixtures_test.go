package favicon

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The fixtures in testdata/chrome/Default are a Favicons and a History
// database with Chromium's schema (components/favicon/core/favicon_database.cc
// and components/history/core/browser/url_database.cc, version 8 and 66).
// Regenerate them with:
//
//	BROWSERPIER_WRITE_FIXTURES=1 go test -run TestWriteFixtures ./internal/favicon
const fixtureDir = "testdata/chrome"

var faviconsSchema = []string{
	`CREATE TABLE meta(key LONGVARCHAR NOT NULL UNIQUE PRIMARY KEY, value LONGVARCHAR)`,
	`CREATE TABLE icon_mapping(id INTEGER PRIMARY KEY,page_url LONGVARCHAR NOT NULL,icon_id INTEGER)`,
	`CREATE TABLE favicons(id INTEGER PRIMARY KEY,url LONGVARCHAR NOT NULL,icon_type INTEGER DEFAULT 1)`,
	`CREATE TABLE favicon_bitmaps(id INTEGER PRIMARY KEY,icon_id INTEGER NOT NULL,last_updated INTEGER DEFAULT 0,image_data BLOB,width INTEGER DEFAULT 0,height INTEGER DEFAULT 0,last_requested INTEGER DEFAULT 0)`,
	`CREATE INDEX icon_mapping_page_url_idx ON icon_mapping(page_url)`,
	`CREATE INDEX icon_mapping_icon_id_idx ON icon_mapping(icon_id)`,
	`CREATE INDEX favicons_url ON favicons(url)`,
	`CREATE INDEX favicon_bitmaps_icon_id ON favicon_bitmaps(icon_id)`,
	`INSERT INTO meta VALUES ('mmap_status', '-1'), ('version', '8'), ('last_compatible_version', '8')`,
}

var historySchema = []string{
	`CREATE TABLE meta(key LONGVARCHAR NOT NULL UNIQUE PRIMARY KEY, value LONGVARCHAR)`,
	`CREATE TABLE urls(id INTEGER PRIMARY KEY AUTOINCREMENT,url LONGVARCHAR,title LONGVARCHAR,visit_count INTEGER DEFAULT 0 NOT NULL,typed_count INTEGER DEFAULT 0 NOT NULL,last_visit_time INTEGER NOT NULL,hidden INTEGER DEFAULT 0 NOT NULL)`,
	`CREATE INDEX urls_url_index ON urls (url)`,
	`CREATE TABLE visits(id INTEGER PRIMARY KEY AUTOINCREMENT,url INTEGER NOT NULL,visit_time INTEGER NOT NULL,from_visit INTEGER,external_referrer_url TEXT,transition INTEGER DEFAULT 0 NOT NULL,segment_id INTEGER,visit_duration INTEGER DEFAULT 0 NOT NULL,incremented_omnibox_typed_score BOOLEAN DEFAULT FALSE NOT NULL,opener_visit INTEGER,originator_cache_guid TEXT,originator_visit_id INTEGER,originator_from_visit INTEGER,originator_opener_visit INTEGER,is_known_to_sync BOOLEAN DEFAULT FALSE NOT NULL,consider_for_ntp_most_visited BOOLEAN DEFAULT FALSE NOT NULL,visited_link_id INTEGER DEFAULT 0 NOT NULL,app_id TEXT)`,
	`CREATE INDEX visits_url_index ON visits (url)`,
	`INSERT INTO meta VALUES ('mmap_status', '-1'), ('version', '66'), ('last_compatible_version', '16')`,
}

// Colours of the fixture's bitmaps, to tell which one was taken.
var (
	blue   = color.NRGBA{0, 0, 255, 255}
	green  = color.NRGBA{0, 128, 0, 255}
	purple = color.NRGBA{128, 0, 128, 255}
	grey   = color.NRGBA{100, 100, 100, 255}
	yellow = color.NRGBA{255, 255, 0, 255}
	cyan   = color.NRGBA{0, 255, 255, 255}
	navy   = color.NRGBA{0, 0, 128, 255}
	white  = color.NRGBA{255, 255, 255, 255}
	teal   = color.NRGBA{0, 128, 128, 255}
)

type fixtureIcon struct {
	pages   []string
	bitmaps [][]byte
}

// fixtureIcons are the favicon cache's icons, by id.
func fixtureIcons(t testing.TB) map[int]fixtureIcon {
	return map[int]fixtureIcon{
		// The main site: the 32px one is the largest up to 64.
		1: {pages: []string{"https://mail.example.com/mail/u/0/", "https://mail.example.com/mail/u/0/#inbox"},
			bitmaps: [][]byte{solidPNG(t, 16, 16, blue), solidPNG(t, 32, 32, green), solidPNG(t, 128, 128, purple)}},
		2: {pages: []string{"https://accounts.example.com/signin"}, bitmaps: [][]byte{solidPNG(t, 16, 16, grey)}},
		// Only larger than 64: the smallest above it.
		3: {pages: []string{"https://www.big.test/"}, bitmaps: [][]byte{solidPNG(t, 192, 192, cyan), solidPNG(t, 128, 128, yellow)}},
		4: {pages: []string{"https://ico.test/"}, bitmaps: [][]byte{icoFile(t, bmp32(16, 16, white), solidPNG(t, 32, 32, navy))}},
		5: {pages: []string{"https://bmp.test/"}, bitmaps: [][]byte{icoFile(t, bmp32(16, 16, teal))}},
		6: {pages: []string{"https://corrupt.test/"}, bitmaps: [][]byte{append(append([]byte{}, pngMagic...), "not really a png"...)}},
		7: {pages: []string{"https://huge.test/"}, bitmaps: [][]byte{solidPNG(t, 300, 300, green)}},
		8: {pages: []string{"https://wide.test/"}, bitmaps: [][]byte{solidPNG(t, 64, 32, blue)}},
	}
}

// fixtureHistory: example.com is the most visited registrable domain
// (65), ahead of other.test (50); hidden rows and non-web URLs don't count.
var fixtureHistory = []struct {
	url    string
	visits int
	hidden bool
}{
	{"https://mail.example.com/mail/u/0/", 60, false},
	{"https://accounts.example.com/signin", 5, false},
	{"https://www.other.test/", 30, false},
	{"https://news.other.test/a", 20, false},
	{"https://ads.tracker.test/frame", 1000, true},
	{"chrome://settings/", 500, false},
	{"file:///tmp/notes.html", 300, false},
}

func TestWriteFixtures(t *testing.T) {
	if os.Getenv("BROWSERPIER_WRITE_FIXTURES") == "" {
		t.Skip("set BROWSERPIER_WRITE_FIXTURES=1 to regenerate the fixtures")
	}
	dir := filepath.Join(fixtureDir, "Default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Favicons", "History"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	writeFavicons(t, filepath.Join(dir, "Favicons"), fixtureIcons(t), "")
	writeHistory(t, filepath.Join(dir, "History"))
}

// writeFavicons writes a Favicons database at path. journal is the journal
// mode, "" for SQLite's default; with "wal" the database is left open, as
// a running browser would, until the test ends.
func writeFavicons(t testing.TB, path string, icons map[int]fixtureIcon, journal string) {
	t.Helper()
	d := create(t, path, faviconsSchema, journal)
	mapping, bitmap := 1, 1
	for _, id := range slices.Sorted(maps.Keys(icons)) {
		icon := icons[id]
		exec(t, d, `INSERT INTO favicons (id, url, icon_type) VALUES (?, ?, 1)`, id, icon.pages[0]+"favicon.ico")
		for _, page := range icon.pages {
			exec(t, d, `INSERT INTO icon_mapping (id, page_url, icon_id) VALUES (?, ?, ?)`, mapping, page, id)
			mapping++
		}
		for _, b := range icon.bitmaps {
			w, h := 0, 0
			if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err == nil {
				w, h = cfg.Width, cfg.Height
			} else if bytes.HasPrefix(b, icoMagic) {
				w, h = 16, 16 // as Chromium records an icon it decoded
			}
			exec(t, d, `INSERT INTO favicon_bitmaps (id, icon_id, last_updated, image_data, width, height) VALUES (?, ?, ?, ?, ?, ?)`,
				bitmap, id, 13_380_000_000_000_000, b, w, h)
			bitmap++
		}
	}
	if journal != "wal" {
		_ = d.Close()
	}
}

func writeHistory(t testing.TB, path string) {
	t.Helper()
	d := create(t, path, historySchema, "")
	for _, u := range fixtureHistory {
		exec(t, d, `INSERT INTO urls (url, title, visit_count, typed_count, last_visit_time, hidden) VALUES (?, '', ?, 0, 13380000000000000, ?)`,
			u.url, u.visits, u.hidden)
	}
	_ = d.Close()
}

func create(t testing.TB, path string, schema []string, journal string) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	if journal != "" {
		exec(t, d, "PRAGMA journal_mode = "+journal)
		exec(t, d, "PRAGMA wal_autocheckpoint = 0")
		t.Cleanup(func() { _ = d.Close() })
	}
	for _, q := range schema {
		exec(t, d, q)
	}
	return d
}

func exec(t testing.TB, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func solidPNG(t testing.TB, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// bmp32 is an ICO's 32-bit BMP image: the header with twice the height,
// BGRA rows from the bottom up, and an all-opaque mask.
func bmp32(w, h int, c color.NRGBA) []byte {
	var b bytes.Buffer
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr, 40)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(w))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(2*h))
	binary.LittleEndian.PutUint16(hdr[12:], 1)
	binary.LittleEndian.PutUint16(hdr[14:], 32)
	b.Write(hdr)
	for range w * h {
		b.Write([]byte{c.B, c.G, c.R, c.A})
	}
	b.Write(make([]byte, (((w+7)/8+3)&^3)*h))
	return b.Bytes()
}

// icoFile puts images (PNG or BMP) in an ICO, each declared 16 or 32 px
// as its size says.
func icoFile(t testing.TB, images ...[]byte) []byte {
	t.Helper()
	var dir, data bytes.Buffer
	dir.Write([]byte{0, 0, 1, 0})
	_ = binary.Write(&dir, binary.LittleEndian, uint16(len(images)))
	off := 6 + 16*len(images)
	for _, img := range images {
		side := 16
		if cfg, err := png.DecodeConfig(bytes.NewReader(img)); err == nil {
			side = cfg.Width
		}
		e := make([]byte, 16)
		e[0], e[1] = byte(side), byte(side)
		binary.LittleEndian.PutUint16(e[4:], 1)
		binary.LittleEndian.PutUint16(e[6:], 32)
		binary.LittleEndian.PutUint32(e[8:], uint32(len(img)))
		binary.LittleEndian.PutUint32(e[12:], uint32(off+data.Len()))
		dir.Write(e)
		data.Write(img)
	}
	return append(dir.Bytes(), data.Bytes()...)
}
