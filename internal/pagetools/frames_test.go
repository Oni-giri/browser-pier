package pagetools

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// consentPage is a news page under a consent banner: an iframe from
// another site, fixed on top of the page's own buttons (PROJ-110).
type consentPage struct {
	doc                            *fnode
	agree, read, above, background *fnode
	frame, through, behind         *fnode
	agreeClicks                    int
}

func newConsentPage(fb *fakeBrowser) *consentPage {
	p := &consentPage{}
	button := func(name string, b *[4]float64, paint int) *fnode {
		n := el("button", "button", name, b, txt(name, b))
		n.paint = paint
		return n
	}
	// Under the banner: the page's own consent button and a headline link.
	p.agree = button("Yes, I agree", box(500, 600, 120, 40), 1)
	p.read = el("a", "link", "Headline", box(100, 650, 200, 20), txt("Headline", box(100, 650, 200, 20)))
	p.read.paint = 1
	// Painted after the banner, inside its box: a chat button on top.
	p.above = button("Chat", box(1200, 700, 60, 60), 9)
	// Out of the banner's box.
	p.background = button("Menu", box(10, 10, 60, 30), 1)
	p.frame = el("iframe", "Iframe", "SP Consent Message", box(0, 500, 1280, 300)).attr("src", "https://cdn.consent.example/index.html?x=1")
	p.frame.remoteURL = "https://cdn.consent.example/index.html?x=1"
	p.frame.paint = 5
	// An iframe clicks go through, and one painted under its parent's
	// content (a negative z-index): neither covers anything.
	p.through = el("iframe", "Iframe", "Ad", box(0, 0, 1280, 100)).attr("src", "https://ads.example/")
	p.through.paint, p.through.pointer = 6, "none"
	p.behind = el("div", "", "", box(0, 0, 1280, 100), el("iframe", "Iframe", "Video", box(0, 0, 1280, 100)).with(func(n *fnode) { n.paint = 7 }))
	p.behind.zIndex = "-1"
	fb.behave[p.agree] = &behaviour{cover: p.frame, onClick: func() { p.agreeClicks++ }}
	p.doc = el("#document", "RootWebArea", "News", box(0, 0, 1280, 800),
		p.background, p.read, p.agree, p.above, p.frame, p.through, p.behind)
	return p
}

func TestSnapshotMarksControlsUnderAnIframe(t *testing.T) {
	f := newFixture(t)
	p := newConsentPage(f.fb)
	f.fb.addTab("https://news.example/", p.doc, true)
	out := f.mustSnapshot(nil)
	for _, want := range []string{`button "Yes, I agree" [under an iframe]`, `link "Headline" [under an iframe]`, `iframe https://cdn.consent.example (not read)`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, clear := range []string{`button "Chat"`, `button "Menu"`} {
		if !strings.Contains(out, clear+"\n") {
			t.Errorf("%s is marked, or missing:\n%s", clear, out)
		}
	}
	checkSent(t, f.fb)
}

// Without paint orders (an older browser), nothing is marked.
func TestNoPaintOrderNoMarks(t *testing.T) {
	doc := newConsentPage(newFakeBrowser(t)).doc
	d, nodes := readFake(t, doc)
	d.s.Documents[0].Layout.PaintOrders = nil
	items, _, _, err := join(context.Background(), nodes, d, [4]float64{0, 0, 1280, 800})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if len(it.states) > 0 {
			t.Errorf("%s %q is marked %v", it.role, it.name, it.states)
		}
	}
}

func TestActSaysAnIframeCoversARef(t *testing.T) {
	f := newFixture(t)
	p := newConsentPage(f.fb)
	f.fb.addTab("https://news.example/", p.doc, true)
	r := f.refs()
	_, err := f.act(Step{Action: "click", Ref: r["Yes, I agree"]})
	if !isStatus(err, http.StatusConflict) {
		t.Fatalf("got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"Step 1 (click) failed: " + r["Yes, I agree"] + " is covered by <untrusted-content-",
		`iframe "SP Consent Message" from https://cdn.consent.example`,
		"an iframe in the way", "request_handoff", "POST /api/sessions/{id}/handoff", "start_browser with mode visual",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
	// The advice is BrowserPier's, after the fence; the frame's query isn't shown.
	if i, j := strings.Index(msg, "request_handoff"), strings.LastIndex(msg, "</untrusted-content-"); i < j {
		t.Errorf("the advice is inside the fence:\n%s", msg)
	}
	if strings.Contains(msg, "x=1") {
		t.Errorf("the frame's query is shown:\n%s", msg)
	}
	if p.agreeClicks != 0 {
		t.Error("the covered button was clicked")
	}
	// Something else on top (not a frame) gets no such advice.
	ap := newActPage(f.fb)
	f.fb.addTab("https://example.com/", ap.doc, true)
	r = f.refs()
	_, err = f.act(Step{Action: "click", Ref: r["Buy now"]})
	if err == nil || strings.Contains(err.Error(), "request_handoff") {
		t.Errorf("an overlay that isn't a frame: %v", err)
	}
}

// act names a covering iframe by the document it shows, not by its src,
// which the page sets and can change the frame's address after; a src
// alone, relative, is not echoed (PROJ-110).
func TestActNamesAFrameByItsDocument(t *testing.T) {
	cases := []struct {
		name              string
		src               string
		docURL, remoteURL string
		want, notWant     string
	}{
		{name: "navigated away, out of process", src: "https://accounts.google.com/signin?x=1",
			remoteURL: "https://evil.example/landing?y=2", want: "from https://evil.example\n", notWant: "google"},
		{name: "navigated away, in process", src: "https://accounts.google.com/signin",
			docURL: "https://news.example/frame?y=2", want: "from https://news.example\n", notWant: "google"},
		{name: "relative src, document unknown", src: "/embed?session=abc", want: "from an unknown origin\n", notWant: "session=abc"},
		{name: "about:blank", src: "/embed?session=abc", docURL: "about:blank", want: "from about:blank\n", notWant: "session=abc"},
		{name: "a data: document", docURL: "data:text/html,<p>secret</p>", want: "from data:\n", notWant: "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p := newConsentPage(f.fb)
			p.frame.attrs = nil
			if tc.src != "" {
				p.frame.attr("src", tc.src)
			}
			p.frame.docURL, p.frame.remoteURL = tc.docURL, tc.remoteURL
			f.fb.addTab("https://news.example/", p.doc, true)
			r := f.refs()
			_, err := f.act(Step{Action: "click", Ref: r["Yes, I agree"]})
			if !isStatus(err, http.StatusConflict) {
				t.Fatalf("got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, `iframe "SP Consent Message" `+tc.want) {
				t.Errorf("missing %q in\n%s", tc.want, msg)
			}
			if strings.Contains(msg, tc.notWant) {
				t.Errorf("%q is shown:\n%s", tc.notWant, msg)
			}
			checkSent(t, f.fb)
		})
	}
}
