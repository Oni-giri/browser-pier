// Package docs holds BrowserPier's docs site, built from site/ and embedded,
// so each binary has its own version's docs: the admin reads them at
// /docs/, and agents through MCP's read_docs.
//
// `make docs` (or the image build) builds the site into site/ here. A
// binary built without it has no docs and says where to read them online.
package docs

import (
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"
)

//go:embed all:site
var embedded embed.FS

// Online is where the latest docs are, for binaries built without them.
const Online = "https://browserpier.com/docs/"

// Docs is a built docs site.
type Docs struct {
	fsys  fs.FS
	pages []Page
	csp   map[string]string // each HTML file's Content-Security-Policy
}

// Page is one docs page, as Markdown.
type Page struct {
	Path        string // such as "reference/sessions"; "index" for the home page
	Title       string
	Description string
	Markdown    string // the whole page, title and description included
}

// Embedded returns the docs built into this binary, read once.
var Embedded = sync.OnceValue(func() *Docs {
	sub, err := fs.Sub(embedded, "site")
	if err != nil {
		panic(err) // the directive guarantees the folder
	}
	return New(sub)
})

// New reads a built site: its pages as Markdown (each page's .md file,
// next to its folder), and its HTML files' inline scripts for the CSP.
func New(fsys fs.FS) *Docs {
	d := &Docs{fsys: fsys, csp: map[string]string{}}
	_ = fs.WalkDir(fsys, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is left out
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil //nolint:nilerr // as above
		}
		switch path.Ext(name) {
		case ".md":
			d.pages = append(d.pages, parsePage(strings.TrimSuffix(name, ".md"), string(b)))
		case ".html":
			d.csp[name] = htmlCSP(string(b))
		}
		return nil
	})
	sort.Slice(d.pages, func(i, j int) bool {
		// The home page first, then by path: guides, then reference.
		if (d.pages[i].Path == "index") != (d.pages[j].Path == "index") {
			return d.pages[i].Path == "index"
		}
		return d.pages[i].Path < d.pages[j].Path
	})
	return d
}

// Built says whether this binary has its docs.
func (d *Docs) Built() bool {
	_, err := fs.Stat(d.fsys, "index.html")
	return err == nil && len(d.pages) > 0
}

// Pages lists every page, the home page first.
func (d *Docs) Pages() []Page { return d.pages }

// Page finds a page by its path, as Pages gives it or as a link has it:
// "reference/sessions", "/docs/reference/sessions/", "reference/sessions.md"
// and "reference/sessions#errors" all name the same page.
func (d *Docs) Page(name string) (Page, bool) {
	name = Normalize(name)
	for _, p := range d.pages {
		if p.Path == name {
			return p, true
		}
	}
	return Page{}, false
}

// Normalize turns a page's link or path into its path in Pages.
func Normalize(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.IndexAny(name, "#?"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, strings.TrimSuffix(Online, "/"))
	name = strings.Trim(name, "/")
	name = strings.TrimPrefix(name, "docs/")
	if name == "docs" {
		name = ""
	}
	name = strings.TrimSuffix(name, ".md")
	name = strings.TrimSuffix(name, "/index")
	if name == "" {
		return "index"
	}
	return name
}

// parsePage reads a page's Markdown: "# Title", then the description as a
// "> " quote, then the body (see site/src/pages/[...slug].md.ts).
func parsePage(name, md string) Page {
	p := Page{Path: name, Markdown: md}
	for line := range strings.Lines(md) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if p.Title == "" && strings.HasPrefix(line, "# ") {
			p.Title = strings.TrimSpace(line[2:])
			continue
		}
		if p.Title != "" && strings.HasPrefix(line, "> ") {
			p.Description = strings.TrimSpace(line[2:])
		}
		break
	}
	if p.Title == "" {
		p.Title = name
	}
	return p
}

// Hit is one section of a page that matches a search.
type Hit struct {
	Page    Page
	Heading string // "" for the part before the first heading
	Anchor  string // the heading's id on the HTML page
	Text    string // the section's Markdown, heading included
	score   int
	terms   int
}

// Search finds the sections (between ## or ### headings) that contain the
// query's words, best first: those with the most of its words, then with
// them in their heading, then the most often.
func (d *Docs) Search(query string, limit int) []Hit {
	terms := words(query)
	if len(terms) == 0 {
		return nil
	}
	var hits []Hit
	for _, p := range d.pages {
		for _, sec := range sections(p) {
			body, head := strings.ToLower(sec.Text), strings.ToLower(sec.Heading)
			for _, t := range terms {
				n := strings.Count(body, t)
				if n == 0 {
					continue
				}
				sec.terms++
				sec.score += min(n, 5)
				if strings.Contains(head, t) {
					sec.score += 10
				}
			}
			if sec.terms > 0 {
				hits = append(hits, sec)
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].terms != hits[j].terms {
			return hits[i].terms > hits[j].terms
		}
		return hits[i].score > hits[j].score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// words are a query's words, lower case, without the very short ones.
func words(q string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '/' && r != '.'
	}) {
		w = strings.Trim(w, "-/.")
		if len(w) < 2 || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// sections splits a page at its ## and ### headings, outside code blocks.
func sections(p Page) []Hit {
	var out []Hit
	cur := Hit{Page: p}
	var b strings.Builder
	fenced := false
	flush := func() {
		cur.Text = strings.TrimSpace(b.String())
		if cur.Text != "" {
			out = append(out, cur)
		}
		b.Reset()
	}
	for line := range strings.Lines(p.Markdown) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
		}
		if !fenced && (strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ")) {
			flush()
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			cur = Hit{Page: p, Heading: heading, Anchor: slug(heading)}
		}
		b.WriteString(line)
	}
	flush()
	return out
}

// slug is a heading's id as Starlight makes it (github-slugger): lower
// case, punctuation dropped, spaces to hyphens.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}
