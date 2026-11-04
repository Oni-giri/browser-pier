//go:build docker

package sessions

import (
	"bytes"
	"context"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/favicon"
)

// TestIconAfterGracefulStop: a page's favicon, as the real browser cached
// it, becomes the profile's icon when the browser stops, and the site it
// was on becomes the profile's inferred main site.
func TestIconAfterGracefulStop(t *testing.T) {
	f := newRealFixture(t)
	m := f.manager(f.layout)
	name := f.profile(m)
	site := browsertest.NewSite(t, siteHost(t))

	s := startRunning(t, m, name)
	p := browsertest.Page(t, browsertest.ControlClient(f.layout.ControlSocket(name)))
	p.Navigate(site.URL + "/icon")
	deadline := time.Now().Add(30 * time.Second)
	for !site.IconServed() {
		if time.Now().After(deadline) {
			t.Fatal("the browser never fetched the page's favicon")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Chromium saves the favicon a moment after fetching it.
	time.Sleep(2 * time.Second)
	p.Close()
	stopCleanly(t, m, s.ID)

	entries, _ := filepath.Glob(filepath.Join(f.dir, "profiles", name, "chrome", "Default", "Favicons*"))
	t.Logf("favicon cache files: %v", entries)
	b, err := os.ReadFile(f.layout.IconFile(name))
	if err != nil {
		t.Fatalf("no icon after the stop: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != favicon.Size || r.Dy() != favicon.Size {
		t.Fatalf("icon is %v", r)
	}
	if got := color.NRGBAModel.Convert(img.At(32, 32)).(color.NRGBA); got != (color.NRGBA{0x2f, 0x6b, 0xff, 0xff}) {
		t.Fatalf("icon colour %v, want the site's", got)
	}
	prof, err := m.Profiles().Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if want := favicon.Registrable(siteHost(t)); prof.SiteInferred != want {
		t.Fatalf("inferred site %q, want %q", prof.SiteInferred, want)
	}
}
