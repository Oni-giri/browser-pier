package favicon

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, chromeDir, site string) Result {
	t.Helper()
	res, err := Read(context.Background(), chromeDir, site, t.TempDir())
	if err != nil {
		t.Fatalf("Read(%q): %v", site, err)
	}
	return res
}

// icon decodes a result and checks it's a Size×Size PNG.
func icon(t *testing.T, b []byte) image.Image {
	t.Helper()
	if b == nil {
		t.Fatal("no icon")
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != Size || r.Dy() != Size {
		t.Fatalf("icon is %v, want %d×%d", r, Size, Size)
	}
	return img
}

func wantColor(t *testing.T, img image.Image, x, y int, want color.NRGBA) {
	t.Helper()
	got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	if got != want {
		t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
	}
}

func TestPicksTheLargestBitmapUpTo64(t *testing.T) {
	res := read(t, fixtureDir, "mail.example.com")
	if res.Site != "" {
		t.Fatalf("inferred %q with a site given", res.Site)
	}
	wantColor(t, icon(t, res.PNG), 32, 32, green)
}

func TestSiteMatchesItsSubdomains(t *testing.T) {
	// Both example.com icons match; the one on more pages wins.
	wantColor(t, icon(t, read(t, fixtureDir, "example.com").PNG), 10, 10, green)
	wantColor(t, icon(t, read(t, fixtureDir, "accounts.example.com").PNG), 10, 10, grey)
	// Not a suffix match on a label boundary.
	if res := read(t, fixtureDir, "ample.com"); res.PNG != nil {
		t.Fatal("ample.com took example.com's icon")
	}
}

func TestOnlyLargerBitmapsTakeTheSmallest(t *testing.T) {
	// 128 px, not 192, scaled down.
	wantColor(t, icon(t, read(t, fixtureDir, "big.test").PNG), 32, 32, yellow)
}

func TestICO(t *testing.T) {
	// The ICO's 32 px PNG rather than its 16 px BMP.
	wantColor(t, icon(t, read(t, fixtureDir, "ico.test").PNG), 32, 32, navy)
	// A 32-bit BMP in an ICO.
	wantColor(t, icon(t, read(t, fixtureDir, "bmp.test").PNG), 32, 32, teal)
}

func TestKeepsProportions(t *testing.T) {
	img := icon(t, read(t, fixtureDir, "wide.test").PNG)
	wantColor(t, img, 32, 32, blue)
	wantColor(t, img, 32, 4, color.NRGBA{})  // above: transparent
	wantColor(t, img, 32, 60, color.NRGBA{}) // below
}

func TestNoUsableFaviconFallsBack(t *testing.T) {
	for _, site := range []string{"corrupt.test", "huge.test", "nofavicon.test"} {
		if res := read(t, fixtureDir, site); res.PNG != nil {
			t.Errorf("%s: got an icon", site)
		}
	}
	// A profile never started has no folder at all.
	if res := read(t, filepath.Join(t.TempDir(), "chrome"), ""); res.PNG != nil || res.Site != "" {
		t.Fatalf("empty profile: %+v", res)
	}
}

func TestInfersTheSiteFromHistory(t *testing.T) {
	res := read(t, fixtureDir, "")
	if res.Site != "example.com" {
		t.Fatalf("inferred %q, want example.com", res.Site)
	}
	wantColor(t, icon(t, res.PNG), 32, 32, green)
}

func TestSiteGivenBeatsHistory(t *testing.T) {
	res := read(t, fixtureDir, "big.test")
	if res.Site != "" {
		t.Fatalf("inferred %q though a site was given", res.Site)
	}
	wantColor(t, icon(t, res.PNG), 32, 32, yellow)
}

func TestReadsAWriteAheadLog(t *testing.T) {
	// The browser still has the database open: the newest rows are in
	// its -wal. The copy must include them.
	dir := filepath.Join(t.TempDir(), "chrome")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFavicons(t, filepath.Join(dir, "Default", "Favicons"),
		map[int]fixtureIcon{1: {pages: []string{"https://wal.test/"}, bitmaps: [][]byte{solidPNG(t, 32, 32, teal)}}}, "wal")
	if st, err := os.Stat(filepath.Join(dir, "Default", "Favicons-wal")); err != nil || st.Size() == 0 {
		t.Fatalf("no -wal to test with: %v", err)
	}
	wantColor(t, icon(t, read(t, dir, "wal.test").PNG), 32, 32, teal)
}

func TestSkipsBlobsOverTheCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chrome")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	heavy := append(append([]byte{}, pngMagic...), make([]byte, MaxBytes)...)
	writeFavicons(t, filepath.Join(dir, "Default", "Favicons"),
		map[int]fixtureIcon{1: {pages: []string{"https://heavy.test/"}, bitmaps: [][]byte{heavy}}}, "")
	if res := read(t, dir, "heavy.test"); res.PNG != nil {
		t.Fatal("took a blob over the cap")
	}
}

func TestRefusesLinksOutOfTheProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chrome")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(filepath.Join(fixtureDir, "Default", "Favicons"))
	if err := os.Symlink(abs, filepath.Join(dir, "Default", "Favicons")); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), dir, "mail.example.com", t.TempDir()); err == nil {
		t.Fatal("followed a link out of the profile")
	}
}

func TestSanitize(t *testing.T) {
	for name, blob := range map[string][]byte{
		"empty":     nil,
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"gif":       []byte("GIF89a"),
		"truncated": solidPNG(t, 32, 32, blue)[:40],
		"too wide":  solidPNG(t, 257, 16, blue),
		"too big":   append(solidPNG(t, 16, 16, blue), make([]byte, MaxBytes)...),
		"bad ico":   {0, 0, 1, 0, 9, 0},
		"ico, 8 bit": func() []byte {
			b := bmp32(16, 16, teal)
			b[14] = 8
			return icoFile(t, b)
		}(),
		"ico, out of bounds": func() []byte {
			b := icoFile(t, solidPNG(t, 16, 16, blue))
			b[18] = 0xff // offset past the end
			return b
		}(),
	} {
		if _, err := Sanitize(blob); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	// A PNG's own chunks don't survive: only pixels do.
	src := solidPNG(t, 16, 16, blue)
	withText := append(append([]byte{}, src[:33]...), textChunk("Comment", "<script>alert(1)</script>")...)
	withText = append(withText, src[33:]...)
	out, err := Sanitize(withText)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("script")) {
		t.Fatal("a text chunk survived")
	}
	wantColor(t, icon(t, out), 0, 0, blue)
}

// textChunk is a PNG tEXt chunk.
func textChunk(key, value string) []byte {
	body := []byte("tEXt" + key + "\x00" + value)
	b := binary.BigEndian.AppendUint32(nil, uint32(len(body)-4))
	b = append(b, body...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(body))
}

func TestRegistrable(t *testing.T) {
	for host, want := range map[string]string{
		"accounts.google.com":      "google.com",
		"www.bbc.co.uk":            "bbc.co.uk",
		"oni-giri.github.io":       "oni-giri.github.io",
		"localhost":                "localhost",
		"172.17.0.1":               "172.17.0.1",
		"host.containers.internal": "containers.internal",
	} {
		if got := Registrable(host); got != want {
			t.Errorf("Registrable(%q) = %q, want %q", host, got, want)
		}
	}
	if !strings.HasSuffix(Registrable("mail.example.com"), "example.com") {
		t.Fatal("example.com")
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(dir, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(dir, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, FileName)); string(b) != "two" {
		t.Fatalf("icon = %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if err := RemoveFile(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFile(dir); err != nil {
		t.Fatalf("removing no icon: %v", err)
	}
}
