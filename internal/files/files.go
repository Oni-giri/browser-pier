// Package files manages each profile's downloads and the files uploaded
// into its sessions. The browser can write anything in these folders,
// symlinks included, so everything here goes through os.Root, which
// won't follow a link out of the folder, and skips whatever isn't a
// regular file.
package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// The folders inside a profile's files folder, as the browser sees them
// under /files.
const (
	Downloads = "downloads"
	Uploads   = "uploads"
	// MountPoint is where the files folder is mounted in the browser container.
	MountPoint = "/files"
	// partial marks a download Chrome is still writing.
	partial = ".crdownload"
)

var (
	ErrNotFound = errors.New("no such file")
	ErrBadName  = errors.New("not a valid file name")
	ErrTooLarge = errors.New("the file is too large")
	// ErrOverCap: the file doesn't fit under the profile's cap even after
	// evicting everything older.
	ErrOverCap = errors.New("the file doesn't fit under the profile's files cap")
)

// File is a downloaded or uploaded file.
type File struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Store reads and writes profiles' files folders.
type Store struct {
	dir      func(profile string) string // the profile's files folder
	limit    int64                       // bytes per profile, both folders together; 0 is no limit
	uid, gid int                         // the browser's user, who must be able to read uploads

	mu   sync.Mutex
	seen map[string]map[string]seen // by session: its downloads folder at its last notice
}

// New makes a Store. dir gives a profile's files folder; limit caps the
// bytes a profile's files may take, evicting the oldest first (0: no cap).
// uid and gid are the browser containers' user: uploads are handed to it
// when the orchestrator runs as root.
func New(dir func(profile string) string, limit int64, uid, gid int) *Store {
	return &Store{dir: dir, limit: limit, uid: uid, gid: gid}
}

// Limit is the per-profile cap in bytes, or 0.
func (s *Store) Limit() int64 { return s.limit }

// open opens a profile's downloads or uploads folder. The files folder
// itself is the browser's mount point, which it can't replace; the
// subfolders it can, with a symlink to anywhere, so they are opened
// inside the files folder's root, which refuses a link that leaves it.
func (s *Store) open(profile, sub string) (*os.Root, error) {
	files, err := os.OpenRoot(s.dir(profile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = files.Close() }()
	// Not even a link that stays inside (uploads -> downloads): the
	// folder itself must be a real one, as the browser checks too.
	if fi, err := files.Lstat(sub); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrNotFound
	}
	root, err := files.OpenRoot(sub)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return root, err
}

// List returns the profile's finished downloads, newest first.
func (s *Store) List(profile string) ([]File, error) {
	root, err := s.open(profile, Downloads)
	if errors.Is(err, ErrNotFound) {
		return []File{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	all, err := regularFiles(root)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(all))
	for _, f := range all {
		// Not a download under way, nor one of Chrome's hidden temporary
		// files on the way to one.
		if !strings.HasSuffix(f.Name, partial) && !strings.HasPrefix(f.Name, ".") {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

func regularFiles(root *os.Root) ([]File, error) {
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue // symlinks, folders, sockets: never ours to serve
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	return out, nil
}

// Open opens a finished download for reading. The caller closes it.
func (s *Store) Open(profile, name string) (*os.File, File, error) {
	if !ValidName(name) || strings.HasSuffix(name, partial) {
		return nil, File{}, ErrNotFound
	}
	root, err := s.open(profile, Downloads)
	if err != nil {
		return nil, File{}, err
	}
	defer func() { _ = root.Close() }()
	// The browser could swap the file for a FIFO or a link between any
	// check and the open, so open without following links and without
	// blocking (a FIFO would wait for a writer), then check what was opened.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, File{}, ErrNotFound
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, File{}, ErrNotFound
	}
	return f, File{Name: name, Size: info.Size(), Modified: info.ModTime()}, nil
}

// Delete removes a download.
func (s *Store) Delete(profile, name string) error {
	if !ValidName(name) {
		return ErrNotFound
	}
	root, err := s.open(profile, Downloads)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if info, err := root.Lstat(name); err != nil || !info.Mode().IsRegular() {
		return ErrNotFound
	}
	return root.Remove(name)
}

// Upload stores r as uploads/name (or "name (2)" and so on if taken), at
// most max bytes, and returns the file and its path in the browser.
func (s *Store) Upload(profile, name string, r io.Reader, max int64) (File, string, error) {
	if !ValidName(name) {
		return File{}, "", ErrBadName
	}
	if s.limit > 0 && s.limit < max {
		max = s.limit
	}
	root, err := s.open(profile, Uploads)
	if err != nil {
		return File{}, "", err
	}
	defer func() { _ = root.Close() }()
	tmp := fmt.Sprintf(".upload-%d", time.Now().UnixNano())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return File{}, "", err
	}
	defer func() { _ = root.Remove(tmp) }()
	if os.Getuid() == 0 {
		// A root orchestrator runs browsers as another user, who must
		// be able to read the file.
		if err := f.Chown(s.uid, s.gid); err != nil {
			_ = f.Close()
			return File{}, "", err
		}
	}
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = ErrTooLarge
	}
	if err != nil {
		return File{}, "", err
	}
	final, err := publish(root, tmp, name)
	if err != nil {
		return File{}, "", err
	}
	// Drop the temporary name before counting: it is a second link to the
	// same file and would be counted twice.
	_ = root.Remove(tmp)
	removed, err := s.Enforce(profile)
	if err != nil {
		return File{}, "", err
	}
	for _, r := range removed {
		if r == Uploads+"/"+final {
			return File{}, "", ErrOverCap
		}
	}
	return File{Name: final, Size: n, Modified: time.Now()}, path.Join(MountPoint, Uploads, final), nil
}

// Uploaded checks that uploads/name is a file the browser can be given
// (PROJ-117): a plain name (ValidName), opened inside the uploads
// folder's root without following a link, and a regular file. It returns
// the file and its path in the browser, /files/uploads/<name>, the only
// kind of path act's upload step hands to DOM.setFileInputFiles. A
// symlink, a folder, a FIFO or a missing file is ErrNotFound.
func (s *Store) Uploaded(profile, name string) (File, string, error) {
	if !ValidName(name) {
		return File{}, "", ErrBadName
	}
	root, err := s.open(profile, Uploads)
	if err != nil {
		return File{}, "", err
	}
	defer func() { _ = root.Close() }()
	// os.Root follows a link that stays inside the folder, but the
	// browser resolves the path in its own filesystem: the name itself
	// must be a regular file, not a link of any kind (Lstat), and what is
	// opened must be that same file.
	link, err := root.Lstat(name)
	if err != nil || !link.Mode().IsRegular() {
		return File{}, "", ErrNotFound
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return File{}, "", ErrNotFound
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(link, info) {
		return File{}, "", ErrNotFound
	}
	return File{Name: name, Size: info.Size(), Modified: info.ModTime()}, path.Join(MountPoint, Uploads, name), nil
}

// publish links tmp in as name, or name with " (2)", " (3)"… before its
// extension, whichever is free. A hard link never replaces an existing
// file, so two uploads with the same name can't overwrite each other.
func publish(root *os.Root, tmp, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		try := name
		if i > 1 {
			try = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		err := root.Link(tmp, try)
		if err == nil {
			return try, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("too many files with that name")
}

// Enforce removes the profile's oldest files, uploads and finished
// downloads alike, until it is under the cap. It returns what it removed.
func (s *Store) Enforce(profile string) ([]string, error) {
	if s.limit <= 0 {
		return nil, nil
	}
	type entry struct {
		File
		root *os.Root
		sub  string
	}
	var all []entry
	var total int64
	for _, sub := range []string{Downloads, Uploads} {
		root, err := s.open(profile, sub)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		files, err := regularFiles(root)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if strings.HasPrefix(f.Name, ".upload-") {
				continue // an upload still arriving; counted once it's published
			}
			total += f.Size
			if !strings.HasSuffix(f.Name, partial) {
				all = append(all, entry{f, root, sub})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Modified.Before(all[j].Modified) })
	var removed []string
	for _, e := range all {
		if total <= s.limit {
			break
		}
		if err := e.root.Remove(e.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		total -= e.Size
		removed = append(removed, e.sub+"/"+e.Name)
	}
	return removed, nil
}

// Sweep enforces the cap for the profiles listed by live, every interval,
// until ctx ends: downloads arrive while a browser runs, not only through
// the API.
func (s *Store) Sweep(ctx context.Context, live func() []string, every time.Duration, onError func(profile string, err error)) {
	if s.limit <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, p := range live() {
				if _, err := s.Enforce(p); err != nil && onError != nil {
					onError(p, err)
				}
			}
		}
	}
}

// ValidName reports whether name is a plain file name: no folders, not
// hidden, printable, at most 255 bytes.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
