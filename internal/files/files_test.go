package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func setup(t *testing.T, limit int64) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	for _, sub := range []string{Downloads, Uploads} {
		if err := os.MkdirAll(filepath.Join(base, "google", sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return New(func(p string) string { return filepath.Join(base, p) }, limit, os.Getuid(), os.Getgid()), filepath.Join(base, "google")
}

func write(t *testing.T, path, content string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestListOpenDelete(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	write(t, filepath.Join(dl, "old.pdf"), "old", time.Hour)
	write(t, filepath.Join(dl, "new.csv"), "new!", time.Minute)
	write(t, filepath.Join(dl, "big.zip.crdownload"), "partial", 0)
	write(t, filepath.Join(dir, "secret"), "outside", 0)
	if err := os.Symlink(filepath.Join(dir, "secret"), filepath.Join(dl, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dl, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}

	list, err := s.List("google")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "new.csv" || list[0].Size != 4 || list[1].Name != "old.pdf" {
		t.Fatalf("List = %+v; want the two finished regular files, newest first", list)
	}

	f, info, err := s.Open("google", "new.csv")
	if err != nil || info.Size != 4 {
		t.Fatalf("Open = %+v, %v", info, err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "new!" {
		t.Fatalf("read %q", b)
	}
	for _, name := range []string{"link.txt", "folder", "big.zip.crdownload", "../secret", "missing", "", ".hidden"} {
		if f, _, err := s.Open("google", name); !errors.Is(err, ErrNotFound) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("Open(%q) = %v, want ErrNotFound", name, err)
		}
	}

	if err := s.Delete("google", "link.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete through a symlink = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret")); err != nil {
		t.Fatal("deleting a symlink touched its target")
	}
	if err := s.Delete("google", "old.pdf"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("google", "old.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v", err)
	}
	if list, _ := s.List("never-started"); len(list) != 0 {
		t.Fatalf("a profile without files lists %v", list)
	}
}

func TestDownloadsFolderReplacedBySymlink(t *testing.T) {
	// The browser can replace downloads/ with a symlink to a host path;
	// nothing outside the profile's files folder may be listed or served.
	s, dir := setup(t, 0)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "passwd"), "root:x:0:0", 0)
	dl := filepath.Join(dir, Downloads)
	if err := os.RemoveAll(dl); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dl); err != nil {
		t.Fatal(err)
	}
	if list, err := s.List("google"); err == nil && len(list) > 0 {
		t.Fatalf("listed files outside the profile: %+v", list)
	}
	if f, _, err := s.Open("google", "passwd"); err == nil {
		_ = f.Close()
		t.Fatal("served a file outside the profile")
	}
	if err := s.Delete("google", "passwd"); err == nil {
		t.Fatal("deleted a file outside the profile")
	}
	if _, err := os.Stat(filepath.Join(outside, "passwd")); err != nil {
		t.Fatal("the outside file is gone")
	}
	// The same through a relative link.
	if err := os.Remove(dl); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(dir, outside)
	if err := os.Symlink(rel, dl); err != nil {
		t.Fatal(err)
	}
	if f, _, err := s.Open("google", "passwd"); err == nil {
		_ = f.Close()
		t.Fatal("served a file outside the profile through a relative link")
	}
}

func TestUpload(t *testing.T) {
	s, dir := setup(t, 0)
	f, path, err := s.Upload("google", "invoice.pdf", strings.NewReader("%PDF"), 1<<20)
	if err != nil || f.Name != "invoice.pdf" || f.Size != 4 || path != "/files/uploads/invoice.pdf" {
		t.Fatalf("Upload = %+v, %q, %v", f, path, err)
	}
	f, path, _ = s.Upload("google", "invoice.pdf", strings.NewReader("%PDF2"), 1<<20)
	if f.Name != "invoice (2).pdf" || path != "/files/uploads/invoice (2).pdf" {
		t.Fatalf("second upload with the same name: %+v %q", f, path)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, Uploads, "invoice.pdf")); string(b) != "%PDF" {
		t.Fatal("the first upload was overwritten")
	}
	if _, _, err := s.Upload("google", "big.bin", strings.NewReader(strings.Repeat("x", 11)), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	for _, bad := range []string{"../escape", "a/b", ".hidden", "", "tab\tname", strings.Repeat("a", 256)} {
		if _, _, err := s.Upload("google", bad, strings.NewReader("x"), 10); !errors.Is(err, ErrBadName) {
			t.Errorf("Upload(%q) = %v", bad, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, Uploads))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestEnforceEvictsOldestFirst(t *testing.T) {
	s, dir := setup(t, 10)
	write(t, filepath.Join(dir, Downloads, "a"), "aaaa", 3*time.Hour)
	write(t, filepath.Join(dir, Uploads, "b"), "bbbb", 2*time.Hour)
	write(t, filepath.Join(dir, Downloads, "c"), "cccc", time.Hour)
	write(t, filepath.Join(dir, Downloads, "d.crdownload"), "dd", 4*time.Hour) // in progress: counted, never evicted
	removed, err := s.Enforce("google")
	if err != nil {
		t.Fatal(err)
	}
	// 14 bytes: dropping a (oldest finished) leaves 10.
	if strings.Join(removed, ",") != "downloads/a" {
		t.Fatalf("removed %v, want only the oldest", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, Downloads, "d.crdownload")); err != nil {
		t.Fatal("an unfinished download was evicted")
	}
	if removed, _ := s.Enforce("google"); len(removed) != 0 {
		t.Fatalf("under the cap, still removed %v", removed)
	}
	// Uploading past the cap evicts the oldest too.
	if _, _, err := s.Upload("google", "e", strings.NewReader("eeee"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, Uploads, "b")); !os.IsNotExist(err) {
		t.Fatal("upload past the cap didn't evict the oldest file")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"report.pdf", "Ünïcødé ✓.txt", "a b (2).zip"} {
		if !ValidName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
}

func TestOpenNeverBlocksOnAFIFO(t *testing.T) {
	s, dir := setup(t, 0)
	if err := syscall.Mkfifo(filepath.Join(dir, Downloads, "trap"), 0o600); err != nil {
		t.Skip("no mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		f, _, err := s.Open("google", "trap")
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Open(fifo) = %v, want ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open blocked on a FIFO")
	}
	if list, _ := s.List("google"); len(list) != 0 {
		t.Fatalf("a FIFO was listed: %v", list)
	}
}

func TestConcurrentSameNameUploadsKeepBoth(t *testing.T) {
	s, dir := setup(t, 0)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Upload("google", "same.txt", strings.NewReader(fmt.Sprint(i)), 100); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	entries, _ := os.ReadDir(filepath.Join(dir, Uploads))
	if len(entries) != 10 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("10 uploads left %d files: %v", len(entries), names)
	}
}

func TestUploadOverTheCapFails(t *testing.T) {
	s, dir := setup(t, 10)
	if _, _, err := s.Upload("google", "big", strings.NewReader(strings.Repeat("x", 11)), 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an upload bigger than the whole cap: %v", err)
	}
	// One that fits alone but not beside a download in progress (never
	// evicted) is evicted at once: the upload must fail, not report success.
	write(t, filepath.Join(dir, Downloads, "x.crdownload"), "12345678", 0)
	if _, _, err := s.Upload("google", "mid", strings.NewReader("abcdef"), 100); !errors.Is(err, ErrOverCap) {
		t.Fatalf("an upload that can't fit: %v", err)
	}
}

func TestUploadIsCountedOnce(t *testing.T) {
	// 6 bytes under a 10-byte cap: if the temporary link were counted as
	// well, the cap would look exceeded and the upload would be evicted.
	s, dir := setup(t, 10)
	if _, _, err := s.Upload("google", "six", strings.NewReader("123456"), 100); err != nil {
		t.Fatal(err)
	}
	// Another upload still arriving doesn't count either.
	write(t, filepath.Join(dir, Uploads, ".upload-123"), "1234567890", 0)
	if removed, err := s.Enforce("google"); err != nil || len(removed) != 0 {
		t.Fatalf("Enforce removed %v, %v", removed, err)
	}
}
