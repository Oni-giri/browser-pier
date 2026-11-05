package profiles

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestValidateSite(t *testing.T) {
	for _, site := range []string{"", "google.com", "accounts.google.com", "localhost", "xn--bcher-kva.de", "192.168.1.10",
		"a-b.example", strings.Repeat("a", 63) + ".com", strings.Repeat("a.", 126) + "b"} {
		p := New("p")
		p.Site = site
		if err := p.Validate(); err != nil {
			t.Errorf("%q: %v", site, err)
		}
	}
	for _, site := range []string{"Google.com", "https://google.com", "google.com/", "-a.com", "a-.com", "a..com", ".com", "a_b.com",
		"google.com:443", "::1", "bücher.de", "a b.com", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "b"} {
		p := New("p")
		p.Site = site
		if err := p.Validate(); err == nil {
			t.Errorf("%q: want an error", site)
		}
	}
}

func TestNormalizeSite(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"  Mail.Google.com ":                "mail.google.com",
		"https://mail.google.com/mail/u/0/": "mail.google.com",
		"github.com/login":                  "github.com",
		"example.com:8443":                  "example.com",
		"example.com.":                      "example.com",
		"not a site":                        "not a site",
	} {
		if got := NormalizeSite(in); got != want {
			t.Errorf("NormalizeSite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateIcon(t *testing.T) {
	for _, g := range Glyphs {
		p := New("p")
		p.IconGlyph = g
		if err := p.Validate(); err != nil {
			t.Errorf("glyph %q: %v", g, err)
		}
	}
	for _, c := range Colors {
		p := New("p")
		p.IconColor = c
		if err := p.Validate(); err != nil {
			t.Errorf("colour %q: %v", c, err)
		}
	}
	for _, c := range []string{"orange", "red", "Blue", "#2f6bff"} {
		p := New("p")
		p.IconColor = c
		if err := p.Validate(); err == nil {
			t.Errorf("colour %q: want an error", c)
		}
	}
	p := New("p")
	p.IconGlyph = "<svg>"
	if err := p.Validate(); err == nil {
		t.Error("an unknown glyph passed")
	}
	if slices.Contains(Colors, "orange") || slices.Contains(Colors, "red") {
		t.Error("orange and red are never icon colours")
	}
}

func TestIconDefaultsFromTheName(t *testing.T) {
	glyphs, colors := map[string]bool{}, map[string]bool{}
	for _, name := range []string{"google", "github", "bank", "mail", "shop", "work", "notes", "cloud", "x", "y", "z", "w"} {
		g, c := Icon(New(name))
		if !slices.Contains(Glyphs, g) || !slices.Contains(Colors, c) {
			t.Fatalf("%s: glyph %q, colour %q", name, g, c)
		}
		if g2, c2 := Icon(New(name)); g2 != g || c2 != c {
			t.Fatalf("%s: the default changed between calls", name)
		}
		glyphs[g], colors[c] = true, true
	}
	if len(glyphs) < 3 || len(colors) < 3 {
		t.Fatalf("defaults barely vary: %v, %v", glyphs, colors)
	}
	p := New("google")
	p.IconGlyph, p.IconColor = "bank", "teal"
	if p.Glyph() != "bank" || p.Color() != "teal" {
		t.Fatalf("chosen icon = %s %s, want bank teal", p.Glyph(), p.Color())
	}
	p.IconGlyph = ""
	if g, _ := Icon(New("google")); p.Glyph() != g || p.Color() != "teal" {
		t.Fatalf("half chosen = %s %s", p.Glyph(), p.Color())
	}
}

func TestEffectiveSite(t *testing.T) {
	p := New("p")
	p.SiteInferred = "google.com"
	if p.EffectiveSite() != "google.com" {
		t.Fatalf("got %q", p.EffectiveSite())
	}
	p.Site = "mail.google.com"
	if p.EffectiveSite() != "mail.google.com" {
		t.Fatalf("the site set didn't win: %q", p.EffectiveSite())
	}
}

func TestStoreKeepsSiteAndIcon(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := New("mail")
	p.Site, p.IconGlyph, p.IconColor = "mail.google.com", "envelope", "sky"
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSiteInferred(ctx, "mail", "google.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSiteInferred(ctx, "mail", "not a site"); err == nil {
		t.Fatal("SetSiteInferred took a non-hostname")
	}
	got, _ := s.Get(ctx, "mail")
	if got.Site != "mail.google.com" || got.SiteInferred != "google.com" || got.IconGlyph != "envelope" || got.IconColor != "sky" {
		t.Fatalf("got %+v", got)
	}

	// The UI saves the whole profile it read: the inferred site isn't its
	// to change.
	got.Site, got.SiteInferred, got.IconGlyph, got.IconColor = "", "evil.example", "", "violet"
	updated, err := s.Update(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Site != "" || updated.SiteInferred != "google.com" || updated.IconGlyph != "" || updated.IconColor != "violet" {
		t.Fatalf("after Update: %+v", updated)
	}

	p.Site = "Not A Host"
	if _, err := s.Create(ctx, New("other")); err != nil {
		t.Fatal(err)
	}
	p.Name = "other"
	if _, err := s.Update(ctx, p); err == nil {
		t.Fatal("Update took an invalid site")
	}
}

func TestDropInvalidIcon(t *testing.T) {
	p := New("p")
	p.Site, p.SiteInferred, p.IconGlyph, p.IconColor = "Bad Site", "google.com", "unicorn", "teal"
	p.DropInvalidIcon()
	if p.Site != "" || p.SiteInferred != "google.com" || p.IconGlyph != "" || p.IconColor != "teal" {
		t.Fatalf("got %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}
