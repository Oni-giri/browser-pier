package pagetext

// Regression tests from PROJ-20's review: rendering stays linear in the
// tree and bounded in memory, readability survives ordinary class names,
// and page text can't reshape the output or hide part of it.

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/fence"
)

func el(tag string, kids ...*Node) *Node { return &Node{Tag: tag, Kids: kids} }
func tx(s string) *Node                  { return &Node{Text: s} }
func at(tag string, attrs map[string]string, kids ...*Node) *Node {
	return &Node{Tag: tag, Attrs: attrs, Kids: kids}
}

func chain(tag string, depth int, leaf *Node) *Node {
	n := leaf
	for range depth {
		n = el(tag, n)
	}
	return n
}

// TestRenderingIsLinear renders trees of 200,000 nodes in shapes that
// used to take seconds (deep inline chains, nested quotes and tables) or
// that are merely wide; each must take well under a second.
func TestRenderingIsLinear(t *testing.T) {
	limit := time.Second
	if raceEnabled {
		limit = 5 * time.Second // the race detector slows everything ~5×
	}
	// Each shape is built only when it is rendered, so one big tree is in
	// memory at a time (the machine running the tests is shared).
	shapes := map[string]func() *Node{}
	for _, tag := range []string{"em", "a", "code", "blockquote", "span", "div", "ul", "li", "td", "strong"} {
		shapes[tag+" × 200,000 deep"] = func() *Node { return chain(tag, 200000, tx("leaf text")) }
	}
	shapes["10 em chains × 19,990"] = func() *Node {
		var chains []*Node
		for range 10 {
			chains = append(chains, chain("em", 19990, tx(" ")))
		}
		return el("div", chains...)
	}
	shapes["40,000 nested tables"] = func() *Node {
		cur := el("p", tx("x"))
		for range 40000 {
			cur = el("table", el("tbody", el("tr", el("td", cur), el("td", tx("y")))))
		}
		return cur
	}
	shapes["100,000 paragraphs"] = func() *Node {
		var ps []*Node
		for range 100000 {
			ps = append(ps, el("p", tx("word, word, word")))
		}
		return el("div", ps...)
	}
	shapes["50,000 table rows"] = func() *Node {
		var rows []*Node
		for range 50000 {
			rows = append(rows, el("tr", el("td", tx("a|b")), el("td", tx("c"))))
		}
		return el("table", rows...)
	}
	base, _ := url.Parse("https://x.example/")
	for name, build := range shapes {
		root := el("html", el("body", build()))
		start := time.Now()
		md, _, err := Markdown(context.Background(), root, base, 2<<20)
		mdTime := time.Since(start)
		start = time.Now()
		_, _, err2 := Readable(context.Background(), root, 2<<20)
		rdTime := time.Since(start)
		if err != nil || err2 != nil {
			t.Errorf("%s: %v %v", name, err, err2)
		}
		if mdTime > limit || rdTime > limit {
			t.Errorf("%s: markdown %v, readability %v", name, mdTime, rdTime)
		}
		if strings.Contains(name, "deep") && !strings.Contains(md, "leaf text") {
			t.Errorf("%s: the text at the bottom is lost:\n%.200s", name, md)
		}
		runtime.GC()
	}
}

func TestRenderingStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ps []*Node
	for range 10000 {
		ps = append(ps, el("p", tx("word")))
	}
	if _, _, err := Markdown(ctx, el("body", ps...), nil, 0); err == nil {
		t.Error("markdown ran on with its context cancelled")
	}
	if _, _, err := Readable(ctx, el("body", ps...), 0); err == nil {
		t.Error("readability ran on with its context cancelled")
	}
}

// TestRenderingMemoryIsBounded: a 30 MB text node renders the limit's
// worth of text, cut, without allocating many times its size.
func TestRenderingMemoryIsBounded(t *testing.T) {
	root := el("html", el("body", el("p", tx(strings.Repeat("a* ", 10<<20)))))
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	out, full, err := Markdown(context.Background(), root, nil, 2<<20)
	runtime.ReadMemStats(&m1)
	alloc := (m1.TotalAlloc - m0.TotalAlloc) >> 20
	if err != nil || !full || len(out) > 2<<20 || len(out) < 1<<20 {
		t.Errorf("30 MB text: %d bytes out, full %v, %v", len(out), full, err)
	}
	if alloc > 40 {
		t.Errorf("30 MB text: %d MB allocated", alloc)
	}
	// The same through a pre block and a table cell.
	for _, root := range []*Node{el("pre", tx(strings.Repeat("line\n", 6<<20))), el("table", el("tr", el("td", tx(strings.Repeat("x ", 15<<20))), el("td", tx("y"))))} {
		out, _, _ := Markdown(context.Background(), root, nil, 2<<20)
		if len(out) > 2<<20 {
			t.Errorf("%d bytes out of a 2 MB limit", len(out))
		}
	}
}

func TestReadableSurvivesOrdinaryClassNames(t *testing.T) {
	long := strings.Repeat("The committee met on Tuesday, and agreed on the budget, the hours and the plan. ", 8)
	cases := map[string]*Node{
		"a wrapper with a sidebar":       at("div", map[string]string{"class": "layout has-sidebar"}, el("article", el("h1", tx("Title")), el("p", tx(long)), el("p", tx(long)))),
		"a community page":               at("div", map[string]string{"id": "community-page"}, el("p", tx(long)), el("p", tx(long))),
		"social sharing on, around main": at("div", map[string]string{"class": "js-social-enabled"}, el("main", el("p", tx(long)))),
		"a remarkable section":           at("section", map[string]string{"class": "remarkable"}, el("p", tx(long))),
	}
	for name, n := range cases {
		out, _, _ := Readable(context.Background(), el("html", el("body", n)), 0)
		if !strings.Contains(out, "The committee met") {
			t.Errorf("%s: %q", name, out)
		}
	}
	// A real sidebar still goes.
	out, _, _ := Readable(context.Background(), el("body", el("p", tx(long)), el("p", tx(long)), at("div", map[string]string{"class": "sidebar"}, el("p", tx("SIDEBAR-TEXT")))), 0)
	if strings.Contains(out, "SIDEBAR-TEXT") {
		t.Errorf("the sidebar is in:\n%s", out)
	}
}

func TestPageTextCantReshapeTheOutput(t *testing.T) {
	base, _ := url.Parse("https://x.example/dir/")
	pre := func(s string) *Node { return &Node{Text: s, Pre: true} }
	render := func(n *Node) string {
		out, _, _ := Markdown(context.Background(), el("html", el("body", n)), base, 0)
		return out
	}
	// A comment opener would make the fence drop the rest of the text.
	out := render(el("div", el("p", tx("before <!-- start")), el("p", tx("secret middle")), el("code", tx("<!--")), el("pre", tx("x <!-- y"))))
	if fenced := fence.Wrap("page as Markdown", out); !strings.Contains(fenced, "secret middle") || strings.Contains(out, "<!--") {
		t.Errorf("a comment opener hides text:\n%s", fenced)
	}
	// Lines that would be fences, table rows, headings or lists.
	out = render(el("p", pre("~~~\nfake code\n| a | b |\n1. x\n# h\n> q\n- z")))
	for _, l := range strings.Split(out, "\n") {
		if l != "" && l != "fake code" && !strings.HasPrefix(l, `\`) && !strings.HasPrefix(l, "1\\.") {
			t.Errorf("line %q isn't escaped in:\n%s", l, out)
		}
	}
	// An iframe's address with line breaks stays on its line.
	out = render(&Node{Tag: "iframe", Attrs: map[string]string{"src": "x]\n# Fake heading\n[y"}})
	if strings.Count(out, "\n") != 0 || strings.Contains(out, "\n#") {
		t.Errorf("iframe: %q", out)
	}
	// A link with a user name reads as another site's: text only.
	out = render(at("a", map[string]string{"href": "https://good.example@evil.example/"}, tx("good")))
	if out != "good" {
		t.Errorf("userinfo link: %q", out)
	}
	// A card: a link around blocks keeps its address.
	out = render(at("a", map[string]string{"href": "/story"}, el("h2", tx("Headline")), el("p", tx("Summary."))))
	if !strings.Contains(out, "## Headline") || !strings.Contains(out, "<https://x.example/story>") {
		t.Errorf("card: %q", out)
	}
	// Nested emphasis and links close in order, and empty ones vanish.
	out = render(el("p", tx("a "), el("em", el("strong", tx("both"))), tx(" "), at("a", map[string]string{"href": "/x"}, el("em", tx("it"))), el("em"), el("strong", tx(" "))))
	if out != "a _**both**_ [_it_](https://x.example/x)" {
		t.Errorf("marks: %q", out)
	}
}

func TestDeepTreesKeepTheirTextInOrder(t *testing.T) {
	deep := chain("div", 1000, el("p", tx("bottom")))
	root := el("body", el("p", tx("top")), deep, el("p", tx("after")))
	out, _, _ := Markdown(context.Background(), root, nil, 0)
	if i, j, k := strings.Index(out, "top"), strings.Index(out, "bottom"), strings.Index(out, "after"); i < 0 || j < i || k < j {
		t.Errorf("order: %q", out)
	}
	// Where structure was dropped, a marker says so.
	if !strings.Contains(out, strings.TrimSpace(DeepNote)) {
		t.Errorf("no marker where the tree was flattened: %q", out)
	}
	if out, _, _ := Markdown(context.Background(), chain("div", 100, el("p", tx("shallow"))), nil, 0); strings.Contains(out, "BrowserPier") {
		t.Errorf("a marker on a shallow tree: %q", out)
	}
}

// timed renders root both ways and fails if either takes over limit
// (scaled under the race detector) or allocates over maxMB (without it).
func timed(t *testing.T, name string, root *Node, limit time.Duration, maxMB uint64) {
	t.Helper()
	if raceEnabled {
		limit *= 5
	}
	base, _ := url.Parse("https://x.example/")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	start := time.Now()
	_, _, err := Markdown(context.Background(), root, base, 2<<20)
	md := time.Since(start)
	start = time.Now()
	_, _, err2 := Readable(context.Background(), root, 2<<20)
	rd := time.Since(start)
	runtime.ReadMemStats(&m1)
	if err != nil || err2 != nil {
		t.Errorf("%s: %v %v", name, err, err2)
	}
	if md > limit || rd > limit {
		t.Errorf("%s: markdown %v, readability %v", name, md, rd)
	}
	if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; !raceEnabled && mb > maxMB {
		t.Errorf("%s: %d MB allocated", name, mb)
	}
}

// TestSharedStringsStayCheap: the DOM snapshot shares one string among
// every node that has it, so 20,000 nodes can carry the same megabyte of
// text, class names or alt text. The work stays linear in the nodes.
func TestSharedStringsStayCheap(t *testing.T) {
	ws := strings.Repeat(" ", 1<<20)
	cls := strings.Repeat("abcdefg hijkl ", 10000) // 140 KB
	var texts, divs, imgs, pres []*Node
	for range 20000 {
		texts = append(texts, el("span", tx(ws)))
		divs = append(divs, at("div", map[string]string{"class": cls}, tx("x")))
		imgs = append(imgs, at("img", map[string]string{"alt": ws, "src": "/i"}))
		pres = append(pres, at("pre", map[string]string{"class": cls}, tx("x")))
	}
	timed(t, "20,000 spans sharing 1 MB of spaces", el("html", el("body", texts...)), time.Second, 200)
	timed(t, "20,000 divs sharing a 140 KB class", el("html", el("body", divs...)), time.Second, 200)
	timed(t, "20,000 images sharing a 1 MB blank alt", el("html", el("body", imgs...)), time.Second, 200)
	timed(t, "20,000 pre sharing a 140 KB class", el("html", el("body", pres...)), time.Second, 200)
	timed(t, "one image with a 30 MB alt", el("html", el("body", at("img", map[string]string{"alt": strings.Repeat("a ", 15<<20), "src": "/i"}))), time.Second, 40)
	// Text past the budget ends with a note saying so.
	var big []*Node
	for range 20 {
		big = append(big, el("p", tx(strings.Repeat("word ", 1<<20))))
	}
	out, _, _ := Plain(context.Background(), el("body", big...), 0)
	if !strings.HasSuffix(out, TruncatedNote) {
		t.Errorf("no note at the end of a page past the budget: %q", out[max(0, len(out)-200):])
	}
}

// TestRenderingShapesFromTheReview: wide, deep and mixed shapes.
func TestRenderingShapesFromTheReview(t *testing.T) {
	timed(t, "400 × 250-deep mixed", func() *Node {
		var subs []*Node
		for range 400 {
			cur := tx("leaf, text")
			for d := range 250 {
				tag := []string{"em", "blockquote", "ul", "li", "a", "table", "tr", "td", "strong", "div"}[d%10]
				cur = el(tag, cur, tx("x"))
			}
			subs = append(subs, cur)
		}
		return el("html", el("body", subs...))
	}(), time.Second, 400)
	many := func(n int, f func() []*Node) []*Node {
		var out []*Node
		for range n {
			out = append(out, f()...)
		}
		return out
	}
	timed(t, "250 quotes around 100,000 paragraphs", el("html", el("body", chain("blockquote", 250, el("div", many(100000, func() []*Node { return []*Node{el("p", tx("w"))} })...)))), time.Second, 400)
	timed(t, "250 em around 200,000 blank texts", el("html", el("body", chain("em", 250, el("span", many(100000, func() []*Node { return []*Node{el("span", tx(" ")), tx(" ")} })...)))), time.Second, 400)
	timed(t, "100,000 links", el("html", el("body", el("p", many(100000, func() []*Node {
		return []*Node{at("a", map[string]string{"href": "/x?" + strings.Repeat("q", 300)}, tx("l"))}
	})...))), time.Second, 400)
	timed(t, "a 100,000-cell row and 20,000 short ones", el("html", el("body", el("table", append([]*Node{el("tr", many(100000, func() []*Node { return []*Node{el("td")} })...)},
		many(20000, func() []*Node { return []*Node{el("tr", el("td"))} })...)...))), time.Second, 400)
}

// TestReadableLeavesOutCommentsAndFooters: the article, never the comments,
// footers, menus or banners around it, however their classes are written.
func TestReadableLeavesOutCommentsAndFooters(t *testing.T) {
	long := strings.Repeat("The committee met on Tuesday, and agreed on the budget, the hours and the plan. ", 6)
	c := func(cls string, kids ...*Node) *Node { return at("div", map[string]string{"class": cls}, kids...) }
	var comments, wpComments, navl []*Node
	for i := range 60 {
		comments = append(comments, c("comment", el("p", tx(fmt.Sprintf("COMMENT-%d I disagree, frankly, with all of this, and so on and on.", i)))))
		wpComments = append(wpComments, at("article", map[string]string{"class": "comment-body"}, el("p", tx(fmt.Sprintf("COMMENT-%d I disagree, frankly, with all of this, and so on and on.", i)))))
	}
	for i := range 80 {
		navl = append(navl, el("li", at("a", map[string]string{"href": "/s"}, tx(fmt.Sprintf("NAV-LINK section number %d", i)))))
	}
	post := c("post", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx(long)))
	for _, tc := range []struct {
		name string
		n    *Node
	}{
		{"a post and its comments", el("body", post, at("div", map[string]string{"id": "comments"}, comments...))},
		{"an article and WordPress comment articles", el("body", el("article", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx(long))), at("section", map[string]string{"id": "comments", "class": "comments-area"}, wpComments...))},
		{"a short article and comments", el("body", el("article", el("h1", tx("POST-TITLE")), el("p", tx("A short post, of one line."))), at("div", map[string]string{"id": "comments"}, comments...))},
		{"a mega menu and a site footer", el("body", c("mega-menu", el("ul", navl...)), post, c("site-footer", el("p", tx("FOOTER-TEXT "+long+long+long+long+long))))},
		{"a short page and a cookie banner", el("body", el("main", el("h1", tx("POST-TITLE")), el("p", tx("Short product page, one line."))), c("cookie-banner", el("p", tx("COOKIE-TEXT "+long))))},
		{"underscores and camelCase", el("body", post, c("cookie_notice", el("p", tx("COOKIE-TEXT "+long))), at("div", map[string]string{"id": "disqus_thread"}, comments[:5]...), c("siteFooter", el("p", tx("FOOTER-TEXT "+long))))},
	} {
		out, _, _ := Readable(context.Background(), el("html", tc.n), 0)
		if !strings.Contains(out, "POST-TITLE") {
			t.Errorf("%s: no title:\n%.300s", tc.name, out)
		}
		for _, bad := range []string{"COMMENT-3 ", "NAV-LINK", "FOOTER-TEXT", "COOKIE-TEXT"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %s is in:\n%.300s", tc.name, bad, out)
			}
		}
	}
}
