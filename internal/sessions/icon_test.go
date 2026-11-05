package sessions

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withFixtureHistory puts the favicon package's fixture Favicons and
// History databases in the profile, as its browser would have left them.
func (f *fixture) withFixtureHistory(profile string) {
	f.t.Helper()
	dst := filepath.Join(f.dir, "profiles", profile, "chrome", "Default")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		f.t.Fatal(err)
	}
	for _, name := range []string{"Favicons", "History"} {
		b, err := os.ReadFile(filepath.Join("..", "favicon", "testdata", "chrome", "Default", name))
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) stop(id string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s, err := f.m.Stop(ctx, id); err != nil || s.State != Stopped {
		f.t.Fatalf("Stop = %+v, %v", s, err)
	}
}

// iconColor is the colour at the middle of the cached icon.
func (f *fixture) iconColor() color.NRGBA {
	f.t.Helper()
	b, err := os.ReadFile(f.layout.IconFile("google"))
	if err != nil {
		f.t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		f.t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 64 || r.Dy() != 64 {
		f.t.Fatalf("icon is %v", r)
	}
	return color.NRGBAModel.Convert(img.At(32, 32)).(color.NRGBA)
}

func TestGracefulStopCachesTheIcon(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	f.withFixtureHistory("google")
	if err := f.m.RefreshIcon("google"); !errors.Is(err, ErrProfileBusy) {
		t.Fatalf("RefreshIcon while running = %v, want ErrProfileBusy", err)
	}
	f.stop(s.ID)

	// No site set: the most visited one, and its icon.
	p, _ := f.m.Profiles().Get(ctx, "google")
	if p.Site != "" || p.SiteInferred != "example.com" {
		t.Fatalf("site %q, inferred %q; want example.com inferred", p.Site, p.SiteInferred)
	}
	if got := f.iconColor(); got != (color.NRGBA{0, 128, 0, 255}) {
		t.Fatalf("icon colour %v, want the main site's green", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(f.dir, "profiles", "google", ".icon-*")); len(entries) != 0 {
		t.Fatalf("copies left behind: %v", entries)
	}

	// A site set wins, and the inferred one isn't touched.
	p.Site = "big.test"
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := f.m.RefreshIcon("google"); err != nil {
		t.Fatal(err)
	}
	if got := f.iconColor(); got != (color.NRGBA{255, 255, 0, 255}) {
		t.Fatalf("icon colour %v, want big.test's yellow", got)
	}
	if p, _ := f.m.Profiles().Get(ctx, "google"); p.SiteInferred != "example.com" {
		t.Fatalf("inferred site became %q", p.SiteInferred)
	}

	// A site with no favicon drops the old icon, for the fallback.
	p.Site = "nofavicon.test"
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	s = f.start(f.m)
	f.stop(s.ID)
	if _, err := os.Stat(f.layout.IconFile("google")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old icon stayed: %v", err)
	}
}

func TestCrashLeavesTheIcon(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.withFixtureHistory("google")
	f.rt.Exit(f.stored(s.ID).ContainerID, 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, live := f.m.Live("google"); !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session didn't end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(f.layout.IconFile("google")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a crashed browser's databases were read: %v", err)
	}
}

func TestBrokenFaviconsDoesNotFailTheStop(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	dst := filepath.Join(f.dir, "profiles", "google", "chrome", "Default")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "History"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dst, "Favicons")); err != nil {
		t.Fatal(err)
	}
	f.stop(s.ID)
	if _, err := os.Stat(f.layout.IconFile("google")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an icon from nothing: %v", err)
	}
}
