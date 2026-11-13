package pagetools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/pagetext"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// The content endpoints (PROJ-20): the active tab as Markdown, as its
// main text alone, or as a PDF. They are calls like the page tools' (a
// lease, one short-lived connection, nothing enabled), with their own
// allow-list: they read the page and print it, and can't navigate, type
// or touch the DOM. Markdown and the main text come from a DOM snapshot,
// rendered in Go (internal/pagetext): no script of BrowserPier's runs to
// read them. Screenshots aren't here: they come from the X display, in
// both modes, with no CDP at all (internal/api).
var contentAllowList = cdp.NewAllowList("content endpoints",
	"Target.getTargets", "Target.attachToTarget", "Target.detachFromTarget",
	"Page.getFrameTree", "Page.createIsolatedWorld", "Page.getNavigationHistory",
	// The active tab is found as the page tools find it: its visibility,
	// read in an isolated world (its `this`, then a function on it).
	"Runtime.evaluate", "Runtime.callFunctionOn",
	"DOMSnapshot.captureSnapshot", "Page.printToPDF",
)

// ContentAllowedMethods lists the CDP methods the content endpoints may
// send, for docs and tests.
func ContentAllowedMethods() []string { return contentAllowList.Methods() }

const (
	// DefaultContentChars is how much text a content call returns unless
	// asked; MaxContentChars caps what it may ask. Longer pages are read
	// in parts with offset.
	DefaultContentChars = 20000
	MaxContentChars     = 100000
	// maxRendered caps the text rendered from one page, so a huge page
	// can't make the orchestrator build megabytes it will never send.
	maxRendered = 2 << 20
	// maxContentNodes caps the DOM a content call renders. It is higher
	// than the snapshot's cap: the DOM snapshot alone is about four times
	// cheaper than the AX tree (see the spike).
	maxContentNodes = 200000
	// MaxPDF caps a PDF's size: ask for fewer pages beyond it.
	MaxPDF = 10 << 20
	// pdfTimeout is how long printing may take.
	pdfTimeout = 60 * time.Second
)

// contentStyles are the computed styles content reads: display and
// visibility first, as dom.shown expects, then white-space.
var contentStyles = []string{"display", "visibility", "white-space"}

// TextIn is the input of the text formats.
type TextIn struct {
	// Format is "markdown" (the whole page) or "readability" (its main
	// text, as plain text).
	Format   string
	MaxChars int
	Offset   int
}

// errContentVisual refuses what needs CDP in Visual mode.
var errContentVisual = &Error{Status: http.StatusConflict, Msg: "Markdown, readability text and PDF read the page over CDP, which nothing may attach to in Visual mode. Use format screenshot, or restart the browser with mode api"}

// Text returns the active tab as Markdown or as its main text, fenced,
// from offset, at most max_chars characters, with a line after the fence
// saying how to read on.
func (t *Tools) Text(ctx context.Context, s sessions.Session, who Caller, in TextIn) (string, error) {
	limit := in.MaxChars
	if limit == 0 {
		limit = DefaultContentChars
	}
	switch {
	case in.Format != "markdown" && in.Format != "readability":
		return "", &Error{Status: 400, Msg: "format: markdown, readability, screenshot or pdf"}
	case limit < 1 || limit > MaxContentChars:
		return "", &Error{Status: 400, Msg: fmt.Sprintf("max_chars: want 1 to %d", MaxContentChars)}
	case in.Offset < 0:
		return "", &Error{Status: 400, Msg: "offset: want 0 or more"}
	case s.Mode != profiles.ModeAPI:
		return "", errContentVisual
	}
	return runWith(ctx, t, s, who, contentAllowList, func(c *call) (string, error) {
		// The slot is held through rendering: the snapshot and what is
		// made from it are in memory until then.
		release, err := c.readSlot(c.lease.Ctx)
		if err != nil {
			return "", err
		}
		defer release()
		root, base, header, err := c.readTree(c.lease.Ctx)
		if err != nil {
			return "", err
		}
		// Rendering is linear, but a handoff, a stop or the caller going
		// away still ends it, and it never runs long.
		rctx, cancel := context.WithTimeout(c.lease.Ctx, c.t.slow)
		defer cancel()
		var body, source, what string
		var full bool
		if in.Format == "markdown" {
			body, full, err = pagetext.Markdown(rctx, root, base, maxRendered)
			source, what = "page as Markdown", "Read the page as Markdown"
		} else {
			body, full, err = pagetext.Readable(rctx, root, maxRendered)
			source, what = "page's main text", "Read the page's main text"
		}
		if err != nil {
			if c.lease.Ctx.Err() == nil {
				return "", &Error{Status: 422, Msg: "page too large to read; use find"}
			}
			return "", err
		}
		total := utf8.RuneCountInString(body)
		if in.Offset > total || in.Offset > 0 && in.Offset == total {
			return "", &Error{Status: 400, Msg: fmt.Sprintf("offset: the text has %s characters", thousands(total))}
		}
		c.note(what)
		part, end := window(body, in.Offset, limit)
		if in.Format == "markdown" && part != "" {
			// A part that starts inside a code block opens it again.
			if open := openFence(body, in.Offset); open != "" {
				part = open + "\n" + part
			}
		}
		trailer := ""
		switch {
		case in.Offset > 0 || end < total:
			trailer = fmt.Sprintf("Characters %s to %s of %s.", thousands(in.Offset), thousands(end), thousands(total))
			if end < total {
				trailer += fmt.Sprintf(" Read on with offset %d.", end)
			}
		case body == "":
			trailer = "The page shows no text."
		}
		if full && end >= total {
			trailer = strings.TrimSpace(trailer + fmt.Sprintf(" The page has more than BrowserPier renders (%s characters); use find for the rest.", thousands(total)))
		}
		text := header
		if in.Offset == 0 && part != "" {
			text += "\n\n" + part
		} else if part != "" {
			text += "\n\n…\n" + part
		}
		return c.result(source, text, trailer), nil
	})
}

// window cuts s to at most n characters from offset, at a line's end
// when one is close, and says where it ended.
func window(s string, offset, n int) (string, int) {
	r := []rune(s)
	if offset >= len(r) {
		return "", len(r)
	}
	end := min(len(r), offset+n)
	if end < len(r) {
		for i := end; i > offset+n*4/5; i-- {
			if r[i-1] == '\n' {
				end = i
				break
			}
		}
	}
	return strings.TrimRight(string(r[offset:end]), "\n"), end
}

// openFence is the line that opened the fenced code block offset (in
// characters) falls inside, "" if it isn't in one. Fences are what
// pagetext writes: a run of three or more backticks, after any list
// indent or quote marks.
func openFence(s string, offset int) string {
	r := []rune(s)
	if offset <= 0 || offset > len(r) {
		return ""
	}
	open, fence := "", ""
	for _, l := range strings.Split(string(r[:offset]), "\n") {
		l = strings.TrimLeft(l, " >")
		switch {
		case open == "" && strings.HasPrefix(l, "```"):
			open, fence = l, strings.TrimRight(l, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_+#-")
		case open != "" && l == fence:
			open, fence = "", ""
		}
	}
	return open
}

// readTree takes a DOM snapshot of the active tab and turns what it lays
// out into a pagetext tree. The tab's document is checked before and
// after: a snapshot that straddles a navigation is taken again, once.
func (c *call) readTree(ctx context.Context) (*pagetext.Node, *url.URL, string, error) {
	for try := 0; ; try++ {
		_, before, _, err := c.frameTree(ctx, c.tgt)
		if err != nil {
			return nil, nil, "", err
		}
		d, err := c.readContentDOM(ctx)
		if err != nil {
			return nil, nil, "", err
		}
		_, after, _, err := c.frameTree(ctx, c.tgt)
		if err != nil {
			return nil, nil, "", err
		}
		if before != after {
			if try == 0 {
				continue
			}
			return nil, nil, "", &Error{Status: http.StatusConflict, Msg: "the page kept changing while it was read; try again"}
		}
		doc := &d.s.Documents[0]
		pageURL := d.str(doc.DocumentURL)
		base, err := url.Parse(d.str(doc.BaseURL))
		if err != nil || !base.IsAbs() {
			base, _ = url.Parse(pageURL)
		}
		header := "URL: " + capText(pageURL, maxHeader) + "\nTitle: " + capText(strings.Join(strings.Fields(d.str(doc.Title)), " "), maxHeader)
		// The fence drops HTML comments: a "<!--" in a title would take
		// the page's text with it.
		header = strings.ReplaceAll(header, "<!--", `<\!--`)
		return contentTree(d), base, header, nil
	}
}

// readContentDOM takes the DOM snapshot content renders from.
func (c *call) readContentDOM(ctx context.Context) (*dom, error) {
	var s domSnapshot
	if err := c.renderer(ctx, c.t.slow, "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": contentStyles, "includeDOMRects": true}, &s); err != nil {
		if errors.Is(err, cdp.ErrTooLarge) {
			return nil, &Error{Status: 422, Msg: "page too large to read; use find"}
		}
		return nil, err
	}
	if len(s.Documents) == 0 || len(s.Documents[0].Nodes.BackendNodeID) == 0 {
		return nil, &Error{Status: 502, Msg: "the page has no document yet; try again"}
	}
	if len(s.Documents[0].Nodes.BackendNodeID) > maxContentNodes {
		return nil, &Error{Status: 422, Msg: "page too large to read; use find"}
	}
	return indexDOM(&s), nil
}

// contentAttrs are the attributes pagetext reads.
var contentAttrs = map[string]bool{"href": true, "src": true, "alt": true, "class": true, "id": true, "role": true, "start": true, "colspan": true, "rowspan": true}

// contentTree builds the pagetext tree of the snapshot's top document,
// in the order the page shows it (the snapshot walks the flat tree:
// shadow roots and slots are already in place). Text that isn't laid out
// or is hidden (display:none, visibility:hidden, zero-size) is left out,
// as are images and iframes that aren't shown. Form fields' values are
// never copied: pagetext leaves the fields out.
func contentTree(d *dom) *pagetext.Node {
	doc := d.s.Documents[0]
	nodes := make([]*pagetext.Node, len(doc.Nodes.NodeType))
	root := &pagetext.Node{Tag: "#document"}
	for i, typ := range doc.Nodes.NodeType {
		var n *pagetext.Node
		switch typ {
		case 9: // the document itself
			nodes[i] = root
			continue
		case 1:
			if _, laidOut := d.layout[i]; !laidOut {
				// Not laid out: display:none and what is under it (its
				// texts aren't shown either), or display:contents, whose
				// children go to its parent. Either way the element
				// itself adds nothing, and a hidden chain of thousands
				// of elements costs nothing to render.
				nodes[i] = parentOf(nodes, d.parent(i), root)
				continue
			}
			tag := strings.ToLower(d.name(i))
			// XHTML can prefix names (h:input): the local name is the tag.
			if k := strings.LastIndexByte(tag, ':'); k >= 0 {
				tag = tag[k+1:]
			}
			n = &pagetext.Node{Tag: tag, Display: d.display(i), Pre: preWhiteSpace(d, i)}
			if attrs := doc.Nodes.Attributes; i < len(attrs) {
				for k := 0; k+1 < len(attrs[i]); k += 2 {
					if name := strings.ToLower(d.str(attrs[i][k])); contentAttrs[name] {
						if n.Attrs == nil {
							n.Attrs = map[string]string{}
						}
						n.Attrs[name] = d.str(attrs[i][k+1])
					}
				}
			}
			switch tag {
			case "img", "iframe":
				if _, ok := d.shown(i); !ok {
					n = nil
				} else if tag == "iframe" {
					n.Attrs = map[string]string{"src": frameOrigin(d, i)}
				}
			}
		case 3:
			text := d.str(doc.Nodes.NodeValue[i])
			if strings.TrimSpace(text) != "" {
				if _, ok := d.shown(i); !ok {
					break
				}
			}
			p := d.parent(i)
			n = &pagetext.Node{Text: text, Pre: p >= 0 && preWhiteSpace(d, p)}
		}
		if n == nil {
			continue
		}
		parent := parentOf(nodes, d.parent(i), root)
		if parent == nil {
			continue // under something left out
		}
		nodes[i] = n
		parent.Kids = append(parent.Kids, n)
	}
	return root
}

// parentOf is the tree node that snapshot node p's children go under:
// root for none, nil when p was left out.
func parentOf(nodes []*pagetext.Node, p int, root *pagetext.Node) *pagetext.Node {
	if p < 0 || p >= len(nodes) {
		return root
	}
	return nodes[p]
}

// preWhiteSpace reports whether node i's white-space keeps line breaks.
func preWhiteSpace(d *dom, i int) bool {
	li, ok := d.layout[i]
	if !ok {
		return false
	}
	st := d.s.Documents[0].Layout.Styles
	if li >= len(st) || len(st[li]) < 3 {
		return false
	}
	switch d.str(st[li][2]) {
	case "pre", "pre-wrap", "pre-line", "break-spaces":
		return true
	}
	return false
}

// PDFIn is the input of the pdf format.
type PDFIn struct {
	Landscape  bool
	Background bool
	// Pages is a page range like "1-5, 8", empty for all.
	Pages string
}

// pageRangeError matches Chromium's refusals of a page range ("Page range
// syntax error", "Page range exceeds page count").
var pageRangeError = regexp.MustCompile(`(?i)page range`)

var pageRanges = regexp.MustCompile(`^\s*\d{1,5}(\s*-\s*\d{0,5})?(\s*,\s*\d{1,5}(\s*-\s*\d{0,5})?)*\s*$`)

// PDF prints the active tab to PDF, as Chromium's print does: the page
// sees it print (beforeprint, afterprint, print media), as when a person
// presses ctrl+P; see images/chromium/ANTI-BOT.md.
func (t *Tools) PDF(ctx context.Context, s sessions.Session, who Caller, in PDFIn) ([]byte, error) {
	switch {
	case len(in.Pages) > 100 || in.Pages != "" && !pageRanges.MatchString(in.Pages):
		return nil, &Error{Status: 400, Msg: `pages: a range like "1-5, 8"`}
	case s.Mode != profiles.ModeAPI:
		return nil, errContentVisual
	}
	return runWith(ctx, t, s, who, contentAllowList, func(c *call) ([]byte, error) {
		// A PDF is held in memory as a whole (its base64, then its bytes):
		// it takes a read slot like a whole-page read.
		release, err := c.readSlot(c.lease.Ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		params := map[string]any{"landscape": in.Landscape, "printBackground": in.Background}
		if p := strings.TrimSpace(in.Pages); p != "" {
			params["pageRanges"] = strings.Join(strings.Fields(p), "")
		}
		var r struct {
			Data string `json:"data"`
		}
		err = c.renderer(c.lease.Ctx, pdfTimeout, "Page.printToPDF", params, &r)
		var ce *cdp.Error
		switch {
		case errors.Is(err, cdp.ErrTooLarge):
			return nil, &Error{Status: 422, Msg: fmt.Sprintf("the PDF is larger than %d MiB; ask for fewer pages", MaxPDF>>20)}
		case errors.As(err, &ce) && in.Pages != "" && pageRangeError.MatchString(ce.Message):
			return nil, &Error{Status: 400, Msg: "pages: the page has no such page range"}
		case errors.As(err, &ce) && strings.Contains(ce.Message, "navigated or closed"):
			return nil, &Error{Status: http.StatusConflict, Msg: "the page changed while it was printed; try again"}
		case err != nil:
			return nil, err
		}
		if base64.StdEncoding.DecodedLen(len(r.Data)) > MaxPDF+3 {
			return nil, &Error{Status: 422, Msg: fmt.Sprintf("the PDF is larger than %d MiB; ask for fewer pages", MaxPDF>>20)}
		}
		pdf, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil || !strings.HasPrefix(string(pdf), "%PDF-") {
			return nil, &Error{Status: http.StatusBadGateway, Msg: "the browser didn't print a PDF"}
		}
		if len(pdf) > MaxPDF {
			return nil, &Error{Status: 422, Msg: fmt.Sprintf("the PDF is larger than %d MiB; ask for fewer pages", MaxPDF>>20)}
		}
		c.note(fmt.Sprintf("Printed the page to PDF (%s kB)", thousands((len(pdf)+1023)/1024)))
		return pdf, nil
	})
}
