package ui

import (
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/theme"
)

// The blue, sky and orange lights and the icon hues are the same in both
// themes; every other colour is a token, so dark mode can't miss one.
var (
	colourLiteral = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|(?:rgba?|hsla?)\([^)]*\)`)
	accentLight   = regexp.MustCompile(`^rgba\((47,107,255|56,189,248|255,106,61),[.0-9]+\)$`)
	iconHue       = regexp.MustCompile(`--hue:#[0-9A-F]{6}\}`)
)

func TestCSSColoursAreTokens(t *testing.T) {
	b, err := embedded.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, "*{box-sizing:border-box}")
	if i < 0 {
		t.Fatal("app.css: can't find where the tokens end")
	}
	line := strings.Count(css[:i], "\n") + 1
	for _, l := range strings.Split(css[i:], "\n") {
		for _, loc := range colourLiteral.FindAllStringIndex(l, -1) {
			c := l[loc[0]:loc[1]]
			if accentLight.MatchString(c) || iconHue.MatchString(l[max(loc[0]-6, 0):min(loc[1]+1, len(l))]) {
				continue
			}
			t.Errorf("app.css:%d: %s isn't a token: add it to :root as light-dark(light, dark)", line, c)
		}
		line++
	}
}

// Inline styles in templates use tokens too: the guard above can't see them.
func TestTemplateStylesAreTokens(t *testing.T) {
	inline := regexp.MustCompile(`style="[^"]*"`)
	files, err := fs.Glob(embedded, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatal("no templates", err)
	}
	for _, name := range files {
		b, err := embedded.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range inline.FindAllString(string(b), -1) {
			if colourLiteral.MatchString(s) {
				t.Errorf("%s: %s: use a token from app.css", name, s)
			}
		}
	}
}

// Orange means a person is needed (DESIGN.md). A notice is a note or a
// caution in a form or a dialog, never a request for a person, so no
// template marks one warm and no rule for a notice reaches for orange.
func TestNoticesAreNeverOrange(t *testing.T) {
	classAttr := regexp.MustCompile(`class="([^"]*)"`)
	files, err := fs.Glob(embedded, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatal("no templates", err)
	}
	for _, name := range files {
		b, err := embedded.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range classAttr.FindAllStringSubmatch(string(b), -1) {
			classes := strings.Fields(m[1])
			if slices.Contains(classes, "notice") && slices.Contains(classes, "warn") {
				t.Errorf("%s: %s: a notice is neutral; orange is only for a person needed", name, m[0])
			}
		}
	}
	b, err := embedded.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range strings.Split(string(b), "}") {
		sel, decl, ok := strings.Cut(rule, "{")
		if ok && strings.Contains(sel, ".notice") && strings.Contains(decl, "orange") {
			t.Errorf("app.css: %s{%s}: a notice is never orange", strings.TrimSpace(sel), decl)
		}
	}
}

func (f *fixture) setTheme(v string) {
	u, _ := url.Parse(f.srv.URL)
	f.client.Jar.SetCookies(u, []*http.Cookie{{Name: theme.Cookie, Value: v, Path: "/"}})
}

func TestThemeIsOnThePageFromTheStart(t *testing.T) {
	f := newFixture(t)
	want(t, f.get("/settings"), http.StatusOK, `<html lang="en">`, `<meta name="color-scheme" content="light dark">`, `media="(prefers-color-scheme: dark)"`, `value="system" checked`)
	f.setTheme("dark")
	want(t, f.get("/settings"), http.StatusOK, `<html lang="en" data-theme="dark">`, `<meta name="color-scheme" content="dark">`, `<meta name="theme-color" content="#070C18">`, `value="dark" checked`)
	f.setTheme("light")
	want(t, f.get("/"), http.StatusOK, `<html lang="en" data-theme="light">`, `<meta name="color-scheme" content="light">`)
	f.setTheme("blue")
	if r := f.get("/"); !strings.Contains(r.body, `<html lang="en">`) || strings.Contains(r.body, "blue") {
		t.Fatalf("a made-up theme is shown: %.300s", r.body)
	}
}

func TestAppearance(t *testing.T) {
	f := newFixture(t)
	htmx := []string{"HX-Request", "true"}
	if r := f.post("/settings/appearance", url.Values{"theme": {"dark"}}, htmx...); r.code != http.StatusNoContent || !strings.Contains(r.header.Get("Set-Cookie"), theme.Cookie+"=dark") {
		t.Fatalf("dark: %d %q", r.code, r.header.Get("Set-Cookie"))
	}
	want(t, f.get("/profiles"), http.StatusOK, `data-theme="dark"`)
	if r := f.post("/settings/appearance", url.Values{"theme": {"system"}}, htmx...); r.code != http.StatusNoContent {
		t.Fatalf("system: %d", r.code)
	}
	if r := f.get("/profiles"); strings.Contains(r.body, "data-theme") {
		t.Fatal("system still forces a theme")
	}
	// Without htmx, back to Settings with a toast.
	if r := f.post("/settings/appearance", url.Values{"theme": {"light"}}); r.code != http.StatusSeeOther || r.location != "/settings" {
		t.Fatalf("plain form: %d to %q", r.code, r.location)
	}
	want(t, f.get("/settings"), http.StatusOK, "Appearance saved", `data-theme="light"`)
	for _, v := range []string{"", "blue", "Dark"} {
		if r := f.post("/settings/appearance", url.Values{"theme": {v}}, htmx...); r.code != http.StatusBadRequest || r.header.Get("Set-Cookie") != "" {
			t.Errorf("%q: %d %q", v, r.code, r.header.Get("Set-Cookie"))
		}
	}
	if r := f.post("/settings/appearance", url.Values{"theme": {"dark"}, "pad": {strings.Repeat("x", 20<<10)}}, htmx...); r.code != http.StatusBadRequest {
		t.Errorf("oversized form: %d", r.code)
	}
	// Another site can't change it, and nor can someone signed out.
	if r := f.post("/settings/appearance", url.Values{"theme": {"dark"}}, "Sec-Fetch-Site", "cross-site"); r.code != http.StatusForbidden || r.header.Get("Set-Cookie") != "" {
		t.Errorf("cross-site: %d %q", r.code, r.header.Get("Set-Cookie"))
	}
	resp, err := http.PostForm(f.srv.URL+"/settings/appearance", url.Values{"theme": {"dark"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == theme.Cookie {
			t.Errorf("signed out: set %q", c.Value)
		}
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("signed out: %d", resp.StatusCode)
	}
}
