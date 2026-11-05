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

func TestIgnoresAWriteAheadLog(t *testing.T) {
	// The browser had the database open: everything is still in its
	// -wal. That isn't replayed, so the read fails cleanly.
	dir := filepath.Join(t.TempDir(), "chrome")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFavicons(t, filepath.Join(dir, "Default", "Favicons"),
		map[int]fixtureIcon{1: {pages: []string{"https://wal.test/"}, bitmaps: [][]byte{solidPNG(t, 32, 32, teal)}}}, "wal")
	if st, err := os.Stat(filepath.Join(dir, "Default", "Favicons-wal")); err != nil || st.Size() == 0 {
		t.Fatalf("no -wal to test with: %v", err)
	}
	res, err := Read(context.Background(), dir, "wal.test", t.TempDir())
	if err == nil && res.PNG != nil {
		t.Fatal("the -wal was applied")
	}
}

// A hot journal names a super-journal, which SQLite deletes when its
// child journals are gone. The browser writes the journal, so it must not
// get to choose a file for the orchestrator to delete. This one also says
// the database had one page: replayed, it would cut the copy short and
// the read would fail.
func TestHotJournalCantDeleteFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chrome")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(fixtureDir, "Default", "Favicons"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Favicons"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(victim, []byte(filepath.Join(t.TempDir(), "gone-journal")+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Favicons-journal"), hotJournal(victim), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Read(context.Background(), dir, "mail.example.com", t.TempDir())
	if _, serr := os.Stat(victim); serr != nil {
		t.Fatalf("the super-journal the journal named is gone: %v", serr)
	}
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	wantColor(t, icon(t, res.PNG), 32, 32, green)
}

// hotJournal is a rollback journal with no pages and a super-journal
// name, in SQLite's format (pager.c): a header padded to the sector,
// then the name, its length, its checksum and the magic again.
func hotJournal(super string) []byte {
	magic := []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}
	hdr := append([]byte{}, magic...)
	hdr = binary.BigEndian.AppendUint32(hdr, 0)    // records
	hdr = binary.BigEndian.AppendUint32(hdr, 0)    // checksum nonce
	hdr = binary.BigEndian.AppendUint32(hdr, 1)    // pages before: replayed, it would cut the database short
	hdr = binary.BigEndian.AppendUint32(hdr, 512)  // sector size
	hdr = binary.BigEndian.AppendUint32(hdr, 4096) // page size
	b := append(hdr, make([]byte, 512-len(hdr))...)
	// The super-journal record starts with a page number, the lock page's.
	b = binary.BigEndian.AppendUint32(b, uint32(1<<30/4096+1))
	b = append(b, super...)
	sum := uint32(0)
	for _, c := range []byte(super) {
		sum += uint32(c)
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(super)))
	b = binary.BigEndian.AppendUint32(b, sum)
	return append(b, magic...)
}

func TestRefusesAViewForATable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chrome")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	var schema []string
	for _, q := range faviconsSchema {
		if !strings.Contains(q, "favicon_bitmaps") {
			schema = append(schema, q)
		}
	}
	d := create(t, filepath.Join(dir, "Default", "Favicons"), append(schema,
		`CREATE VIEW favicon_bitmaps AS SELECT 1 AS id, 1 AS icon_id, 0 AS last_updated, randomblob(16) AS image_data, 16 AS width, 16 AS height`,
		`INSERT INTO icon_mapping (page_url, icon_id) VALUES ('https://view.test/', 1)`), "")
	_ = d.Close()
	if _, err := Read(context.Background(), dir, "view.test", t.TempDir()); err == nil || !strings.Contains(err.Error(), "isn't a table") {
		t.Fatalf("Read = %v, want a refusal", err)
	}
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
