package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
)

const pass = "correct horse battery"

// profileDir is a stopped profile's folder, with what must and mustn't go.
func profileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("chrome/Default/Cookies", "session=abc")
	write("chrome/Default/Local Storage/leveldb/000003.log", "storage")
	write("chrome/Default/Cache/Cache_Data/data_0", "cache")
	write("chrome/.browserpier-mount", "s1")
	write("files/downloads/report.csv", "a,b")
	write("run/proxy.json", `{"password":"s3cret"}`)
	write("run/.browserpier-mount", "s1")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "chrome/Default/link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "chrome/Default/fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRoundTrip(t *testing.T) {
	src := profileDir(t)
	var buf bytes.Buffer
	m := Manifest{Version: "test", ExportedAt: time.Now(), Profile: Profile{Name: "google", DefaultMode: "visual", Width: 1440, Height: 900, Proxy: "http://u:p@proxy:1"}}
	if err := Export(&buf, src, m, pass); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("session=abc")) || bytes.Contains(buf.Bytes(), []byte("u:p@proxy")) {
		t.Fatal("the archive isn't encrypted")
	}
	dest := filepath.Join(t.TempDir(), "imported")
	got, err := Import(bytes.NewReader(buf.Bytes()), pass, dest, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != m.Profile || got.Format != Format {
		t.Fatalf("manifest %+v", got)
	}
	for name, want := range map[string]string{
		"chrome/Default/Cookies":                         "session=abc",
		"chrome/Default/Local Storage/leveldb/000003.log": "storage",
		"files/downloads/report.csv":                     "a,b",
	} {
		if b, err := os.ReadFile(filepath.Join(dest, name)); err != nil || string(b) != want {
			t.Errorf("%s: %q, %v", name, b, err)
		}
	}
	for _, name := range []string{"run", "chrome/Default/Cache", "chrome/.browserpier-mount", "chrome/Default/link", "chrome/Default/fifo"} {
		if _, err := os.Lstat(filepath.Join(dest, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was carried over", name)
		}
	}
}

func TestExportRefusesAShortPassphrase(t *testing.T) {
	if err := Export(&bytes.Buffer{}, profileDir(t), Manifest{}, "short"); err == nil {
		t.Fatal("exported with a short passphrase")
	}
}

func TestWrongPassphrase(t *testing.T) {
	var buf bytes.Buffer
	if err := Export(&buf, profileDir(t), Manifest{}, pass); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "x")
	if _, err := Import(&buf, "not the passphrase", dest, DefaultLimits); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Import(strings.NewReader("not an archive at all"), pass, dest, DefaultLimits); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("not an archive: %v", err)
	}
}

// crafted builds an encrypted archive from raw tar entries.
func crafted(t *testing.T, entries []tar.Header, body []byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	r, _ := age.NewScryptRecipient(pass)
	r.SetWorkFactor(10)
	enc, _ := age.Encrypt(&buf, r)
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	_ = writeManifest(tw, Manifest{Format: Format})
	for _, h := range entries {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write(body[:h.Size])
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = enc.Close()
	return &buf
}

func TestImportRefusesWhatAnExportNeverHolds(t *testing.T) {
	body := []byte("x")
	for name, h := range map[string]tar.Header{
		"parent":      {Name: "chrome/../../evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		"absolute":    {Name: "/tmp/evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		"elsewhere":   {Name: "run/proxy.json", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		"symlink":     {Name: "chrome/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hardlink":    {Name: "chrome/hard", Typeflag: tar.TypeLink, Linkname: "chrome/x"},
		"fifo":        {Name: "chrome/fifo", Typeflag: tar.TypeFifo},
		"not clean":   {Name: "chrome//x", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
		"backslashes": {Name: `chrome\..\x`, Typeflag: tar.TypeReg, Size: 1, Mode: 0o600},
	} {
		dest := filepath.Join(t.TempDir(), "x")
		if _, err := Import(crafted(t, []tar.Header{h}, body), pass, dest, DefaultLimits); err == nil {
			t.Errorf("%s: imported", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); err == nil {
			t.Errorf("%s: wrote outside dest", name)
		}
	}
}

func TestImportLimits(t *testing.T) {
	big := make([]byte, 4<<20)
	lim := DefaultLimits
	lim.MaxBytes = 1 << 20
	dest := filepath.Join(t.TempDir(), "x")
	if _, err := Import(crafted(t, []tar.Header{{Name: "chrome/big", Typeflag: tar.TypeReg, Size: int64(len(big)), Mode: 0o600}}, big), pass, dest, lim); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("over the size cap: %v", err)
	}
	lim = DefaultLimits
	lim.MaxEntries = 3
	var many []tar.Header
	for i := range 5 {
		many = append(many, tar.Header{Name: "chrome/d" + string(rune('a'+i)), Typeflag: tar.TypeDir, Mode: 0o700})
	}
	dest = filepath.Join(t.TempDir(), "y")
	if _, err := Import(crafted(t, many, nil), pass, dest, lim); err == nil {
		t.Fatal("over the entry cap")
	}
}

func TestImportRefusesAnExpensivePassphraseHeader(t *testing.T) {
	var buf bytes.Buffer
	r, _ := age.NewScryptRecipient(pass)
	r.SetWorkFactor(maxWorkFactor + 1)
	enc, _ := age.Encrypt(&buf, r)
	_ = enc.Close()
	if _, err := Import(&buf, pass, filepath.Join(t.TempDir(), "x"), DefaultLimits); err == nil {
		t.Fatal("decrypted a header past the work factor cap")
	}
}
