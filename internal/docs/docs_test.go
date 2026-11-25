package docs

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

const themeScript = `document.documentElement.dataset.theme = "light";`

// site is a small built site, laid out as Starlight builds one.
func site() fstest.MapFS {
	page := func(title, body string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`<!doctype html><html><head><script>` + themeScript + `</script>` +
			`<script type="module" src="/docs/_astro/page.js"></script></head><body><h1>` + title + `</h1>` + body + `</body></html>`)}
	}
	return fstest.MapFS{
		"index.html":                     page("BrowserPier docs", ""),
		"index.md":                       {Data: []byte("# BrowserPier docs\n\n> What BrowserPier is.\n\nIntro.\n")},
		"404.html":                       page("Not found", ""),
		"reference/sessions/index.html":  page("Sessions API", `<h2 id="start-or-attach-to-a-browser">Start or attach to a browser</h2>`),
		"reference/sessions.md":          {Data: []byte(sessionsMD)},
		"guides/handoff/index.html":      page("Hand a browser to a person", ""),
		"guides/handoff.md":              {Data: []byte(handoffMD)},
		"_astro/page.js":                 {Data: []byte("console.log(1)")},
		"llms.txt":                       {Data: []byte("# BrowserPier\n\n- [Sessions](https://browserpier.com/docs/reference/sessions.md)\n")},
		".hidden":                        {Data: []byte("secret")},
		"reference/empty/placeholder.js": {Data: []byte("")},
	}
}

const sessionsMD = "# Sessions API\n\n> Start, list and stop browsers.\n\nA session is a running browser.\n\n" +
	"## Start or attach to a browser\n\n`POST /api/sessions`\n\n```sh\n## not a heading: in a code block\ncurl …\n```\n\n" +
	"### Errors\n\n| 404 | `no profile named \"github\"` |\n\n" +
	"## Stop a browser\n\n`DELETE /api/sessions/{id}` keeps the logins.\n"

const handoffMD = "# Hand a browser to a person\n\n> Ask a person to take over.\n\n" +
	"## 1. Ask for a handoff\n\nSend a reason. A session must be running.\n\n" +
	"## Hand back by itself when the page gets there\n\nGive `until_url`.\n"

func TestPages(t *testing.T) {
	d := New(site())
	if !d.Built() {
		t.Fatal("not built")
	}
	var got []string
	for _, p := range d.Pages() {
		got = append(got, p.Path+": "+p.Title+" / "+p.Description)
	}
	want := []string{
		"index: BrowserPier docs / What BrowserPier is.",
		"guides/handoff: Hand a browser to a person / Ask a person to take over.",
		"reference/sessions: Sessions API / Start, list and stop browsers.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, name := range []string{
		"reference/sessions", "/reference/sessions/", "reference/sessions.md", "/docs/reference/sessions/",
		"reference/sessions#errors", "https://browserpier.com/docs/reference/sessions/", " reference/sessions/index ",
	} {
		if p, ok := d.Page(name); !ok || p.Path != "reference/sessions" {
			t.Errorf("Page(%q) = %q, %v", name, p.Path, ok)
		}
	}
	for _, name := range []string{"", "/", "/docs/", "index", "index.md"} {
		if p, ok := d.Page(name); !ok || p.Path != "index" {
			t.Errorf("Page(%q) = %q, %v; want the home page", name, p.Path, ok)
		}
	}
	for _, name := range []string{"reference", "../etc/passwd", "llms.txt", "nothing"} {
		if _, ok := d.Page(name); ok {
			t.Errorf("Page(%q) found a page", name)
		}
	}
}

func TestNotBuilt(t *testing.T) {
	d := New(fstest.MapFS{".gitkeep": {}})
	if d.Built() || len(d.Pages()) != 0 {
		t.Fatal("an empty site counts as built")
	}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	for path, code := range map[string]int{"/": 200, "/reference/sessions/": 404} {
		res, body := get(t, srv.URL+path)
		if res.StatusCode != code || !strings.Contains(body, Online) {
			t.Errorf("%s: %d, want %d with a link to %s", path, res.StatusCode, code, Online)
		}
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
}

func TestSearch(t *testing.T) {
	d := New(site())
	hits := d.Search("stop browser", 10)
	if len(hits) == 0 || hits[0].Heading != "Stop a browser" || hits[0].Anchor != "stop-a-browser" {
		t.Fatalf("first hit: %+v", hits)
	}
	if !strings.HasPrefix(hits[0].Text, "## Stop a browser") || !strings.Contains(hits[0].Text, "keeps the logins") {
		t.Fatalf("hit text: %q", hits[0].Text)
	}
	// An error message finds the section that lists it.
	hits = d.Search(`no profile named "github"`, 3)
	if len(hits) == 0 || hits[0].Heading != "Errors" || hits[0].Page.Path != "reference/sessions" {
		t.Fatalf("error search: %+v", hits)
	}
	// A heading in a code block doesn't split a section.
	for _, h := range d.Search("curl", 10) {
		if h.Heading == "not a heading: in a code block" {
			t.Fatal("a code block's line was taken for a heading")
		}
	}
	if h := d.Search("until_url", 1); len(h) != 1 || h[0].Anchor != "hand-back-by-itself-when-the-page-gets-there" {
		t.Fatalf("until_url: %+v", h)
	}
	if h := d.Search("Ask for a handoff", 1); len(h) != 1 || h[0].Anchor != "1-ask-for-a-handoff" {
		t.Fatalf("numbered heading: %+v", h)
	}
	if d.Search("zzzz qqqq", 5) != nil || d.Search(" ", 5) != nil {
		t.Fatal("hits for nothing")
	}
}

func TestHandler(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("/docs", New(site()).Handler()))
	defer srv.Close()

	res, body := get(t, srv.URL+"/docs/reference/sessions/")
	if res.StatusCode != 200 || !strings.Contains(body, "<h1>Sessions API</h1>") {
		t.Fatalf("page: %d %q", res.StatusCode, body)
	}
	sum := sha256.Sum256([]byte(themeScript))
	hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'wasm-unsafe-eval' "+hash) || strings.Contains(csp, "'unsafe-inline' 'wasm") {
		t.Fatalf("CSP %q, want the inline script's hash %s", csp, hash)
	}
	if strings.Count(csp, "sha256-") != 1 {
		t.Fatalf("CSP %q: only the inline script, once", csp)
	}
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "private, no-cache",
		"Content-Type":           "text/html; charset=utf-8",
	} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	res, body = get(t, srv.URL+"/docs/reference/sessions.md")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || !strings.HasPrefix(body, "# Sessions API") {
		t.Fatalf("markdown: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	// llms.txt links to this server's copy, not the online docs.
	if res, body := get(t, srv.URL+"/docs/llms.txt"); res.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		!strings.Contains(body, "(/docs/reference/sessions.md)") || strings.Contains(body, "browserpier.com") {
		t.Fatalf("llms.txt: %q %q", res.Header.Get("Content-Type"), body)
	}
	if res, _ := get(t, srv.URL+"/docs/_astro/page.js"); res.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Fatalf("asset cache: %q", res.Header.Get("Cache-Control"))
	}
	if res, body := get(t, srv.URL+"/docs/"); res.StatusCode != 200 || !strings.Contains(body, "<h1>BrowserPier docs</h1>") {
		t.Fatalf("home: %d", res.StatusCode)
	}

	// Missing, hidden and traversing paths get the site's 404 page.
	for _, path := range []string{"/docs/nothing/", "/docs/.hidden", "/docs/../docs.go", "/docs/%2e%2e/docs.go", "/docs/reference/empty/"} {
		res, body := get(t, srv.URL+path)
		if res.StatusCode != 404 || !strings.Contains(body, "<h1>Not found</h1>") {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
	}

	// A folder without its slash goes to the one with it.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirect, err := c.Get(srv.URL + "/docs/reference/sessions") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	_ = redirect.Body.Close()
	if redirect.StatusCode != http.StatusMovedPermanently || redirect.Header.Get("Location") != "sessions/" {
		t.Fatalf("no slash: %d %q", redirect.StatusCode, redirect.Header.Get("Location"))
	}
}

// TestBuiltSite checks the docs this binary was built with, when it was
// (make docs; CI's site workflow): every page has a title and a
// description, and every heading search links to is an id on its page.
func TestBuiltSite(t *testing.T) {
	d := Embedded()
	if !d.Built() {
		t.Skip("built without the docs; make docs builds them")
	}
	ids := regexp.MustCompile(`\sid="([^"]+)"`)
	for _, p := range d.Pages() {
		if p.Title == p.Path || p.Description == "" {
			t.Errorf("%s: title %q, description %q", p.Path, p.Title, p.Description)
		}
		file := p.Path + "/index.html"
		if p.Path == "index" {
			file = "index.html"
		}
		html, err := fsRead(d, file)
		if err != nil {
			t.Errorf("%s: %v", p.Path, err)
			continue
		}
		have := map[string]bool{}
		for _, m := range ids.FindAllStringSubmatch(html, -1) {
			have[m[1]] = true
		}
		for _, sec := range sections(p) {
			if sec.Anchor != "" && !have[sec.Anchor] {
				t.Errorf("%s: no id %q for %q", p.Path, sec.Anchor, sec.Heading)
			}
		}
		if !strings.Contains(d.csp[file], "sha256-") {
			t.Errorf("%s: no inline script hashes in its CSP", file)
		}
	}
}

type reply struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, url string) (reply, string) {
	t.Helper()
	res, err := http.Get(url) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{res.StatusCode, res.Header}, string(b)
}

func fsRead(d *Docs, name string) (string, error) {
	b, err := fs.ReadFile(d.fsys, name)
	return string(b), err
}
