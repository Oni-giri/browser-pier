// Package archive exports a profile's folder as a passphrase-encrypted
// archive and imports one into a new profile. An archive holds live
// session cookies, so it is a secret: it is encrypted with age (a
// passphrase, scrypt), which `age -d` can also open.
//
// Inside: manifest.json first, then a gzipped tar of the profile's
// chrome/ (Chromium's user data) and files/ folders, regular files and
// folders only. run/ (the control socket, and briefly proxy credentials)
// and Chromium's caches stay out.
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
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
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
	HandoffPolicy   string `json:"handoff_policy"`
	TimeZone        string `json:"time_zone,omitempty"`
	Locale          string `json:"locale,omitempty"`
}

// MinPassphrase is the shortest passphrase an export takes.
const MinPassphrase = 10

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
	if len(passphrase) < MinPassphrase {
		return fmt.Errorf("use a passphrase of at least %d characters", MinPassphrase)
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
	if err := writeManifest(tw, m); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, top := range folders {
		if err := addTree(tw, root, top); err != nil {
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

func addTree(tw *tar.Writer, root *os.Root, top string) error {
	if _, err := root.Lstat(top); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fs.WalkDir(root.FS(), top, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipped[d.Name()] {
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
			return addFile(tw, root, name)
		}
		return nil // links, sockets, FIFOs, devices
	})
}

func addFile(tw *tar.Writer, root *os.Root, name string) error {
	// O_NONBLOCK: if the entry was swapped for a FIFO since the walk saw
	// it, opening it doesn't hang; Stat then shows it isn't a file.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
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
	if lim.Chown {
		if err := root.Lchown(".", lim.UID, lim.GID); err != nil {
			return m, err
		}
	}
	tr := tar.NewReader(gz)
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
			if err := mkdirAll(root, name, lim); err != nil {
				return m, err
			}
		case tar.TypeReg:
			if h.Size < 0 || total+h.Size > lim.MaxBytes {
				return m, fmt.Errorf("the archive holds more than %d MiB", lim.MaxBytes>>20)
			}
			total += h.Size
			if err := haveRoom(dest, h.Size, lim.KeepFree); err != nil {
				return m, err
			}
			if err := writeFile(root, name, tr, h, lim); err != nil {
				return m, err
			}
		default:
			return m, fmt.Errorf("%s: only files and folders can be imported", name)
		}
	}
	if m.Format == 0 {
		return m, errors.New("not a BrowserPier export: empty")
	}
	return m, nil
}

// entryName checks an archive path: relative, clean, inside chrome/ or
// files/.
func entryName(raw string) (string, error) {
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

func mkdirAll(root *os.Root, name string, lim Limits) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		dir := strings.Join(parts[:i+1], "/")
		if err := root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if lim.Chown {
			if err := root.Lchown(dir, lim.UID, lim.GID); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFile(root *os.Root, name string, r io.Reader, h *tar.Header, lim Limits) error {
	if dir := path.Dir(name); dir != "." {
		if err := mkdirAll(root, dir, lim); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
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
	if lim.Chown {
		if err := root.Lchown(name, lim.UID, lim.GID); err != nil {
			return err
		}
	}
	_ = root.Chtimes(name, h.ModTime, h.ModTime)
	return nil
}

// haveRoom fails before a file would leave less than keepFree on dest's disk.
func haveRoom(dest string, size, keepFree int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dest, &st); err != nil {
		return nil // can't tell: carry on
	}
	free := int64(st.Bavail) * int64(st.Bsize) //nolint:gosec // block counts fit in int64
	if free-size < keepFree {
		return fmt.Errorf("not enough disk space to import this archive (%d MiB free)", free>>20)
	}
	return nil
}
