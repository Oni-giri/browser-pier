package pagetools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// divsPage is PROJ-124's page: controls built from a div or a span, which
// only a pointer cursor or a tabindex give away, and what isn't one.
type divsPage struct {
	doc                                    *fnode
	archive, mark, star, decor, link, card *fnode
	save                                   *fnode
	mu                                     sync.Mutex
	clicks                                 []string
}

func inline(n *fnode) *fnode { n.display = "inline"; return n }

func newDivsPage(fb *fakeBrowser) *divsPage {
	p := &divsPage{}
	y := 10.0
	next := func() *[4]float64 { y += 40; return box(10, y, 200, 30) }
	clicks := func(n *fnode, label string) {
		fb.behave[n] = &behaviour{onClick: func() { p.mu.Lock(); p.clicks = append(p.clicks, label); p.mu.Unlock() }}
	}
	// <div style="cursor:pointer"><span>Archive</span> <span>(3)</span></div>:
	// Chromium leaves the spans out of the AX tree.
	b := next()
	p.archive = el("div", "generic", "", b, inline(el("span", "", "", b, txt("Archive", b))), txt(" ", b), inline(el("span", "", "", b, txt("(3)", b))))
	p.archive.cursor = "pointer"
	clicks(p.archive, "a")
	// <p><span tabindex="0">Mark read</span></p>
	b = next()
	p.mark = inline(el("span", "generic", "", b, txt("Mark read", b))).attr("tabindex", "0")
	clicks(p.mark, "b")
	markP := el("p", "paragraph", "", b, p.mark)
	// <p>Liked by 2 people. <span style="cursor:pointer">Star it</span></p>,
	// the span's click handled by a listener on the document: no AX node.
	b = next()
	p.star = inline(el("span", "", "", b, txt("Star it", b)))
	p.star.cursor = "pointer"
	clicks(p.star, "c")
	starP := el("p", "paragraph", "", b, txt("Liked by 2 people. ", b), p.star)
	// A decorative pointer div, with no text.
	p.decor = el("div", "generic", "", next())
	p.decor.cursor = "pointer"
	clicks(p.decor, "decor")
	// A link: its children inherit its pointer.
	b = next()
	p.link = inline(el("a", "link", "More options", b, inline(el("span", "", "", b, txt("More", b))), txt(" ", b), inline(el("b", "", "", b, txt("options", b)))))
	p.link.cursor = "pointer"
	clicks(p.link, "link")
	linkP := el("p", "paragraph", "", b, p.link)
	// A pointer div holding a real button.
	b = next()
	p.save = el("button", "button", "Save", b, txt("Save", b))
	p.save.cursor = "default"
	clicks(p.save, "save")
	p.card = el("div", "generic", "", b, p.save)
	p.card.cursor = "pointer"
	clicks(p.card, "card")
	p.doc = el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		el("main", "main", "", box(0, 0, 1280, 800), el("h1", "heading", "Inbox", box(10, 10, 200, 30), txt("Inbox", box(10, 10, 200, 30))),
			p.archive, markP, starP, p.decor, linkP, p.card))
	return p
}

func (p *divsPage) clicked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.clicks...)
}

// items joins a fake document as a snapshot reads it.
func items(t *testing.T, doc *fnode) []item {
	t.Helper()
	d, nodes := readFake(t, doc)
	out, _, _, err := join(context.Background(), nodes, d, fullView)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// lines is how items read, one per line: a control as role "name", text
// as itself.
func lines(items []item) []string {
	var out []string
	for _, it := range items {
		if it.control {
			out = append(out, it.role+" "+quote(it.name))
		} else {
			out = append(out, it.text)
		}
	}
	return out
}

// TestGenericControls: the page's div and span controls are generic
// items with their text as name, once, and what is inside them isn't
// listed again; a decorative div, a link's children and a div around a
// button aren't generics.
func TestGenericControls(t *testing.T) {
	p := newDivsPage(newFakeBrowser(t))
	got := strings.Join(lines(items(t, p.doc)), "\n")
	want := strings.Join([]string{`Inbox`, `generic "Archive (3)"`, `generic "Mark read"`, `Liked by 2 people.`, `generic "Star it"`, `link "More options"`, `button "Save"`}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// Each names its own element, for act.
	for _, it := range items(t, p.doc) {
		switch it.name {
		case "Archive (3)":
			if it.backend != p.archive.backend {
				t.Errorf("Archive names node %d", it.backend)
			}
		case "Mark read":
			if it.backend != p.mark.backend {
				t.Errorf("Mark read names node %d", it.backend)
			}
		case "Star it":
			if it.backend != p.star.backend {
				t.Errorf("Star it names node %d", it.backend)
			}
		}
	}
}

// TestGenericControlRules: each condition, case by case.
func TestGenericControlRules(t *testing.T) {
	b := box(10, 10, 200, 30)
	div := func(role string, kids ...*fnode) *fnode { return el("div", role, "", b, kids...) }
	pointer := func(n *fnode) *fnode { n.cursor = "pointer"; return n }
	for _, c := range []struct {
		name string
		node func() *fnode
		want []string
	}{
		{"tabindex -1 isn't the tab order", func() *fnode { return div("generic", txt("Skip", b)).attr("tabindex", "-1") }, []string{"Skip"}},
		{"tabindex -0 is", func() *fnode { return div("generic", txt("Zero", b)).attr("tabindex", "-0") }, []string{`generic "Zero"`}},
		{"tabindex junk isn't", func() *fnode { return div("generic", txt("Junk", b)).attr("tabindex", "x") }, []string{"Junk"}},
		{"tabindex 3 is", func() *fnode { return div("generic", txt("Three", b)).attr("tabindex", " 3") }, []string{`generic "Three"`}},
		{"a role of its own isn't generic", func() *fnode { return pointer(el("li", "listitem", "", b, txt("Paris", b))).attr("tabindex", "0") }, []string{"Paris"}},
		{"an ignored (uninteresting) div is", func() *fnode {
			return pointer(div("none", txt("Open", b)).with(func(n *fnode) { n.ignored = true }))
		}, []string{`generic "Open"`}},
		{"aria-hidden text is no name", func() *fnode {
			return pointer(div("generic", &fnode{tag: "#text", text: "Hidden", role: "StaticText", name: "Hidden", ignored: true, box: b, display: "inline", visib: "visible"}))
		}, nil},
		{"hidden text is no name", func() *fnode {
			// Under display:none, nothing is laid out.
			hidden := inline(el("span", "", "", nil, txt("SECRET", nil)))
			hidden.display = "none"
			return pointer(div("generic", txt("Open", b), hidden))
		}, []string{`generic "Open"`}},
		{"a hidden generic isn't listed", func() *fnode {
			gone := txt("Gone", b)
			gone.visib = "hidden" // inherited
			n := pointer(div("generic", gone))
			n.visib = "hidden"
			return n
		}, nil},
		{"the outer of nested generics", func() *fnode {
			return pointer(div("generic", txt("Outer", b), div("generic", txt("inner", b)).attr("tabindex", "0")))
		}, []string{`generic "Outer inner"`}},
		{"a pointer inherited through a box-less parent doesn't start", func() *fnode {
			contents := el("div", "", "", nil, div("generic", txt("Row", b)))
			contents.display = "contents"
			return pointer(div("list", contents))
		}, []string{"Row"}},
		{"a pointer from a box-less parent starts on its child", func() *fnode {
			contents := pointer(el("div", "", "", nil, div("generic", txt("Row", b))))
			contents.display = "contents"
			return div("list", contents)
		}, []string{`generic "Row"`}},
		{"a hidden button inside doesn't count", func() *fnode {
			btn := el("button", "button", "X", b, txt("X", b))
			btn.display = "none"
			return pointer(div("generic", txt("Card", b), btn))
		}, []string{`generic "Card"`}},
		{"a pointer span the AX tree leaves out, inside a generic, is part of it", func() *fnode {
			s := pointer(inline(el("span", "", "", b, txt("two", b))))
			return div("generic", txt("one ", b), s).attr("tabindex", "0")
		}, []string{`generic "one two"`}},
		{"text split in blocks has spaces", func() *fnode {
			return pointer(div("generic", el("div", "", "", b, txt("Title", b)), el("div", "", "", b, txt("Sub", b))))
		}, []string{`generic "Title Sub"`}},
		{"no space inside a word", func() *fnode {
			return pointer(div("generic", txt("Hel", b), inline(el("b", "", "", b, txt("lo", b)))))
		}, []string{`generic "Hello"`}},
	} {
		doc := el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("body", "", "", box(0, 0, 1280, 800), c.node()))
		got := lines(items(t, doc))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestGenericItemsInAContainer: 2,000 rows inherit the pointer from their
// list, so only the list is a generic; its name is cut, so the rows'
// text stays, and nothing is listed twice.
func TestGenericItemsInAContainer(t *testing.T) {
	rows := make([]*fnode, 2000)
	for i := range rows {
		b := box(10, float64(i*20), 400, 20)
		rows[i] = el("div", "generic", "", b, txt(fmt.Sprintf("Row %d", i), b))
	}
	list := el("div", "generic", "", box(10, 0, 400, 40000), rows...)
	list.cursor = "pointer"
	doc := el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("body", "", "", box(0, 0, 1280, 800), list))
	start := time.Now()
	got := items(t, doc)
	t.Logf("2,000 rows: %s", time.Since(start))
	generics, texts := 0, 0
	for _, it := range got {
		switch {
		case it.control:
			generics++
			if it.role != "generic" || it.backend != list.backend || !strings.HasPrefix(it.name, "Row 0 Row 1 Row 2") || len(it.name) > maxName {
				t.Errorf("the control is %s %q (node %d)", it.role, capText(it.name, 40), it.backend)
			}
		case it.text != "":
			texts++
		}
	}
	if generics != 1 || texts != 2000 {
		t.Errorf("%d generics, %d texts", generics, texts)
	}
	// A short list says it all in its line: its rows' text goes.
	short := el("div", "generic", "", box(10, 0, 400, 40), el("div", "generic", "", box(10, 0, 400, 20), txt("One", box(10, 0, 400, 20))), el("div", "generic", "", box(10, 20, 400, 20), txt("Two", box(10, 20, 400, 20))))
	short.cursor = "pointer"
	doc = el("#document", "RootWebArea", "", box(0, 0, 1280, 800), short)
	if got := strings.Join(lines(items(t, doc)), "|"); got != `generic "One Two"` {
		t.Errorf("short list: %s", got)
	}
}

// TestGenericControlsInASnapshot: in a snapshot, a generic has a ref in
// the fence, which act clicks; it diffs like any control.
func TestGenericControlsInASnapshot(t *testing.T) {
	f := newFixture(t)
	p := newDivsPage(f.fb)
	f.fb.addTab("https://example.com/inbox", p.doc, true)
	snap := f.mustSnapshot(nil)
	for _, want := range []string{` generic "Archive (3)"`, ` generic "Mark read"`, ` generic "Star it"`} {
		if strings.Count(snap, want) != 1 || strings.Index(snap, want) > strings.LastIndex(snap, "</untrusted-content-") {
			t.Errorf("%s isn't once in the fence:\n%s", want, snap)
		}
	}
	r := f.refs()
	for _, c := range []struct{ name, click string }{{"Archive (3)", "a"}, {"Mark read", "b"}, {"Star it", "c"}} {
		if _, err := f.act(Step{Action: "click", Ref: r[c.name]}); err != nil {
			t.Fatalf("click %s: %v", c.name, err)
		}
		if got := p.clicked(); got[len(got)-1] != c.click {
			t.Errorf("click %s: clicks %v", c.name, got)
		}
	}
	// Removing one is a diff line, with its ref.
	f.fb.mu.Lock()
	main := p.doc.kids[0]
	main.kids = append(main.kids[:1:1], main.kids[2:]...)
	f.fb.mu.Unlock()
	out, err := f.tools.Snapshot(context.Background(), f.session(), f.who, SnapshotIn{Diff: true})
	if err != nil || !strings.Contains(out, `- `+r["Archive (3)"]+` generic "Archive (3)"`) {
		t.Errorf("diff: %v\n%s", err, out)
	}
}
