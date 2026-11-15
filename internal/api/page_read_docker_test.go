//go:build docker

package api

import (
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestPageToolsRead is PROJ-21's reading scenarios on a real browser:
// a big nav is summed up and the form still fits, an open modal comes
// first, hidden text is out and transparent text fenced, a page-filled
// password never shows, and navigate follows a redirect and refuses
// what it must.
func TestPageToolsRead(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "r")
	open := func(path string) string {
		t.Helper()
		return a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + path})
	}

	// 1. A 200-link nav and a cookie banner: the form is in, the nav summed up.
	open("/nav")
	snap := a.mustTool(s.ID, "snapshot", nil)
	for _, want := range []string{`textbox "Your email"`, `button "Subscribe"`, `button "Accept all"`, "navigation with 200 controls"} {
		if !strings.Contains(snap, want) {
			t.Errorf("nav page: missing %q in\n%s", want, snap)
		}
	}
	_ = refFor(t, snap, `button "Subscribe"`)
	if strings.Contains(snap, `link "Section 100"`) {
		t.Errorf("the nav is listed link by link:\n%s", snap)
	}
	found := a.mustTool(s.ID, "find", map[string]any{"text": "section 137"})
	if !strings.Contains(found, `link "Section 137"`) || !strings.Contains(found, "1 match.") {
		t.Errorf("find: %s", found)
	}

	// 2. An open modal comes first.
	open("/modal")
	snap = a.mustTool(s.ID, "snapshot", nil)
	if !strings.Contains(snap, "-- in the open dialog --") || strings.Index(snap, `button "Confirm"`) > strings.Index(snap+"-- in view --", "-- in view --") {
		t.Errorf("modal page:\n%s", snap)
	}

	// 10. Hidden text: CSS-hidden is absent; transparent text only fenced.
	open("/hidden")
	snap = a.mustTool(s.ID, "snapshot", nil)
	for _, gone := range []string{"DISPLAY-NONE-TEXT", "VISIBILITY-HIDDEN-TEXT", "ARIA-HIDDEN-TEXT", "ZERO-SIZE-TEXT", "ZERO-SIZE-BUTTON"} {
		if strings.Contains(snap, gone) {
			t.Errorf("%s is in the snapshot:\n%s", gone, snap)
		}
	}
	end := strings.LastIndex(snap, "</untrusted-content-")
	for _, kept := range []string{"OPACITY-ZERO-TEXT", "OFF-SCREEN-TEXT", "CLIPPED-TEXT", "LABELLEDBY-HIDDEN-NAME", "ARIA-LABEL-NAME"} {
		if i := strings.Index(snap, kept); i < 0 || i > end || !strings.HasPrefix(snap, "<untrusted-content-") {
			t.Errorf("%s isn't in the fence:\n%s", kept, snap)
		}
	}

	// 11. A password the page filled in never shows.
	open("/password")
	snap = a.mustTool(s.ID, "snapshot", nil)
	found = a.mustTool(s.ID, "find", map[string]any{"text": "password"})
	for _, out := range []string{snap, found} {
		if strings.Contains(out, "PAGE-FILLED-SECRET") || strings.Contains(out, "424242") {
			t.Errorf("a secret value leaked:\n%s", out)
		}
	}
	if !strings.Contains(snap, `"Password" = [filled]`) || !strings.Contains(snap, `"One-time code" = [filled]`) || !strings.Contains(snap, `"User" = "pier"`) {
		t.Errorf("password page:\n%s", snap)
	}

	// Navigate: a redirect is followed; other schemes are refused.
	got := open("/redirect")
	if !strings.Contains(got, "URL: "+site.URL+"/login") || !strings.Contains(got, "Title: Sign in") {
		t.Errorf("redirect: %s", got)
	}
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://u:p@example.com/"} {
		if code, msg := a.tool(s.ID, "navigate", map[string]string{"url": bad}); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, code, msg)
		}
	}
	if got := a.mustTool(s.ID, "navigate", map[string]string{"action": "back"}); !strings.Contains(got, "/password") {
		t.Errorf("back: %s", got)
	}
	for _, act := range a.m.Activity(s.ID) {
		if strings.Contains(act.What, "SECRET") || strings.Contains(act.What, "Section") {
			t.Errorf("the activity log has page text: %q", act.What)
		}
	}
	// Nothing stays attached between calls.
	for _, tg := range targets(t, a.m.Layout().ControlSocket(s.Profile)) {
		if tg.Attached {
			t.Errorf("%s is still attached", tg.URL)
		}
	}
	// The agent's own CDP client can be connected at the same time.
	p := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	if got := p.Eval("document.title"); got != "Saved password" {
		t.Errorf("the agent's own client reads %v", got)
	}
	if snap := a.mustTool(s.ID, "snapshot", nil); !strings.Contains(snap, "Title: Saved password") {
		t.Errorf("snapshot with the agent's client connected:\n%s", snap)
	}
	p.Close()
}

// TestPageToolsRefuseVisualMode is scenario 13: every page tool answers
// 409 in Visual mode, and nothing attaches to the page.
func TestPageToolsRefuseVisualMode(t *testing.T) {
	a := newRealAPI(t)
	name := fmt.Sprintf("v%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	for tool, body := range pageToolBodies {
		if code, msg := a.tool(s.ID, tool, body); code != http.StatusConflict {
			t.Errorf("%s in Visual mode: %d %s", tool, code, msg)
		}
	}
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("in Visual mode, something attached to %s", tg.URL)
		}
	}
}

// pageToolBodies is a valid call of each page tool.
var pageToolBodies = map[string]any{
	"snapshot": map[string]any{},
	"find":     map[string]any{"text": "x"},
	"navigate": map[string]any{"url": "https://example.com/"},
}

// TestPageToolsKeepEachItemOnItsLine is PROJ-95 on a real browser: a
// "<!--" in a page's title and text doesn't hide what follows it, and a
// newline in an iframe's malformed src or in a name can't start a line of
// its own, in snapshot, find and page_content alike.
func TestPageToolsKeepEachItemOnItsLine(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "ln")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/probe-lines"})
	ref := regexp.MustCompile(`^[a-z][a-z0-9]e[0-9]+ `)
	// forged reports the lines page text started: a heading, a line that
	// reads as a ref of its own, or a FAKE that isn't on its item's line.
	forged := func(what, out string, items ...string) {
		t.Helper()
		for _, l := range strings.Split(out, "\n") {
			bad := strings.HasPrefix(l, "#") || strings.HasPrefix(l, "e99 ")
			if strings.Contains(l, "FAKE") && !slices.ContainsFunc(items, func(it string) bool { return strings.Contains(l, it) }) {
				bad = true
			}
			if bad {
				t.Errorf("%s: page text made a line of its own: %q\n%s", what, l, out)
			}
		}
	}
	full := func(what, out string, extra ...string) {
		t.Helper()
		if strings.Contains(out, "<!--") {
			t.Errorf("%s: a comment opener survived:\n%s", what, out)
		}
		for _, want := range append([]string{`code sample: <\!-- x LINES-AFTER`, "LINES-NEXT-PARA", "LINES-END"}, extra...) {
			if !inFence(out, want) {
				t.Errorf("%s: %q isn't in the fence:\n%s", what, want, out)
			}
		}
	}

	snap := a.mustTool(s.ID, "snapshot", nil)
	full("snapshot", snap, `Title: Lines <\!-- title`, `button "LINES-BUTTON`, `navigation "LINES-NAV`, " iframe ")
	forged("snapshot", snap, " iframe ", `button "LINES-BUTTON`, `navigation "LINES-NAV`)
	if l := lineWith(snap, " iframe "); !ref.MatchString(l) || !strings.HasSuffix(l, " (not read)") {
		t.Errorf("snapshot: the iframe's line is %q\n%s", l, snap)
	}

	found := a.mustTool(s.ID, "find", map[string]any{"text": "lines-", "limit": 50})
	full("find", found)
	found = a.mustTool(s.ID, "find", map[string]any{"text": "fake"})
	forged("find", found, " iframe ", `button "LINES-BUTTON`)
	for _, l := range strings.Split(found, "\n") {
		if strings.Contains(l, "FAKE") && !ref.MatchString(l) {
			t.Errorf("find: a match's line has no ref: %q\n%s", l, found)
		}
	}

	for _, format := range []string{"markdown", "readability"} {
		out := a.contentText(s.ID, map[string]any{"format": format})
		full(format, out, `Title: Lines <\!-- title`)
		forged(format, out, "[iframe: ")
	}
}

// lineWith is the first line of s that has what.
func lineWith(s, what string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, what) {
			return l
		}
	}
	return ""
}
