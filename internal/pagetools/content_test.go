package pagetools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
)

// articlePage is a page with a nav, an article, hidden text and a field.
func articlePage() *fnode {
	vb := box(0, 0, 1280, 20)
	pw := "hunter2"
	return el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		el("html", "", "", vb,
			el("body", "", "", vb,
				el("nav", "navigation", "", vb,
					el("a", "link", "Home", vb, txt("Home", vb)).attr("href", "/"),
					el("a", "link", "News", vb, txt("News", vb)).attr("href", "/news")),
				el("main", "main", "", vb,
					el("article", "article", "", vb,
						el("h1", "heading", "Library opens", vb, txt("Library opens", vb)),
						el("p", "paragraph", "", vb, txt(prose, vb), el("a", "link", "the plan", vb, txt("the plan", vb)).attr("href", "plan.html")),
						el("p", "paragraph", "", vb, txt(prose, vb)),
						el("p", "paragraph", "", nil, txt("HIDDEN-TEXT", nil)),
						el("p", "paragraph", "", vb, txt("VISIBILITY-HIDDEN", vb).with(func(n *fnode) { n.visib = "hidden" })),
						el("input", "textbox", "Password", vb).attr("type", "password").with(func(n *fnode) { n.value = &pw }))),
				el("footer", "contentinfo", "", vb, txt("Copyright footer", vb)))))
}

const prose = "The committee met on Tuesday to discuss the new library, which opens next spring. Members agreed on the budget, the opening hours and the staffing plan. "

func (f *fixture) text(in TextIn) (string, error) {
	return f.tools.Text(context.Background(), f.session(), f.who, in)
}

// checkContentSent checks the fake browser was sent only what the
// content endpoints may send.
func checkContentSent(t *testing.T, fb *fakeBrowser) {
	t.Helper()
	allowed := ContentAllowedMethods()
	for _, m := range fb.Received() {
		if !slices.Contains(allowed, m.Method) {
			t.Errorf("sent %s", m.Method)
		}
	}
	for _, m := range []string{"Input.dispatchMouseEvent", "Input.insertText", "Page.navigate", "DOM.focus", "DOM.resolveNode", "Accessibility.getFullAXTree"} {
		if slices.Contains(allowed, m) {
			t.Errorf("the content endpoints may send %s", m)
		}
	}
}

func TestMarkdown(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/dir/article", articlePage(), true)
	out, err := f.text(TextIn{Format: "markdown"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "<untrusted-content-") || !strings.Contains(out, `source="page as Markdown"`) || strings.Count(out, "</untrusted-content-") != 2 {
		t.Fatalf("not one fenced block:\n%s", out)
	}
	for _, want := range []string{"URL: https://example.com/dir/article", "Title: Page https://example.com/dir/article",
		"[Home](https://example.com/)", "# Library opens", "[the plan](https://example.com/dir/plan.html)", "Copyright footer"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, bad := range []string{"HIDDEN", "hunter2"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s in\n%s", bad, out)
		}
	}
	if strings.HasSuffix(out, ".") {
		t.Errorf("a whole page has no trailer:\n%s", out)
	}
	f.closedAfter()
	checkContentSent(t, f.fb)
	if acts := f.m.Activity(f.sess.ID); len(acts) == 0 || acts[0].What != "Read the page as Markdown" {
		t.Errorf("activity: %+v", acts)
	}
}

func TestReadability(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/dir/article", articlePage(), true)
	out, err := f.text(TextIn{Format: "readability"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `source="page's main text"`) || !strings.Contains(out, "Library opens\n\nThe committee met") {
		t.Errorf("readability:\n%s", out)
	}
	for _, bad := range []string{"Home", "News", "Copyright", "HIDDEN", "https://example.com/dir/plan.html"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s in\n%s", bad, out)
		}
	}
}

func TestTextInParts(t *testing.T) {
	f := newFixture(t)
	var kids []*fnode
	for i := range 300 {
		b := box(0, float64(i*20), 800, 20)
		kids = append(kids, el("p", "paragraph", "", b, txt(fmt.Sprintf("Paragraph %03d %s", i, strings.Repeat("word ", 10)), b)))
	}
	f.fb.addTab("https://example.com/long", el("#document", "RootWebArea", "", box(0, 0, 1280, 800), kids...), true)
	var all strings.Builder
	offset, calls := 0, 0
	for {
		out, err := f.text(TextIn{Format: "markdown", MaxChars: 5000, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		calls++
		body := out[strings.Index(out, "Title: "):strings.LastIndex(out, "\n</untrusted-content-")]
		all.WriteString(body)
		m := strings.LastIndex(out, "Read on with offset ")
		if m < 0 {
			if !strings.Contains(out, " of ") {
				t.Errorf("the last part doesn't say where it is:\n%s", out[len(out)-200:])
			}
			break
		}
		next := 0
		if _, err := fmt.Sscanf(out[m:], "Read on with offset %d.", &next); err != nil || next <= offset {
			t.Fatalf("bad offset in %q", out[m:])
		}
		if len([]rune(body)) > 5200 {
			t.Errorf("a part of %d characters", len([]rune(body)))
		}
		offset = next
		if calls > 50 {
			t.Fatal("never ends")
		}
	}
	for i := range 300 {
		if p := fmt.Sprintf("Paragraph %03d", i); strings.Count(all.String(), p) != 1 {
			t.Fatalf("%s appears %d times across the parts", p, strings.Count(all.String(), p))
		}
	}
	if calls < 4 {
		t.Errorf("%d calls for ~20,000 characters in parts of 5,000", calls)
	}
	if _, err := f.text(TextIn{Format: "markdown", Offset: 10_000_000}); !isStatus(err, 400) {
		t.Errorf("offset past the end: %v", err)
	}
	for _, in := range []TextIn{{Format: "html"}, {Format: "markdown", MaxChars: MaxContentChars + 1}, {Format: "markdown", Offset: -1}} {
		if _, err := f.text(in); !isStatus(err, 400) {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

func TestContentRefusesVisualModeAndStuckPages(t *testing.T) {
	f := newFixture(t)
	s, _, err := f.m.Start(context.Background(), "shot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tools.Text(context.Background(), s, f.who, TextIn{Format: "markdown"}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "Visual mode") {
		t.Errorf("markdown: %v", err)
	}
	if _, err := f.tools.PDF(context.Background(), s, f.who, PDFIn{}); !isStatus(err, http.StatusConflict) {
		t.Errorf("pdf: %v", err)
	}
	if f.fb.Dials() != 0 {
		t.Error("content connected to a Visual-mode browser")
	}
	// A page held by a dialog.
	tb := f.fb.addTab("https://example.com/", articlePage(), true)
	if _, err := f.text(TextIn{Format: "markdown"}); err != nil {
		t.Fatal(err)
	}
	tb.stuck = true
	f.fb.call = printer(nil)
	for _, call := range []func() error{
		func() error { _, err := f.text(TextIn{Format: "readability"}); return err },
		func() error { _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{}); return err },
	} {
		if err := call(); !isStatus(err, http.StatusGatewayTimeout) {
			t.Errorf("stuck page: %v", err)
		}
	}
	f.closedAfter()
}

// printer answers Page.printToPDF with pdf, recording its params.
func printer(got *map[string]any) func(*ftab, cdptest.Message) (any, bool, error) {
	return printerOf([]byte("%PDF-1.4 fake"), got)
}

func printerOf(pdf []byte, got *map[string]any) func(*ftab, cdptest.Message) (any, bool, error) {
	return func(_ *ftab, m cdptest.Message) (any, bool, error) {
		if m.Method != "Page.printToPDF" {
			return nil, false, nil
		}
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		if got != nil {
			*got = p
		}
		if r, _ := p["pageRanges"].(string); r == "99" {
			return nil, true, cdptest.Error{Message: "Page range exceeds page count"}
		}
		return map[string]any{"data": base64.StdEncoding.EncodeToString(pdf)}, true, nil
	}
}

func TestPDF(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", articlePage(), true)
	var params map[string]any
	f.fb.call = printer(&params)
	pdf, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{Landscape: true, Pages: "1-2, 5"})
	if err != nil {
		t.Fatal(err)
	}
	if string(pdf) != "%PDF-1.4 fake" {
		t.Errorf("pdf: %q", pdf)
	}
	if params["landscape"] != true || params["printBackground"] != false || params["pageRanges"] != "1-2,5" {
		t.Errorf("params: %v", params)
	}
	if acts := f.m.Activity(f.sess.ID); len(acts) == 0 || !strings.HasPrefix(acts[0].What, "Printed the page to PDF") {
		t.Errorf("activity: %+v", acts)
	}
	f.closedAfter()
	checkContentSent(t, f.fb)

	// Bad ranges are refused before printing; one past the end after.
	for _, p := range []string{"abc", "1;2", strings.Repeat("1,", 60)} {
		if _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{Pages: p}); !isStatus(err, 400) {
			t.Errorf("pages %q: %v", p, err)
		}
	}
	if _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{Pages: "99"}); !isStatus(err, 400) {
		t.Errorf("pages past the end: %v", err)
	}
	// Too large, and not a PDF.
	f.fb.call = printerOf(append([]byte("%PDF-"), make([]byte, MaxPDF)...), nil)
	if _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{}); !isStatus(err, 422) {
		t.Errorf("too large: %v", err)
	}
	f.fb.call = printerOf([]byte("<html>"), nil)
	if _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{}); !isStatus(err, http.StatusBadGateway) {
		t.Errorf("not a PDF: %v", err)
	}
	f.closedAfter()
}

func TestContentLeavesNothingAttachedOnCancel(t *testing.T) {
	f := newFixture(t)
	tb := f.fb.addTab("https://example.com/", articlePage(), true)
	tb.stuck = true
	f.tools.slow = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := f.tools.Text(ctx, f.session(), f.who, TextIn{Format: "markdown"}); err == nil {
		t.Fatal("a stuck page answered")
	}
	f.closedAfter()
}

// Content calls share the whole-page read slots with the page tools:
// when other sessions hold them all, they answer busy.
func TestContentWaitsForAReadSlot(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", articlePage(), true)
	f.fb.call = printer(nil)
	for range maxReads {
		f.tools.reads <- struct{}{}
	}
	f.tools.readWait = 100 * time.Millisecond
	if _, err := f.text(TextIn{Format: "markdown"}); !isStatus(err, http.StatusServiceUnavailable) {
		t.Errorf("markdown: %v", err)
	}
	if _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{}); !isStatus(err, http.StatusServiceUnavailable) {
		t.Errorf("pdf: %v", err)
	}
	for range maxReads {
		<-f.tools.reads
	}
	if _, err := f.text(TextIn{Format: "markdown"}); err != nil {
		t.Errorf("with a slot free: %v", err)
	}
	if len(f.tools.reads) != 0 {
		t.Errorf("%d read slots still held", len(f.tools.reads))
	}
	f.closedAfter()
}

// A hidden chain of many elements (display:none: nothing laid out) costs
// nothing to render; the page around it is read as usual.
func TestHiddenDeepChainIsCheap(t *testing.T) {
	f := newFixture(t)
	vb := box(0, 0, 800, 20)
	leaf := txt("HIDDEN-LEAF", nil)
	cur := leaf
	for range 50000 {
		cur = el("em", "", "", nil, cur)
	}
	f.fb.addTab("https://example.com/", el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		el("body", "", "", vb, el("p", "paragraph", "", vb, txt("VISIBLE", vb)), el("div", "", "", nil, cur))), true)
	start := time.Now()
	for _, format := range []string{"markdown", "readability"} {
		out, err := f.text(TextIn{Format: format})
		if err != nil || !strings.Contains(out, "VISIBLE") || strings.Contains(out, "HIDDEN-LEAF") {
			t.Errorf("%s: %v\n%s", format, err, out)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

// An offset that turns out past the end is refused, and not logged as a
// read; a part starting inside a code block opens it again.
func TestTextOffsets(t *testing.T) {
	f := newFixture(t)
	vb := box(0, 0, 800, 20)
	var code []string
	for i := range 200 {
		code = append(code, fmt.Sprintf("line %03d of the code", i))
	}
	f.fb.addTab("https://example.com/", el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		el("body", "", "", vb, el("p", "paragraph", "", vb, txt("Intro.", vb)),
			el("pre", "", "", vb, el("code", "", "", vb, txt(strings.Join(code, "\n"), vb)).attr("class", "language-go")))), true)
	before := len(f.m.Activity(f.sess.ID))
	if _, err := f.text(TextIn{Format: "markdown", Offset: 1_000_000}); !isStatus(err, 400) {
		t.Fatalf("offset past the end: %v", err)
	}
	if n := len(f.m.Activity(f.sess.ID)); n != before {
		t.Errorf("a refused read was logged (%d → %d)", before, n)
	}
	out, err := f.text(TextIn{Format: "markdown", Offset: 2000, MaxChars: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\n\n…\n```go\n") {
		t.Errorf("a part inside the code block doesn't open it:\n%s", out)
	}
}

// Only Chromium's refusal of a page range is the agent's mistake; a page
// that navigates while printing is a conflict.
func TestPDFErrors(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", articlePage(), true)
	for _, tc := range []struct {
		msg    string
		status int
	}{
		{"Page range syntax error", 400},
		{"Inspected target navigated or closed", 409},
		{"Printing failed", http.StatusBadGateway},
	} {
		f.fb.call = func(_ *ftab, m cdptest.Message) (any, bool, error) {
			if m.Method != "Page.printToPDF" {
				return nil, false, nil
			}
			return nil, true, cdptest.Error{Message: tc.msg}
		}
		if _, err := f.tools.PDF(context.Background(), f.session(), f.who, PDFIn{Pages: "1"}); !isStatus(err, tc.status) {
			t.Errorf("%s: %v", tc.msg, err)
		}
	}
	f.closedAfter()
}

// TestSharedStringsInTheSnapshot: the DOM snapshot shares one string among
// every node that has it, so thousands of nodes can carry the same long
// class or text. Reading stays fast, and a handoff isn't held up.
func TestSharedStringsInTheSnapshot(t *testing.T) {
	f := newFixture(t)
	vb := box(0, 0, 800, 20)
	cls := strings.Repeat("abcdefg hijkl ", 10000) // 140 KB
	ws := strings.Repeat(" ", 1<<20)
	kids := []*fnode{el("p", "paragraph", "", vb, txt("SHARED-PAGE", vb))}
	for range 5000 {
		kids = append(kids, el("div", "", "", vb, txt("x", vb)).attr("class", cls))
	}
	for range 2000 {
		kids = append(kids, el("span", "", "", vb, txt(ws, vb)))
	}
	f.fb.addTab("https://example.com/", el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("body", "", "", vb, kids...)), true)
	for _, format := range []string{"markdown", "readability"} {
		start := time.Now()
		out, err := f.text(TextIn{Format: format})
		if err != nil || !strings.Contains(out, "SHARED-PAGE") {
			t.Errorf("%s: %v\n%.300s", format, err, out)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s took %v", format, d)
		}
	}
	// A handoff during a read ends it at once.
	done := make(chan error, 1)
	go func() { _, err := f.text(TextIn{Format: "readability"}); done <- err }()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if _, err := f.m.SetNeedsHelp(f.sess.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Logf("the read ended with %v", err)
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("the read ended %v after the handoff", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read went on after the handoff")
	}
}

// TestDisplayContentsKeepsItsRole: an element with no box of its own but
// shown content (display:contents) keeps what it is.
func TestDisplayContentsKeepsItsRole(t *testing.T) {
	f := newFixture(t)
	vb := box(0, 0, 800, 20)
	f.fb.addTab("https://example.com/dir/", el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("body", "", "", vb,
		el("h1", "heading", "", nil, txt("CONTENTS-HEADING", vb)),
		el("a", "link", "", nil, txt("CONTENTS-LINK", vb)).attr("href", "/contents-link"),
		el("div", "", "", nil, el("div", "", "", nil, el("p", "", "", nil, txt("NONE-HIDDEN", nil)))),
	)), true)
	out, err := f.text(TextIn{Format: "markdown"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# CONTENTS-HEADING", "[CONTENTS-LINK](https://example.com/contents-link)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "NONE-HIDDEN") {
		t.Errorf("hidden text in\n%s", out)
	}
}

// TestHiddenTextSpendsNoBudget: 9 MB of display:none text before an
// article doesn't push it out; 9 MB of shown text does, and both formats
// then say the page has more, after the fence.
func TestHiddenTextSpendsNoBudget(t *testing.T) {
	vb := box(0, 0, 800, 20)
	blob := strings.Repeat("var words = 1; ", 69906) // 1 MB
	long := strings.Repeat("The committee met on Tuesday, and agreed on the budget, the hours and the plan. ", 4)
	page := func(shown bool) *fnode {
		kids := []*fnode{el("p", "paragraph", "", vb, txt("BEFORE-BIG", vb))}
		for range 9 {
			b := vb
			if !shown {
				b = nil
			}
			kids = append(kids, el("div", "", "", b, txt(blob, b)))
		}
		kids = append(kids, el("article", "article", "", vb, el("h1", "heading", "", vb, txt("AFTER-BIG-HEADING", vb)), el("p", "paragraph", "", vb, txt(long+"AFTER-BIG-END", vb))))
		return el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("body", "", "", vb, kids...))
	}
	for _, shown := range []bool{false, true} {
		f := newFixture(t)
		f.fb.addTab("https://example.com/", page(shown), true)
		for _, format := range []string{"markdown", "readability"} {
			out, err := f.text(TextIn{Format: format, MaxChars: MaxContentChars})
			if err != nil {
				t.Fatal(err)
			}
			// The notice comes after the last part.
			if m := regexp.MustCompile(`of ([0-9,]+)\.`).FindStringSubmatch(out[strings.LastIndex(out, "</untrusted-content-"):]); m != nil {
				total, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
				if out, err = f.text(TextIn{Format: format, Offset: total - 100}); err != nil {
					t.Fatal(err)
				}
			}
			says := strings.Contains(out[strings.LastIndex(out, "</untrusted-content-"):], "more than BrowserPier renders")
			switch {
			case !shown && (!strings.Contains(out, "AFTER-BIG-HEADING") || says):
				t.Errorf("%s after 9 MB of hidden text: says %v\n%.300s", format, says, out)
			case shown && !says:
				t.Errorf("%s after 9 MB of shown text doesn't say the page has more:\n%s", format, out[max(0, len(out)-300):])
			}
		}
	}
}
