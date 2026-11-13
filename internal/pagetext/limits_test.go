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
	shapes := map[string]*Node{}
	for _, tag := range []string{"em", "a", "code", "blockquote", "span", "div", "ul", "li", "td", "strong"} {
		shapes[tag+" × 200,000 deep"] = chain(tag, 200000, tx("leaf text"))
	}
	var chains []*Node
	for range 10 {
		chains = append(chains, chain("em", 19990, tx(" ")))
	}
	shapes["10 em chains × 19,990"] = el("div", chains...)
	cur := el("p", tx("x"))
	for range 40000 {
		cur = el("table", el("tbody", el("tr", el("td", cur), el("td", tx("y")))))
	}
	shapes["40,000 nested tables"] = cur
	var ps []*Node
	for range 100000 {
		ps = append(ps, el("p", tx("word, word, word")))
	}
	shapes["100,000 paragraphs"] = el("div", ps...)
	var rows []*Node
	for range 50000 {
		rows = append(rows, el("tr", el("td", tx("a|b")), el("td", tx("c"))))
	}
	shapes["50,000 table rows"] = el("table", rows...)
	base, _ := url.Parse("https://x.example/")
	for name, body := range shapes {
		root := el("html", el("body", body))
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
	_ = fmt.Sprint()
}
