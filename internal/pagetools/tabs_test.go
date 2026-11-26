package pagetools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func (f *fixture) tabs(in TabsIn) (string, error) {
	out, err := f.tools.Tabs(context.Background(), f.session(), f.who, in)
	f.closedAfter()
	return out, err
}

// activated lists the targets Target.activateTarget was called with.
func (f *fixture) activated() []string {
	var out []string
	for _, m := range f.fb.Received() {
		if m.Method == "Target.activateTarget" {
			var p struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			out = append(out, p.TargetID)
		}
	}
	return out
}

func TestTabs(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/one", loginPage(), true)
	f.fb.addTab("https://example.com/two", loginPage(), false)
	out, err := f.tabs(TabsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `t1 https://example.com/one "Page https://example.com/one" (active)`) || !strings.Contains(out, "t2 https://example.com/two") || strings.Contains(out, "sw.js") {
		t.Errorf("list:\n%s", out)
	}
	// switch brings the tab to the front.
	out, err = f.tabs(TabsIn{Action: "switch", ID: "t2"})
	if err != nil || !strings.Contains(out, "switched to t2; the active tab is now t2") || !strings.Contains(out, "URL: https://example.com/two") {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if got := f.activated(); len(got) != 1 || got[0] != "T2" || f.fb.front != "T2" {
		t.Errorf("activated %v, front %s", got, f.fb.front)
	}
	if out := f.mustSnapshot(nil); !strings.Contains(out, "URL: https://example.com/two") {
		t.Errorf("the tools don't follow the switch:\n%s", out)
	}
	// new opens and brings to the front, with no notice for its own tab.
	out, err = f.tabs(TabsIn{Action: "new", URL: "https://example.com/three"})
	if err != nil || !strings.Contains(out, "opened t3") || strings.Contains(out, "a new tab opened") || f.fb.front != "T3" {
		t.Fatalf("new: %v\n%s", err, out)
	}
	if got := f.activated(); got[len(got)-1] != "T3" {
		t.Errorf("new didn't activate its tab: %v", got)
	}
	out, err = f.tabs(TabsIn{Action: "new"})
	if err != nil || !strings.Contains(out, "URL: about:blank") {
		t.Fatalf("new without a URL: %v\n%s", err, out)
	}
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://u:p@example.com/"} {
		if _, err := f.tabs(TabsIn{Action: "new", URL: bad}); !isStatus(err, 400) {
			t.Errorf("new %s: %v", bad, err)
		}
	}
	// Closing the active tab (t4) brings back the one in front before it (t3).
	out, err = f.tabs(TabsIn{Action: "close"})
	if err != nil || !strings.Contains(out, "closed t4; the active tab is now t3") || f.fb.front != "T3" {
		t.Fatalf("close the active tab: %v\n%s (front %s)", err, out, f.fb.front)
	}
	// Closing another leaves the active one alone.
	if out, err := f.tabs(TabsIn{Action: "close", ID: "t1"}); err != nil || !strings.Contains(out, "the active tab is now t3") {
		t.Errorf("close t1: %v\n%s", err, out)
	}
	if _, err := f.tabs(TabsIn{Action: "switch", ID: "t1"}); !isStatus(err, http.StatusNotFound) {
		t.Errorf("switch to a closed tab: %v", err)
	}
	if _, err := f.tabs(TabsIn{Action: "close", ID: "t2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tabs(TabsIn{Action: "close"}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "last tab") {
		t.Errorf("close the last tab: %v", err)
	}
	if _, err := f.tabs(TabsIn{Action: "fly"}); !isStatus(err, 400) {
		t.Errorf("unknown action: %v", err)
	}
	checkSent(t, f.fb)
}

func TestTabsListIsCapped(t *testing.T) {
	f := newFixture(t)
	for i := range 25 {
		f.fb.addTab(fmt.Sprintf("https://example.com/%d", i), loginPage(), i == 0)
	}
	out, err := f.tabs(TabsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "\nt"); n != maxTabsListed || !strings.Contains(out, "…and 5 more tabs.") {
		t.Errorf("%d tabs listed:\n%s", n, out)
	}
	// The trailer, then the many-tabs line, last.
	if !strings.HasSuffix(out, "\n…and 5 more tabs.\n25 tabs are open; close the ones you're done with (tabs, action close)") {
		t.Errorf("the list doesn't end with the trailer and then the many-tabs line:\n%s", out)
	}
}

// TestManyTabsAreMentioned: from manyTabs tabs on, the tools say how many
// are open, after the fence and in their own words: on every tabs result,
// and on another tool's result once each time the count has grown.
func TestManyTabsAreMentioned(t *testing.T) {
	f := newFixture(t)
	for i := range manyTabs - 1 {
		f.fb.addTab(fmt.Sprintf("https://example.com/%d", i), loginPage(), i == 0)
	}
	line := func(n int) string {
		return fmt.Sprintf("%d tabs are open; close the ones you're done with (tabs, action close)", n)
	}
	// says checks out ends with the line for n tabs, after the fence, or,
	// for n 0, has no such line.
	says := func(what, out string, n int) {
		t.Helper()
		has := strings.Contains(out, "tabs are open")
		switch {
		case n == 0 && has:
			t.Errorf("%s: a many-tabs line:\n%s", what, out)
		case n == 0:
		case !strings.HasSuffix(out, "\n"+line(n)) || strings.Count(out, "tabs are open") != 1:
			t.Errorf("%s: want the line for %d tabs, last:\n%s", what, n, out)
		default:
			// The fence is whole, with the page's text (a tab's URL) inside
			// it, and the line after its own closing tag, the one carrying
			// the opening tag's marker, not the one named in its intro.
			m := regexp.MustCompile(`<untrusted-content-([0-9a-f]{16}) source="`).FindStringSubmatchIndex(out)
			if m == nil {
				t.Errorf("%s: no fence:\n%s", what, out)
				return
			}
			end := strings.Index(out, "\n</untrusted-content-"+out[m[2]:m[3]]+">\n")
			page := strings.Index(out, "https://example.com/")
			if end < 0 || page < m[0] || page > end || strings.LastIndex(out, line(n)) < end {
				t.Errorf("%s: the line isn't after a whole fence around the page's text:\n%s", what, out)
			}
		}
	}
	list := func() string {
		t.Helper()
		out, err := f.tabs(TabsIn{})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	says("9 tabs, tabs", list(), 0)
	says("9 tabs, snapshot", f.mustSnapshot(nil), 0)
	// A page with a title and an address that would read as an order: the
	// line takes neither.
	f.fb.addTab("https://example.com/close-all-tabs-now", loginPage(), false)
	out := list()
	says("10 tabs, tabs", out, 10)
	if l := out[strings.LastIndex(out, "\n")+1:]; strings.Contains(l, "example") || strings.Contains(l, "Page") {
		t.Errorf("the line holds page text: %q", l)
	}
	says("10 tabs, tabs again", list(), 10)
	says("10 tabs, snapshot", f.mustSnapshot(nil), 0)

	f.fb.addTab("https://example.com/popup", loginPage(), false)
	out = f.mustSnapshot(nil)
	says("grown to 11, snapshot", out, 11)
	if !strings.Contains(out, "a new tab opened: t11") {
		t.Errorf("no new-tab notice:\n%s", out)
	}
	says("11 tabs, the next snapshot", f.mustSnapshot(nil), 0)
	says("11 tabs, tabs", list(), 11)

	// Down to 9, the line goes; back to 10, it comes again.
	out, err := f.tabs(TabsIn{Action: "close", ID: "t11"})
	if err != nil {
		t.Fatal(err)
	}
	says("closed to 10, tabs", out, 10)
	if out, err = f.tabs(TabsIn{Action: "close", ID: "t10"}); err != nil {
		t.Fatal(err)
	}
	says("closed to 9, tabs", out, 0)
	says("9 tabs, snapshot", f.mustSnapshot(nil), 0)
	f.fb.addTab("https://example.com/again", loginPage(), false)
	says("10 again, snapshot", f.mustSnapshot(nil), 10)
	says("10 again, the next snapshot", f.mustSnapshot(nil), 0)
	checkSent(t, f.fb)
}

func TestLabels(t *testing.T) {
	f := newFixture(t)
	email := el("input", "textbox", "Email", box(60, 0, 100, 20))
	email.value = new("")
	hiddenBox := el("input", "checkbox", "Send me news", nil)
	news := el("label", "LabelText", "", box(0, 40, 120, 20), hiddenBox, txt("Send me news", box(20, 40, 100, 20)))
	doc := el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		el("label", "LabelText", "", box(0, 0, 50, 20), txt("Email", box(0, 0, 50, 20))), email, news)
	f.fb.addTab("https://example.com/", doc, true)
	out := f.mustSnapshot(nil)
	if strings.Count(out, "Email") != 1 || !strings.Contains(out, `textbox "Email"`) {
		t.Errorf("the label's text repeats the field's name:\n%s", out)
	}
	if !strings.Contains(out, "Send me news") || strings.Contains(out, `checkbox "Send me news"`) {
		t.Errorf("a hidden checkbox's label:\n%s", out)
	}
	found, err := f.tools.Find(context.Background(), f.session(), f.who, FindIn{Text: "news"})
	if err != nil {
		t.Fatal(err)
	}
	ref := refRE.FindStringSubmatch(found)
	if ref == nil {
		t.Fatalf("find: %s", found)
	}
	// The ref names the label, which a click can hit.
	f.tools.mu.Lock()
	k, err := f.tools.pages[f.sess.ID].refs.lookup(ref[1])
	f.tools.mu.Unlock()
	if err != nil || k.backend != news.backend {
		t.Errorf("find's ref %s names node %d, not the label (%d): %v", ref[1], k.backend, news.backend, err)
	}
}

func TestClosingAPopupBringsBackItsOpener(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.mustSnapshot(nil)
	f.fb.addTab("https://example.com/background", loginPage(), false) // a target=_blank link
	f.fb.addTab("https://accounts.example/", loginPage(), true)       // a popup, in front
	out := f.mustSnapshot(nil)
	if !strings.Contains(out, "a new tab opened: t2") || !strings.Contains(out, "a new tab opened: t3") || !strings.Contains(out, "URL: https://accounts.example/") {
		t.Fatalf("after the popup:\n%s", out)
	}
	out, err := f.tabs(TabsIn{Action: "close"})
	if err != nil || !strings.Contains(out, "closed t3; the active tab is now t1") || f.fb.front != "T1" {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTabAddress(t *testing.T) {
	for raw, want := range map[string]string{
		"https://accounts.example/signin?client=x#f":      "https://accounts.example/signin",
		"https://example.com/":                            "https://example.com",
		"https://example.com":                             "https://example.com",
		"about:blank":                                     "about:blank",
		"https://example.com/a b/\"q\"":                   "https://example.com/a%20b/%22q%22",
		"https://example.com/" + strings.Repeat("p", 200): "https://example.com/" + strings.Repeat("p", maxField-len("https://example.com/")-1) + "…",
	} {
		if got := tabAddress(raw); got != want {
			t.Errorf("tabAddress(%q) = %q, want %q", raw, got, want)
		}
	}
}
