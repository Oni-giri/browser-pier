// Package archive exports a profile's folder as a passphrase-encrypted
// archive and imports one into a new profile. An archive holds live
// session cookies, so it is a secret: it is encrypted with age (a
// passphrase, scrypt), which `age -d` can also open.
//
// Inside: manifest.json first, then a gzipped tar of the profile's
// chrome/ (Chromium's user data) and files/ folders, regular files and
// folders only. run/ (the control socket, and briefly proxy credentials)
// and Chromium's caches stay out. The profile's cached icon travels in the
// manifest, so archives stay readable by versions from before icons.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/Oni-giri/browser-pier/internal/favicon"
)

// Format is the archive format's version, in the manifest.
const Format = 1

// Manifest says what an archive holds.
type Manifest struct {
	Format     int       `json:"format"`
	Version    string    `json:"browserpier_version"`
	ExportedAt time.Time `json:"exported_at"`
	Profile    Profile   `json:"profile"`
}

// Profile is a profile's settings as an archive carries them. The proxy
// keeps its password: the archive is a secret anyway.
type Profile struct {
	Name            string `json:"name"`
	DefaultMode     string `json:"default_mode"`
	AutoStopMinutes int    `json:"auto_stop_minutes"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	Proxy           string `json:"proxy,omitempty"`
	// StickyToken is what {sticky} becomes in the proxy (PROJ-153), so
	// the profile keeps its exit on another BrowserPier. Archives made
	// before it have none: the profile gets a new one when it needs it.
	StickyToken   string `json:"sticky_token,omitempty"`
	HandoffPolicy string `json:"handoff_policy"`
	TimeZone      string `json:"time_zone,omitempty"`
	Locale        string `json:"locale,omitempty"`
	LocalNetwork  bool   `json:"local_network,omitempty"`
	// BlockAds is absent from archives made before PROJ-119, whose
	// profiles didn't block: they import with it off.
	BlockAds     bool   `json:"block_ads,omitempty"`
	Site         string `json:"site,omitempty"`
	SiteInferred string `json:"site_inferred,omitempty"`
	IconGlyph    string `json:"icon_glyph,omitempty"`
	IconColor    string `json:"icon_color,omitempty"`
	// Icon is the cached icon (favicon.FileName), a PNG. Export reads it
	// from the folder and Import writes it back, drawn again: Export's
	// caller needn't set it.
	Icon []byte `json:"icon_png,omitempty"`
}

// MinPassphrase and MaxPassphrase bound an export's passphrase.
const MinPassphrase, MaxPassphrase = 10, 1024

// folders are what an archive carries from a profile's folder.
var folders = []string{"chrome", "files"}

// skipped are names left out wherever they are: caches Chromium rebuilds,
// its single-instance locks (links and sockets, skipped anyway), and
// BrowserPier's own mount markers.
var skipped = map[string]bool{
	"Cache": true, "Code Cache": true, "GPUCache": true, "GrShaderCache": true, "ShaderCache": true,
	"GraphiteDawnCache": true, "DawnCache": true, "DawnGraphiteCache": true, "DawnWebGPUCache": true,
	"component_crx_cache": true, "Crashpad": true, "BrowserMetrics": true,
	"SingletonLock": true, "SingletonSocket": true, "SingletonCookie": true,
	".browserpier-mount": true,
}

// Export writes dir (a stopped profile's folder) as an encrypted archive
// to w. Only regular files and folders are read, through os.Root, so
// links the browser planted can't take it outside dir.
func Export(w io.Writer, dir string, m Manifest, passphrase string) error {
	if len(passphrase) < MinPassphrase || len(passphrase) > MaxPassphrase {
		return fmt.Errorf("use a passphrase of %d to %d characters", MinPassphrase, MaxPassphrase)
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return err
	}
	enc, err := age.Encrypt(w, r)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	m.Format = Format
	m.Profile.Icon = readIcon(dir)
	if err := writeManifest(tw, m); err != nil {
		return err
	}
	// Each folder is its own root, so nothing in run/ (proxy credentials)
	// can be reached through a link. A profile never started has no
	// folders yet: its archive is its settings.
	for _, top := range folders {
		if err := addTree(tw, dir, top); err != nil {
			return err
		}
	}
	// Closing in order: age's Close writes the last authenticated chunk,
	// so an archive cut short fails to decrypt instead of importing half.
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return enc.Close()
}

// readIcon reads dir's cached icon, nil if it has none or it can't be
// read: the icon is optional, and the browser reading it again at its next
// stop brings it back.
func readIcon(dir string) []byte {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(favicon.FileName, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, favicon.MaxBytes+1))
	if err != nil || len(b) > favicon.MaxBytes {
		return nil
	}
	return b
}

func writeManifest(tw *tar.Writer, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(b)), ModTime: m.ExportedAt, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = tw.Write(b)
	return err
}

func addTree(tw *tar.Writer, dir, top string) error {
	root, err := os.OpenRoot(filepath.Join(dir, top))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := path.Join(top, rel)
		if skipped[d.Name()] || strings.HasPrefix(d.Name(), ".upload-") { // an upload still being written
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o700, ModTime: info.ModTime(), Typeflag: tar.TypeDir})
		case d.Type().IsRegular():
			return addFile(tw, root, rel, name)
		}
		return nil // links, sockets, FIFOs, devices
	})
}

// addFile adds rel (in root) as name. The browser is stopped and held, so
// nothing swaps entries meanwhile; still, O_NONBLOCK keeps a FIFO from
// hanging the open, and Stat then shows it isn't a file. (os.Root follows
// a link that stays inside root; the walk already skipped links.)
func addFile(tw *tar.Writer, root *os.Root, rel, name string) error {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // gone since the walk
		}
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	// The browser is stopped, so the file shouldn't change; if it does,
	// write exactly the size announced.
	n, err := io.Copy(tw, io.LimitReader(f, info.Size()))
	if err != nil {
		return err
	}
	if n < info.Size() {
		_, err = tw.Write(make([]byte, info.Size()-n))
	}
	return err
}

// Limits bound what an import may write.
type Limits struct {
	MaxBytes   int64 // file contents in total, after decompression
	MaxEntries int
	// KeepFree is disk space the import leaves free on dest's disk.
	KeepFree int64
	// UID and GID own what's written, when Chown is set.
	UID, GID int
	Chown    bool
}

// DefaultLimits are a generous profile's worth.
var DefaultLimits = Limits{MaxBytes: 8 << 30, MaxEntries: 200_000, KeepFree: 512 << 20}

// ErrWrongPassphrase is returned when the passphrase doesn't open the
// archive, or it isn't a BrowserPier export.
var ErrWrongPassphrase = errors.New("wrong passphrase, or not a BrowserPier export")

// maxWorkFactor caps the scrypt cost a crafted archive can ask for:
// age's own cap (2^22) needs about 4 GiB of memory to try.
const maxWorkFactor = 18

// Import decrypts an archive from r into dest, which must not exist yet,
// and returns its manifest. It writes only regular files and folders
// under dest, through os.Root, within lim. On error dest may hold part of
// the archive: remove it.
func Import(r io.Reader, passphrase, dest string, lim Limits) (Manifest, error) {
	var m Manifest
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return m, ErrWrongPassphrase
	}
	id.SetMaxWorkFactor(maxWorkFactor)
	dec, err := age.Decrypt(r, id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) || strings.Contains(err.Error(), "header") {
			return m, ErrWrongPassphrase
		}
		return m, fmt.Errorf("reading the archive: %w", err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return m, ErrWrongPassphrase
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return m, err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return m, err
	}
	defer func() { _ = root.Close() }()
	// dest itself stays the orchestrator's, as a new profile's folder is;
	// only what's inside goes to the browser's user.
	im := importer{root: root, dest: dest, lim: lim, made: map[string]bool{}}
	// The tar stream as a whole is bounded too: the reader consumes PAX
	// and GNU records itself, which no entry or size count sees.
	tr := tar.NewReader(&budgetReader{r: gz, left: tarBudget(lim)})
	var total int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("reading the archive: %w", err)
		}
		if entries == 0 {
			if h.Name != "manifest.json" || h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return m, errors.New("not a BrowserPier export: no manifest")
			}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil || m.Format != Format {
				return m, fmt.Errorf("not a BrowserPier export this version reads (format %d)", m.Format)
			}
			continue
		}
		if entries > lim.MaxEntries {
			return m, fmt.Errorf("the archive holds more than %d entries", lim.MaxEntries)
		}
		name, err := entryName(h.Name)
		if err != nil {
			return m, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := im.mkdirAll(name); err != nil {
				return m, err
			}
		case tar.TypeReg:
			if !strings.Contains(name, "/") {
				return m, fmt.Errorf("%q: not a path an export writes", h.Name) // chrome and files are folders
			}
			if h.Size < 0 || total+h.Size > lim.MaxBytes {
				return m, fmt.Errorf("the archive holds more than %d MiB", lim.MaxBytes>>20)
			}
			total += h.Size
			if err := im.writeFile(name, tr, h); err != nil {
				return m, err
			}
		default:
			return m, fmt.Errorf("%s: only files and folders can be imported", name)
		}
	}
	if m.Format == 0 {
		return m, errors.New("not a BrowserPier export: empty")
	}
	// The icon is drawn again, as one read from the browser is; one that
	// doesn't pass is dropped, and the profile falls back to its glyph.
	icon := m.Profile.Icon
	m.Profile.Icon = nil
	if icon != nil {
		if png, err := favicon.Sanitize(icon); err == nil {
			if err := favicon.WriteFile(dest, png); err != nil {
				return m, err
			}
			m.Profile.Icon = png
		}
	}
	return m, nil
}

// Bounds on an archive path: Chromium's deepest are a dozen levels.
const maxPathLen, maxPathDepth = 1024, 48

// entryName checks an archive path: relative, clean, inside chrome/ or
// files/, and not absurdly long or deep.
func entryName(raw string) (string, error) {
	if len(raw) > maxPathLen || strings.Count(raw, "/") > maxPathDepth {
		return "", fmt.Errorf("an archive path is too long or too deep (%.40q…)", raw)
	}
	name := strings.TrimSuffix(raw, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || path.Clean(name) != name ||
		name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return "", fmt.Errorf("%q: not a path an export writes", raw)
	}
	top, _, _ := strings.Cut(name, "/")
	if top != "chrome" && top != "files" {
		return "", fmt.Errorf("%q: not a path an export writes", raw)
	}
	return name, nil
}

// tarBudget is how much decompressed tar an import reads at most: the
// file contents, the manifest, and per entry its headers, a long path's
// PAX record and padding.
func tarBudget(lim Limits) int64 {
	return lim.MaxBytes + 1<<20 + int64(lim.MaxEntries+2)*4<<10
}

// budgetReader fails once more than left bytes have been read.
type budgetReader struct {
	r    io.Reader
	left int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("the archive is larger than its contents allow")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// importer writes an archive's entries under root, counting every folder
// and file it creates against the limits: one deep path makes many.
type importer struct {
	root  *os.Root
	dest  string
	lim   Limits
	made  map[string]bool // folders made
	nodes int
}

// create counts one more folder or file of size bytes, before it's made.
func (im *importer) create(size int64) error {
	if im.nodes++; im.nodes > im.lim.MaxEntries {
		return fmt.Errorf("the archive holds more than %d entries", im.lim.MaxEntries)
	}
	return haveRoom(im.dest, size, im.lim.KeepFree)
}

// mkdirAll makes name and its parents. A name already taken by a file
// fails: Mkdir's ErrExist is only ever a file, as made has every folder.
func (im *importer) mkdirAll(name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		dir := strings.Join(parts[:i+1], "/")
		if im.made[dir] {
			continue
		}
		if err := im.create(4 << 10); err != nil {
			return err
		}
		if err := im.root.Mkdir(dir, 0o700); err != nil {
			return err
		}
		im.made[dir] = true
		if im.lim.Chown {
			if err := im.root.Lchown(dir, im.lim.UID, im.lim.GID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (im *importer) writeFile(name string, r io.Reader, h *tar.Header) error {
	if err := im.mkdirAll(path.Dir(name)); err != nil {
		return err
	}
	if err := im.create(h.Size); err != nil {
		return err
	}
	// O_EXCL on a tree this import made: nothing is there to follow.
	f, err := im.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, h.Size); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if im.lim.Chown {
		if err := im.root.Lchown(name, im.lim.UID, im.lim.GID); err != nil {
			return err
		}
	}
	_ = im.root.Chtimes(name, h.ModTime, h.ModTime)
	return nil
}

// keepInodes are the inodes an import leaves free, on disks that count them.
const keepInodes = 10_000

// haveRoom fails before a file or folder would leave less than keepFree,
// or fewer than keepInodes inodes, on dest's disk.
func haveRoom(dest string, size, keepFree int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dest, &st); err != nil {
		return nil // can't tell: carry on
	}
	free := int64(st.Bavail) * int64(st.Bsize) //nolint:gosec // block counts fit in int64
	if free-size < keepFree {
		return fmt.Errorf("not enough disk space to import this archive (%d MiB free)", free>>20)
	}
	if st.Files > 0 && st.Ffree < keepInodes {
		return errors.New("not enough free inodes on the disk to import this archive")
	}
	return nil
}
