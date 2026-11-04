// Package favicon makes a profile's icon from its browser's own favicon
// cache: the Favicons database Chromium keeps in the profile, for pages on
// the profile's main site. Nothing is fetched; the browser already did.
// When the main site isn't set, it is read from the profile's History, as
// the registrable domain visited most.
//
// Both databases are written by the browser, so they are treated as
// hostile: read only once the browser has exited, through os.Root, from a
// private copy, with size and row caps. The image found is decoded,
// checked and drawn again as a 64×64 PNG; its own bytes are never kept.
package favicon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/net/publicsuffix"
	_ "modernc.org/sqlite" // the driver BrowserPier's own database uses
)

// FileName is the cached icon's name in a profile's folder.
const FileName = "icon.png"

// Caps on what is read from a profile.
const (
	maxFavicons = 32 << 20 // the Favicons database, copied
	maxHistory  = 64 << 20 // the History database, copied
	maxPages    = 20_000   // icon mappings, or history rows, looked at
	maxIcons    = 5        // icons tried for the site
	maxTries    = 8        // bitmaps decoded at most
)

// Result is what Read found.
type Result struct {
	// Site is the most visited site in the history, when Read was asked
	// to infer it; "" if there is no history to go by.
	Site string
	// PNG is the icon, 64×64; nil if the favicon cache has none for the site.
	PNG []byte
}

// Read finds the icon for site in chromeDir, a stopped browser's user data
// dir. With site "", it infers the site from the history first. scratch
// is an empty folder for the copies, which the caller removes.
func Read(ctx context.Context, chromeDir, site, scratch string) (Result, error) {
	var res Result
	root, err := os.OpenRoot(chromeDir)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil // never started
	}
	if err != nil {
		return res, err
	}
	defer func() { _ = root.Close() }()
	if site == "" {
		path, ok, err := copyDB(root, "Default/History", scratch, maxHistory)
		if err != nil {
			return res, fmt.Errorf("copying History: %w", err)
		}
		if ok {
			if res.Site, err = InferSite(ctx, path); err != nil {
				return res, fmt.Errorf("reading History: %w", err)
			}
		}
		site = res.Site
	}
	if site == "" {
		return res, nil
	}
	path, ok, err := copyDB(root, "Default/Favicons", scratch, maxFavicons)
	if err != nil {
		return res, fmt.Errorf("copying Favicons: %w", err)
	}
	if !ok {
		return res, nil
	}
	if res.PNG, err = Find(ctx, path, site); err != nil {
		return res, fmt.Errorf("reading Favicons: %w", err)
	}
	return res, nil
}

// copyDB copies the database at name in root, with its -journal or -wal
// if there is one, into dir. ok is false if there's no database.
func copyDB(root *os.Root, name, dir string, limit int64) (path string, ok bool, err error) {
	path = filepath.Join(dir, filepath.Base(name))
	if err := copyFile(root, name, path, limit); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, side := range []string{"-journal", "-wal"} {
		if err := copyFile(root, name+side, path+side, limit); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	return path, true, nil
}

// copyFile copies a regular file of at most limit bytes. os.Root keeps a
// link from leading out of the folder; O_NONBLOCK keeps a FIFO from
// hanging the open, and Stat then shows it isn't a file.
func copyFile(root *os.Root, name, dst string, limit int64) error {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a file", name)
	}
	if info.Size() > limit {
		return fmt.Errorf("%s is over %d MiB", name, limit>>20)
	}
	if info.Size() == 0 {
		return os.ErrNotExist // an empty journal: nothing to apply
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(f, limit)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// open opens a copied database. With no journal or WAL beside it, it's
// opened read-only and immutable; with one, read-write, so SQLite can
// apply it to the copy. Either way the schema is untrusted and no query
// writes.
func open(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "trusted_schema(0)")
	q.Add("_pragma", "cell_size_check(1)")
	_, errJ := os.Stat(path + "-journal")
	_, errW := os.Stat(path + "-wal")
	if errors.Is(errJ, os.ErrNotExist) && errors.Is(errW, os.ErrNotExist) {
		q.Set("mode", "ro")
		q.Set("immutable", "1")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	return d, nil
}

// InferSite returns the registrable domain (see Registrable) of the web
// pages visited most in the History database at path, or "" if none.
func InferSite(ctx context.Context, path string) (string, error) {
	d, err := open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.Close() }()
	rows, err := d.QueryContext(ctx, `SELECT url, visit_count FROM urls WHERE hidden = 0 AND visit_count > 0
		ORDER BY visit_count DESC LIMIT ?`, maxPages)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	visits := map[string]int64{}
	for rows.Next() {
		var raw string
		var n int64
		if err := rows.Scan(&raw, &n); err != nil {
			return "", err
		}
		if host := webHost(raw); host != "" {
			visits[Registrable(host)] += n
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	best, most := "", int64(0)
	for site, n := range visits {
		if n > most || (n == most && site < best) {
			best, most = site, n
		}
	}
	return best, nil
}

// Find returns the favicon for site from the Favicons database at path,
// as a sanitised PNG, or nil if it holds none that passes.
func Find(ctx context.Context, path, site string) ([]byte, error) {
	d, err := open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	icons, err := siteIcons(ctx, d, site)
	if err != nil {
		return nil, err
	}
	tries := 0
	for _, icon := range icons {
		bitmaps, err := iconBitmaps(ctx, d, icon)
		if err != nil {
			return nil, err
		}
		for _, id := range bitmaps {
			if tries++; tries > maxTries {
				return nil, nil
			}
			var blob []byte
			if err := d.QueryRowContext(ctx, `SELECT image_data FROM favicon_bitmaps WHERE id = ?`, id).Scan(&blob); err != nil {
				return nil, err
			}
			if png, err := Sanitize(blob); err == nil {
				return png, nil
			}
		}
	}
	return nil, nil
}

// siteIcons lists the icons of the site's pages, the icon of the most
// pages first: Chromium maps every page it showed to its icon.
func siteIcons(ctx context.Context, d *sql.DB, site string) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT page_url, icon_id FROM icon_mapping WHERE instr(page_url, ?) > 0 LIMIT ?`, site, maxPages)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type score struct {
		pages int
		exact bool // shown on the site's own host, not a subdomain
	}
	scores := map[int64]*score{}
	for rows.Next() {
		var raw string
		var icon int64
		if err := rows.Scan(&raw, &icon); err != nil {
			return nil, err
		}
		host := webHost(raw)
		if host != site && !strings.HasSuffix(host, "."+site) {
			continue
		}
		s := scores[icon]
		if s == nil {
			s = &score{}
			scores[icon] = s
		}
		s.pages++
		s.exact = s.exact || host == site
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	icons := make([]int64, 0, len(scores))
	for id := range scores {
		icons = append(icons, id)
	}
	sort.Slice(icons, func(i, j int) bool {
		a, b := scores[icons[i]], scores[icons[j]]
		if a.pages != b.pages {
			return a.pages > b.pages
		}
		if a.exact != b.exact {
			return a.exact
		}
		return icons[i] > icons[j] // the newer
	})
	if len(icons) > maxIcons {
		icons = icons[:maxIcons]
	}
	return icons, nil
}

// iconBitmaps lists an icon's bitmaps in the order to try them: the
// largest up to Size, then the smallest above it, then those of unknown
// size. Bitmaps too large to take are left out.
func iconBitmaps(ctx context.Context, d *sql.DB, icon int64) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, width, height, length(image_data) FROM favicon_bitmaps WHERE icon_id = ? LIMIT 64`, icon)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type bitmap struct {
		id, side int64
	}
	var list []bitmap
	for rows.Next() {
		var id, w, h int64
		var n sql.NullInt64
		if err := rows.Scan(&id, &w, &h, &n); err != nil {
			return nil, err
		}
		if !n.Valid || n.Int64 == 0 || n.Int64 > MaxBytes {
			continue
		}
		list = append(list, bitmap{id, max(w, h)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rank := func(b bitmap) (int, int64) {
		switch {
		case b.side <= 0:
			return 2, 0
		case b.side <= Size:
			return 0, -b.side
		default:
			return 1, b.side
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		gi, ki := rank(list[i])
		gj, kj := rank(list[j])
		if gi != gj {
			return gi < gj
		}
		return ki < kj
	})
	ids := make([]int64, len(list))
	for i, b := range list {
		ids[i] = b.id
	}
	return ids, nil
}

// webHost is the lowercased host of an http or https URL, without its
// port; "" for anything else.
func webHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// Registrable is host's registrable domain (accounts.google.com →
// google.com), per the Public Suffix List. An IP address, a single label
// such as localhost, or a public suffix itself stays as it is.
func Registrable(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}
