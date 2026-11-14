package pagetools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The DOM snapshot keeps one copy of each string, so a page can give
// thousands of nodes the same long string (a megabyte of spaces as text,
// a megabyte value or name attribute) in a small answer. Work per node
// must not grow with that string's length: before the fix, 500 fields
// sharing a 1 MB value and attributes took 52 s here, and 5,000 ran out
// of memory.

const sharedLen = 1 << 20

// manyNodes is a document of n copies of node(i), under a block body.
func manyNodes(n int, node func(i int) *fnode) *fnode {
	kids := make([]*fnode, n)
	for i := range kids {
		kids[i] = node(i)
	}
	return el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("body", "", "", box(0, 0, 1280, 800), kids...))
}

// readFake indexes a fake document as read() gets it from the browser.
func readFake(t testing.TB, doc *fnode) (*dom, []axNode) {
	t.Helper()
	number(doc)
	fb := &fakeBrowser{}
	raw, err := json.Marshal(fb.snapshot(&ftab{id: "T", url: "https://example.com/", doc: doc}))
	if err != nil {
		t.Fatal(err)
	}
	var s domSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(axTree(doc))
	var nodes []axNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatal(err)
	}
	return indexDOM(&s), nodes
}

var fullView = [4]float64{0, 0, 1280, 800}

func TestSharedLongStringsCostLittle(t *testing.T) {
	spaces := strings.Repeat(" ", sharedLen) + "x"
	long := strings.Repeat("a", sharedLen)
	const n = 5000
	ctx := context.Background()
	cases := []struct {
		name  string
		doc   func() *fnode
		check func(t *testing.T, items []item, textOnly bool)
	}{
		{"text nodes sharing a megabyte of spaces", func() *fnode {
			return manyNodes(n, func(int) *fnode {
				return el("div", "", "", box(0, 0, 10, 10), &fnode{tag: "#text", text: spaces, role: "StaticText", name: "x", box: box(0, 0, 10, 10), display: "inline", visib: "visible"})
			})
		}, func(t *testing.T, items []item, textOnly bool) {
			if textOnly && (len(items) != n || items[0].text != "x") {
				t.Errorf("%d items, the first %q: the text after the spaces is lost", len(items), items[0].text)
			}
		}},
		{"fields sharing a megabyte value and attributes", func() *fnode {
			return manyNodes(n, func(i int) *fnode {
				f := el("input", "textbox", "Field", box(0, 0, 10, 10)).attr("name", long).attr("autocomplete", long).attr("type", long)
				f.value = &long
				if i == 0 {
					f.attrs = []string{"name", "password" + long}
				}
				return f
			})
		}, func(t *testing.T, items []item, textOnly bool) {
			if textOnly {
				return
			}
			if items[0].value != "[filled]" || items[1].value != `"`+strings.Repeat("a", maxField-1)+`…"` {
				t.Errorf("values %q, %q", items[0].value, items[1].value)
			}
		}},
		{"iframes sharing a megabyte src", func() *fnode {
			src := "https://example.com/" + long // one string, as the snapshot sends it
			return manyNodes(n, func(int) *fnode {
				return el("iframe", "Iframe", "", box(0, 0, 10, 10)).attr("src", src)
			})
		}, func(t *testing.T, items []item, textOnly bool) {
			if !textOnly && items[0].name != "https://example.com" {
				t.Errorf("iframe %q", items[0].name)
			}
		}},
		{"30,000 texts in one block", func() *fnode {
			return manyNodes(30000, func(int) *fnode { return txt("word", box(0, 0, 10, 10)) })
		}, func(t *testing.T, items []item, _ bool) {
			if len(items) != 1 || len(items[0].text) > maxRunText || !strings.HasPrefix(items[0].text, "word word") {
				t.Errorf("%d items, the first %d bytes", len(items), len(items[0].text))
			}
		}},
		{"5,000 nested inline elements, each with text", func() *fnode {
			root := el("span", "", "", box(0, 0, 10, 10))
			root.display = "inline"
			leaf := root
			for range 5000 {
				s := el("span", "", "", box(0, 0, 10, 10), txt("w", box(0, 0, 10, 10)))
				s.display = "inline"
				leaf.kids = append(leaf.kids, s)
				leaf = s
			}
			return manyNodes(1, func(int) *fnode { return root })
		}, nil},
		{"more text than a page read keeps", func() *fnode {
			return manyNodes(600, func(i int) *fnode {
				return el("p", "", "", box(0, 0, 10, 10), &fnode{tag: "#text", text: strings.Repeat("y", 10<<10) + string(rune('a'+i%26)), role: "StaticText", name: strings.Repeat("y", 10<<10), box: box(0, 0, 10, 10), display: "inline", visib: "visible"})
			})
		}, func(t *testing.T, items []item, _ bool) {
			total := 0
			for _, it := range items {
				total += len(it.text)
			}
			if total > maxPageText {
				t.Errorf("%d bytes of text kept", total)
			}
		}},
	}
	for _, c := range cases {
		d, nodes := readFake(t, c.doc())
		start := time.Now()
		items, _, cut, err := join(ctx, nodes, d, fullView)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		if took > 2*time.Second {
			t.Errorf("%s: join took %s", c.name, took)
		}
		t.Logf("%s: join %s", c.name, took.Round(time.Millisecond))
		if c.check != nil {
			c.check(t, items, false)
		}
		start = time.Now()
		items, cut2, err := textOnly(ctx, d, fullView)
		if err != nil {
			t.Fatal(err)
		}
		took = time.Since(start)
		if took > 2*time.Second {
			t.Errorf("%s: textOnly took %s", c.name, took)
		}
		t.Logf("%s: textOnly %s", c.name, took.Round(time.Millisecond))
		if c.check != nil {
			c.check(t, items, true)
		}
		if strings.HasPrefix(c.name, "more text") && (!cut || !cut2) {
			t.Errorf("%s: the cut isn't reported", c.name)
		}
	}
}

func TestPageReadsStopWithTheirContext(t *testing.T) {
	d, nodes := readFake(t, manyNodes(5000, func(int) *fnode { return txt("word", box(0, 0, 10, 10)) }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := join(ctx, nodes, d, fullView); err == nil {
		t.Error("join went on after its context ended")
	}
	if _, _, err := textOnly(ctx, d, fullView); err == nil {
		t.Error("textOnly went on after its context ended")
	}
}

func TestCapTextLooksAtFewCharacters(t *testing.T) {
	long := strings.Repeat("é", sharedLen)
	start := time.Now()
	for range 5000 {
		if got := capText(long, maxField); len([]rune(got)) != maxField {
			t.Fatalf("%d characters", len([]rune(got)))
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("5,000 caps of a megabyte took %s", took)
	}
	for _, c := range []struct{ in, want string }{{"short", "short"}, {"abcdef", "abcd…"}, {"abcde", "abcde"}, {"ab   cdef", "ab…"}} {
		if got := capText(c.in, 5); got != c.want {
			t.Errorf("capText(%q, 5) = %q, want %q", c.in, got, c.want)
		}
	}
}
