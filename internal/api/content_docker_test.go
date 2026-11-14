//go:build docker

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// content calls POST /content, returning the status, headers and body.
func (a agent) content(id string, body any) (int, http.Header, []byte) {
	a.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+id+"/content", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// contentText calls a text format that must work.
func (a agent) contentText(id string, body map[string]any) string {
	a.t.Helper()
	code, _, raw := a.content(id, body)
	var out struct{ Text, Error string }
	_ = json.Unmarshal(raw, &out)
	if code != http.StatusOK {
		a.t.Fatalf("content %v: %d %s", body, code, out.Error)
	}
	return out.Text
}

// inFence reports whether every want is inside the output's fence.
func inFence(out, want string) bool {
	i, end := strings.Index(out, want), strings.LastIndex(out, "</untrusted-content-")
	return strings.HasPrefix(out, "<untrusted-content-") && i > 0 && i < end
}

// TestContent is PROJ-20 on a real browser: an article page as Markdown
// and as its main text, in parts, a screenshot and a PDF; hidden text
// and a page-filled password stay out; the caps hold; nothing stays
// attached and the activity log has no page text.
func TestContent(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, siteHost(t))
	s := a.startAPIBrowser(t, "c")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/article"})

	md := a.contentText(s.ID, map[string]any{"format": "markdown"})
	for _, want := range []string{
		"URL: " + site.URL + "/article", "Title: Library opens",
		"# Library opens next spring", "[the full plan](" + site.URL + "/plan.pdf)",
		"| Day | Hours |", "| Monday | 9–18 |", "- Free cards for residents",
		"```sh\ncurl https://library.example/api\n```", "## Opening hours",
		"Text in a closed shadow root.", "OPACITY-ZERO-TEXT", "[World](" + site.URL + "/world)",
		"We use cookies.", "[iframe: " + site.URL + "]",
	} {
		if !inFence(md, want) {
			t.Errorf("markdown: %q isn't in the fence:\n%s", want, md)
		}
	}
	for _, gone := range []string{"DISPLAY-NONE-TEXT", "VISIBILITY-HIDDEN-TEXT", "PAGE-FILLED-SECRET", "attachShadow"} {
		if strings.Contains(md, gone) {
			t.Errorf("markdown has %s:\n%s", gone, md)
		}
	}

	article := a.contentText(s.ID, map[string]any{"format": "readability"})
	for _, want := range []string{"Library opens next spring", "The committee met on Tuesday", "Day | Hours", "Free cards for residents"} {
		if !inFence(article, want) {
			t.Errorf("readability: %q isn't in the fence:\n%s", want, article)
		}
	}
	for _, gone := range []string{"Weather", "We use cookies", "Most read", "© Example News", "DISPLAY-NONE-TEXT", "PAGE-FILLED-SECRET", "](http"} {
		if strings.Contains(article, gone) {
			t.Errorf("readability has %q:\n%s", gone, article)
		}
	}
	t.Logf("markdown, %d bytes:\n%s\nreadability, %d bytes:\n%s", len(md), md, len(article), article)
	if len(article) >= len(md) {
		t.Errorf("readability (%d) isn't shorter than Markdown (%d)", len(article), len(md))
	}

	// In parts: every part fenced, together the whole page.
	whole := md[strings.Index(md, "URL: "):strings.LastIndex(md, "\n</untrusted-content-")]
	var parts strings.Builder
	offset := 0
	for range 50 {
		out := a.contentText(s.ID, map[string]any{"format": "markdown", "max_chars": 300, "offset": offset})
		body := out[strings.Index(out, "Title: "):strings.LastIndex(out, "\n</untrusted-content-")]
		_, body, _ = strings.Cut(body, "\n\n")
		parts.WriteString(strings.TrimPrefix(body, "…"))
		m := regexp.MustCompile(`Read on with offset (\d+)\.`).FindStringSubmatch(out)
		if m == nil {
			break
		}
		offset, _ = strconv.Atoi(m[1])
	}
	// Parts end at a line's end, or mid-line on a long one: compare
	// without whitespace.
	// A part starting inside the code block opens it again: compare
	// without fences too.
	bare := func(s string) string {
		return strings.NewReplacer("```sh", "", "```", "").Replace(strings.Join(strings.Fields(s), ""))
	}
	if bare(parts.String()) != bare(whole[strings.Index(whole, "\n\n"):]) {
		t.Errorf("the parts don't add up to the page:\n%s\n---\n%s", parts.String(), whole)
	}

	// A screenshot from the X display, and a PDF.
	code, h, shot := a.content(s.ID, map[string]any{"format": "screenshot", "width": 640})
	if cfg, err := png.DecodeConfig(bytes.NewReader(shot)); code != http.StatusOK || err != nil || cfg.Width != 640 || h.Get("Content-Type") != "image/png" {
		t.Errorf("screenshot: %d %v %v", code, err, h)
	}
	code, h, pdf := a.content(s.ID, map[string]any{"format": "pdf", "background": true})
	if code != http.StatusOK || !bytes.HasPrefix(pdf, []byte("%PDF-")) || !bytes.Contains(pdf[max(0, len(pdf)-1024):], []byte("%%EOF")) ||
		h.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("pdf: %d %d bytes %v", code, len(pdf), h)
	}
	if code, _, raw := a.content(s.ID, map[string]any{"format": "pdf", "pages": "1", "landscape": true}); code != http.StatusOK || !bytes.HasPrefix(raw, []byte("%PDF-")) {
		t.Errorf("pdf page 1: %d", code)
	}
	if code, _, raw := a.content(s.ID, map[string]any{"format": "pdf", "pages": "99"}); code != http.StatusBadRequest {
		t.Errorf("pdf page 99: %d %s", code, raw)
	}

	// Caps.
	if code, _, raw := a.content(s.ID, map[string]any{"format": "markdown", "max_chars": 100001}); code != http.StatusBadRequest {
		t.Errorf("max_chars past the cap: %d %s", code, raw)
	}
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/huge"})
	for _, format := range []string{"markdown", "readability"} {
		if code, _, raw := a.content(s.ID, map[string]any{"format": format}); code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "too large") {
			t.Errorf("%s of a huge page: %d %s", format, code, raw)
		}
	}

	for _, act := range a.m.Activity(s.ID) {
		if strings.Contains(act.What, "Library") || strings.Contains(act.What, "committee") || strings.Contains(act.What, "SECRET") {
			t.Errorf("the activity log has page text: %q", act.What)
		}
	}
	for _, tg := range targets(t, a.m.Layout().ControlSocket(s.Profile)) {
		if tg.Attached {
			t.Errorf("%s is still attached", tg.URL)
		}
	}
}

// TestContentLeavesOutWhatItMust is the review's probe of PROJ-20: form
// fields' values (typed by script, in a shadow root, or prefixed in
// XHTML), scripts, styles and templates stay out; a page's "<!--" doesn't
// hide the text after it; an iframe's address can't make a heading; and a
// hidden chain of 190,000 elements renders at once.
func TestContentLeavesOutWhatItMust(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, siteHost(t))
	s := a.startAPIBrowser(t, "cf")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/probe-fields"})
	for _, format := range []string{"markdown", "readability"} {
		out := a.contentText(s.ID, map[string]any{"format": format})
		for _, want := range []string{"VISIBLE-PARA", "CONTENTS-TEXT", "SHADOW-P", "AFTER-COMMENT"} {
			if !inFence(out, want) {
				t.Errorf("%s: %s isn't in the fence:\n%s", format, want, out)
			}
		}
		for _, gone := range []string{"JS-TEXT-VALUE", "OTP-123456", "TA-TYPED", "TA-DEFAULT", "SHADOW-INPUT-VALUE", "OPT-ONE", "OPT-TWO",
			"SUBMIT-VALUE", "PLACEHOLDER-TEXT", "NOSCRIPT-TEXT", "TEMPLATE-TEXT", "HIDDEN-ALT", "<!--"} {
			if strings.Contains(out, gone) {
				t.Errorf("%s has %s:\n%s", format, gone, out)
			}
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "#") && strings.Contains(l, "FAKE") {
				t.Errorf("%s: an iframe's address made a heading: %q", format, l)
			}
		}
	}
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/probe.xhtml"})
	out := a.contentText(s.ID, map[string]any{"format": "markdown"})
	if !inFence(out, "XHTML-PARA") {
		t.Errorf("xhtml:\n%s", out)
	}
	for _, gone := range []string{"XHTML-PREFIXED-SECRET", "XHTML-TEXTAREA", "XHTMLSCRIPT", "XHTMLSTYLE"} {
		if strings.Contains(out, gone) {
			t.Errorf("xhtml has %s:\n%s", gone, out)
		}
	}
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/probe-deep?d=19000&c=10"})
	for _, format := range []string{"markdown", "readability"} {
		start := time.Now()
		out := a.contentText(s.ID, map[string]any{"format": format})
		if d := time.Since(start); d > 10*time.Second || !inFence(out, "DEEP-PAGE") {
			t.Errorf("%s of a hidden chain of 190,000 elements: %v\n%s", format, d, out)
		}
	}
}

// TestContentWithSharedStrings is the second review's probe: thousands of
// elements sharing one long class, text or alt (the DOM snapshot sends it
// once) read in a few seconds, and a handoff during a read ends it at once.
func TestContentWithSharedStrings(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, siteHost(t))
	s := a.startAPIBrowser(t, "cs")
	open := func(q string) {
		t.Helper()
		a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/probe-shared?" + q})
		if out := a.mustTool(s.ID, "wait", map[string]any{"text": "SHARED-PAGE"}); out != "ok" {
			t.Fatalf("%s: %s", q, out)
		}
	}
	for _, q := range []string{"k=class&n=5000&kb=100", "k=text&n=2000&kb=1000", "k=alt&n=2000&kb=1000"} {
		open(q)
		for _, format := range []string{"markdown", "readability"} {
			start := time.Now()
			out := a.contentText(s.ID, map[string]any{"format": format})
			if d := time.Since(start); d > 15*time.Second || !inFence(out, "SHARED-PAGE") {
				t.Errorf("%s %s: %v\n%.300s", q, format, d, out)
			} else {
				t.Logf("%s %s: %v", q, format, d.Round(time.Millisecond))
			}
		}
	}
	open("k=class&n=5000&kb=100")
	done := make(chan time.Time, 1)
	go func() {
		a.content(s.ID, map[string]any{"format": "readability"})
		done <- time.Now()
	}()
	time.Sleep(300 * time.Millisecond)
	asked := time.Now()
	if _, err := a.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(asked); d > 3*time.Second {
		t.Errorf("the handoff waited %v for the read", d)
	}
	select {
	case end := <-done:
		t.Logf("the read ended %v after the handoff", end.Sub(asked).Round(time.Millisecond))
	case <-time.After(10 * time.Second):
		t.Error("the read went on 10 s after the handoff")
	}
}

// TestContentDisplayContents: display:contents keeps an element's role
// (a heading, a link, a table row, a nav still left out of the main text)
// without letting field values or hidden text in; past 256 levels a marker
// says the structure was flattened.
func TestContentDisplayContents(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, siteHost(t))
	s := a.startAPIBrowser(t, "cc")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/probe-contents"})
	md := a.contentText(s.ID, map[string]any{"format": "markdown"})
	for _, want := range []string{"# CONTENTS-HEADING", "[CONTENTS-LINK](" + site.URL + "/contents-link)", "| CELL-A | CELL-B |",
		"- LI-ONE", "VIS-REVEALED", "DEEP-VISIBLE", "nested too deep", "line one\nline two"} {
		if !inFence(md, want) {
			t.Errorf("markdown: %q isn't in the fence:\n%s", want, md)
		}
	}
	for _, gone := range []string{"TYPED-IN-CONTENTS", "TA-CONTENTS-DEFAULT", "ATTR-VALUE-IN-CONTENTS", "DEEP-INPUT-VALUE", "NONE-THEN-CONTENTS", "VIS-HIDDEN "} {
		if strings.Contains(md, gone) {
			t.Errorf("markdown has %s:\n%s", gone, md)
		}
	}
	main := a.contentText(s.ID, map[string]any{"format": "readability"})
	if !inFence(main, "Article body") || strings.Contains(main, "NAV-IN-CONTENTS") {
		t.Errorf("readability:\n%s", main)
	}
}

// TestContentInVisualMode: what needs CDP answers 409 and attaches
// nothing; a screenshot works.
func TestContentInVisualMode(t *testing.T) {
	a := newRealAPI(t)
	name := fmt.Sprintf("cv%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	for _, format := range []string{"markdown", "readability", "pdf"} {
		if code, _, raw := a.content(s.ID, map[string]any{"format": format}); code != http.StatusConflict || !strings.Contains(string(raw), "Visual mode") {
			t.Errorf("%s in Visual mode: %d %s", format, code, raw)
		}
	}
	code, _, shot := a.content(s.ID, map[string]any{"format": "screenshot"})
	if _, err := png.DecodeConfig(bytes.NewReader(shot)); code != http.StatusOK || err != nil {
		t.Errorf("screenshot in Visual mode: %d %v", code, err)
	}
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("in Visual mode, something attached to %s", tg.URL)
		}
	}
}

// TestContentStaysQuiet runs each format 20 times against the continuous
// detector, which must catch its positive control. Markdown, readability
// and screenshots must leave nothing; a PDF is a print, which the page
// sees as it sees a person's ctrl+P (images/chromium/ANTI-BOT.md), and
// nothing else.
func TestContentStaysQuiet(t *testing.T) {
	a := newRealAPI(t)
	w := browsertest.NewWatch(t, siteHost(t))
	s := a.startAPIBrowser(t, "cq")
	a.mustTool(s.ID, "navigate", map[string]string{"url": w.URL})
	w.Ready(t)
	time.Sleep(2 * time.Second)
	w.Settle()
	if k := w.Kinds(); len(k) > 0 {
		t.Fatalf("the detector sees something before any content call: %v", k)
	}
	for _, body := range []map[string]any{{"format": "markdown"}, {"format": "readability"}, {"format": "screenshot"}} {
		w.Reset()
		for range 20 {
			if code, _, raw := a.content(s.ID, body); code != http.StatusOK {
				t.Fatalf("%v: %d %s", body, code, raw)
			}
		}
		w.Settle()
		if k := w.Kinds(); len(k) > 0 {
			t.Errorf("20 × %v: the detector saw %v", body, k)
		} else {
			t.Logf("20 × %v: nothing seen", body)
		}
	}
	w.Reset()
	for range 20 {
		if code, _, raw := a.content(s.ID, map[string]any{"format": "pdf"}); code != http.StatusOK {
			t.Fatalf("pdf: %d %s", code, raw)
		}
	}
	w.Settle()
	t.Logf("20 × pdf: the detector saw %v", w.Kinds())
	for _, k := range w.Kinds() {
		if k != "print@main" {
			t.Errorf("20 × pdf: the detector saw %s, more than a print", k)
		}
	}
	if !w.HasKind("print") {
		t.Errorf("printing went unseen: the detector's print check doesn't work, or Chromium changed")
	}
	// The positive control: what Playwright does.
	w.Reset()
	p := browsertest.Page(t, browsertest.ControlClient(a.m.Layout().ControlSocket(s.Profile)))
	browsertest.WatchPositiveControl(t, p)
	w.Settle()
	p.Close()
	t.Logf("positive control: %v", w.Kinds())
	if !w.HasKind("main-world-call") || !w.HasKind("automation-globals") {
		t.Errorf("the detector missed its positive control (%v); its silence above means nothing", w.Kinds())
	}
}
