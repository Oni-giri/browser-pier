package ui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// writeIcon caches a 64×64 icon for the profile, as a stop would.
func (f *fixture) writeIcon(name string) []byte {
	f.t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.m.Layout().IconFile(name), b.Bytes(), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return b.Bytes()
}

func TestIconIsServedToTheAdmin(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	if err := os.MkdirAll(f.m.Layout().ProfileDir("google"), 0o700); err != nil {
		t.Fatal(err)
	}
	ui := &UI{m: f.m}
	if ui.IconTag("google") != "" || ui.IconURL("google") != "" {
		t.Fatal("a tag for no icon")
	}
	if r := f.get("/profiles/google/icon"); r.code != http.StatusNotFound {
		t.Fatalf("no icon yet: %d", r.code)
	}

	png := f.writeIcon("google")
	tag := ui.IconTag("google")
	if tag == "" || ui.IconURL("google") != "/profiles/google/icon?v="+tag {
		t.Fatalf("tag %q, url %q", tag, ui.IconURL("google"))
	}
	r := f.get(ui.IconURL("google"))
	if r.code != http.StatusOK || r.body != string(png) {
		t.Fatalf("icon: %d, %d bytes", r.code, len(r.body))
	}
	for k, v := range map[string]string{
		"Content-Type":           "image/png",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, max-age=3600",
		"ETag":                   `"` + tag + `"`,
	} {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if r := f.get("/profiles/google/icon", "If-None-Match", `"`+tag+`"`); r.code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, want 304", r.code)
	}

	// A new icon gets a new tag.
	var b bytes.Buffer
	_ = pngEncode(&b, 64)
	if err := os.WriteFile(f.m.Layout().IconFile("google"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if ui.IconTag("google") == tag {
		t.Fatal("the tag didn't change with the icon")
	}

	if r := f.get("/profiles/nobody/icon"); r.code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", r.code)
	}
	if r := f.get("/profiles/..%2F..%2Fetc/icon"); r.code != http.StatusNotFound {
		t.Fatalf("a path for a name: %d", r.code)
	}
}

func pngEncode(b *bytes.Buffer, side int) error {
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	img.Pix[3] = 255
	return png.Encode(b, img)
}

func TestIconNeedsTheAdminSession(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	if err := os.MkdirAll(f.m.Layout().ProfileDir("google"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeIcon("google")
	_, token, err := f.tokens.Create(context.Background(), "research-bot", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, header := range map[string][]string{
		"signed out":  nil,
		"agent token": {"Authorization", "Bearer " + token},
	} {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/profiles/google/icon", nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.Header.Get("Content-Type") == "image/png" {
			t.Errorf("%s: got the icon (%d)", name, resp.StatusCode)
		}
	}
}

func TestFormSavesSiteAndIcon(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.post("/profiles", url.Values{"name": {"mail"}, "site": {"https://Mail.Google.com/mail/u/0/"},
		"icon_glyph": {"envelope"}, "icon_color": {"sky"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("create: %d %.300s", r.code, r.body)
	}
	p, err := f.m.Profiles().Get(ctx, "mail")
	if err != nil || p.Site != "mail.google.com" || p.IconGlyph != "envelope" || p.IconColor != "sky" {
		t.Fatalf("created %+v, %v", p, err)
	}

	// A form without the fields (today's templates) keeps them.
	if r := f.post("/profiles/mail/edit", url.Values{"mode": {"api"}}); r.code != http.StatusSeeOther {
		t.Fatalf("edit: %d %.300s", r.code, r.body)
	}
	if p, _ := f.m.Profiles().Get(ctx, "mail"); p.Site != "mail.google.com" || p.IconGlyph != "envelope" || p.DefaultMode != profiles.ModeAPI {
		t.Fatalf("after an edit without them: %+v", p)
	}

	// Cleared: back to the default icon and no site.
	if r := f.post("/profiles/mail/edit", url.Values{"site": {""}, "icon_glyph": {""}, "icon_color": {""}}); r.code != http.StatusSeeOther {
		t.Fatalf("clear: %d", r.code)
	}
	if p, _ := f.m.Profiles().Get(ctx, "mail"); p.Site != "" || p.IconGlyph != "" || p.IconColor != "" {
		t.Fatalf("after clearing: %+v", p)
	}

	for field, value := range map[string]string{"site": "not a site", "icon_glyph": "<svg onload=x>", "icon_color": "orange"} {
		r := f.post("/profiles/mail/edit", url.Values{field: {value}})
		if r.code != http.StatusUnprocessableEntity {
			t.Errorf("%s %q: %d, want 422", field, value, r.code)
		}
		if strings.Contains(r.body, "<svg onload=x>") {
			t.Errorf("%s: echoed unescaped", field)
		}
	}
}

func TestProfilesShowTheirIcon(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"google", "notion"} {
		f.profile(name)
	}
	ctx := context.Background()
	if _, _, err := f.m.Start(ctx, "google", ""); err != nil {
		t.Fatal(err)
	}
	f.writeIcon("google")
	if _, _, err := f.tokens.Create(ctx, "research-bot", true, nil); err != nil {
		t.Fatal(err)
	}
	notion, _ := f.m.Profiles().Get(ctx, "notion")
	fallback := fmt.Sprintf(`<span class="icon c-%s"><svg><use href="#g-%s"/>`, notion.Color(), notion.Glyph())
	for _, path := range []string{"/", "/profiles", "/profiles?view=table"} {
		r := f.get(path)
		want(t, r, http.StatusOK, `<span class="icon fav"><img src="/profiles/google/icon?v=`, fallback, `<symbol id="g-envelope"`)
		if strings.Contains(r.body, `class="initial"`) {
			t.Errorf("%s: a profile still has initials", path)
		}
	}
	want(t, f.get("/profiles/google"), http.StatusOK, `<span class="icon fav"><img src="/profiles/google/icon?v=`)
	want(t, f.get("/profiles/notion"), http.StatusOK, fallback)
	// Agents keep their initials.
	want(t, f.get("/agents"), http.StatusOK, `>RB</span>`)
	// The picker offers every glyph and colour, never orange or red.
	r := f.get("/profiles/notion/edit")
	for _, g := range profiles.Glyphs {
		want(t, r, http.StatusOK, `name="icon_glyph" value="`+g+`"`)
	}
	for _, c := range profiles.Colors {
		want(t, r, http.StatusOK, `name="icon_color" value="`+c+`"`)
	}
	if strings.Contains(r.body, `value="orange"`) || strings.Contains(r.body, `value="red"`) {
		t.Fatal("the picker offers a warm colour")
	}
	want(t, r, http.StatusOK, `id="icon-preview"`, `name="site"`)
}
