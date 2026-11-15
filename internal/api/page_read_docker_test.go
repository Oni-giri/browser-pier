//go:build docker

package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/screenshot"
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
	password := open("/password#top")
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
	// It says so, in the fence (PROJ-110); a fragment isn't a redirect.
	if i := strings.Index(got, "Redirected from: "+site.URL+"/redirect\n"); i < 0 || i > strings.LastIndex(got, "</untrusted-content-") {
		t.Errorf("no redirect note in the fence: %s", got)
	}
	if strings.Contains(password, "Redirected") {
		t.Errorf("a fragment counted as a redirect: %s", password)
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

// TestIsolatedWorldSeesResourceTiming is PROJ-122's step 0: wait_for's
// load condition reads the page's Resource Timing entries from an
// isolated world, which must see the requests the page's own scripts
// made in the main world.
func TestIsolatedWorldSeesResourceTiming(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "r")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/spa-load"})
	p := browsertest.Page(t, browsertest.ControlClient(a.m.Layout().ControlSocket(s.Profile)))
	deadline := time.Now().Add(10 * time.Second)
	for p.Eval("document.body.innerText") != "Ready" {
		if time.Now().After(deadline) {
			t.Fatalf("the page never got ready: %v", p.Eval("document.body.innerText"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	const count = `performance.getEntriesByType('resource').filter(e => e.name.includes('/slow?')).length`
	if got := p.Eval(count); got != float64(3) {
		t.Fatalf("the main world has %v /slow entries", got)
	}
	// A main-world global, which the isolated world must not see.
	p.Eval("window.proofMark = 1")
	var tree struct {
		FrameTree struct{ Frame struct{ ID string } }
	}
	_ = json.Unmarshal(p.Call("Page.getFrameTree", nil), &tree)
	var world struct{ ExecutionContextID int }
	_ = json.Unmarshal(p.Call("Page.createIsolatedWorld", map[string]any{"frameId": tree.FrameTree.Frame.ID, "worldName": "proof"}), &world)
	var r struct {
		Result struct{ Value any }
	}
	_ = json.Unmarshal(p.Call("Runtime.evaluate", map[string]any{"expression": count + ` + ':' + typeof window.proofMark`, "contextId": world.ExecutionContextID, "returnByValue": true}), &r)
	if r.Result.Value != "3:undefined" {
		t.Fatalf("the isolated world sees %v", r.Result.Value)
	}
}

// waitMet reports whether a wait's answer says its condition was met.
func waitMet(out string) bool {
	return strings.HasPrefix(out, "waited ") || strings.HasPrefix(out, "already true when wait started: ")
}

// waitedFor reads the seconds out of "waited 1.4 s for …".
func waitedFor(t *testing.T, out string) float64 {
	t.Helper()
	rest, ok := strings.CutPrefix(out, "waited ")
	num, _, ok2 := strings.Cut(rest, " s for ")
	v, err := strconv.ParseFloat(num, 64)
	if !ok || !ok2 || err != nil {
		t.Fatalf("not a wait's answer: %q", out)
	}
	return v
}

// TestWaitForAnswers is PROJ-122 on a real browser: wait_for says when
// the text was already there and how long it waited, and load waits for
// an app's fetches after its load, but not for a long poll.
func TestWaitForAnswers(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "w")
	open := func(path string) {
		t.Helper()
		a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + path})
	}

	// Already there.
	open("/hello")
	if out := a.mustTool(s.ID, "wait", map[string]any{"text": "Hello"}); out != `already true when wait started: text "Hello" is on the page` {
		t.Errorf("already there: %q", out)
	}
	// About a second later: the answer's seconds match the call's.
	open("/done-later")
	start := time.Now()
	out := a.mustTool(s.ID, "wait", map[string]any{"text": "Done"})
	took := time.Since(start).Seconds()
	t.Logf("done-later: %q, %.2f s by the clock", out, took)
	if w := waitedFor(t, out); w < 0.75 || w > 1.3 || w > took+0.05 || took-w > 0.3 {
		t.Errorf("waited %v s by the answer, %.2f s by the clock: %q", w, took, out)
	}

	// An app that loads, then fetches three times in a row (400 ms each)
	// before it renders: load ends 500 ms after the last fetch.
	start = time.Now()
	open("/spa-load")
	out = a.mustTool(s.ID, "wait", map[string]any{"load": true})
	took = time.Since(start).Seconds()
	t.Logf("spa-load: %q, %.2f s after navigating", out, took)
	if !strings.HasPrefix(out, "waited ") || !strings.HasSuffix(out, " s for the page to load and the network to go quiet") || took < 1.5 || took > 3 {
		t.Errorf("spa-load: %q, %.2f s after navigating", out, took)
	}
	if found := a.mustTool(s.ID, "find", map[string]any{"text": "Ready"}); !strings.Contains(found, "1 match.") {
		t.Errorf("find right after the load: %s", found)
	}
	// Quiet now: already true.
	if out := a.mustTool(s.ID, "wait", map[string]any{"load": true}); out != "already true when wait started: the page is loaded and the network quiet" {
		t.Errorf("spa-load again: %q", out)
	}

	// A fetch that never answers isn't waited for.
	open("/longpoll")
	start = time.Now()
	out = a.mustTool(s.ID, "wait", map[string]any{"load": true})
	t.Logf("longpoll: %q after %s", out, time.Since(start))
	if !waitMet(out) || time.Since(start) > 1700*time.Millisecond {
		t.Errorf("longpoll: %q after %s", out, time.Since(start))
	}

	// A ref that stays hidden: the timeout says so.
	open("/later")
	spinner := refFor(t, a.mustTool(s.ID, "find", map[string]any{"text": "loading spinner"}), "Loading spinner")
	out = a.mustTool(s.ID, "wait", map[string]any{"ref": spinner, "state": "hidden", "timeout_ms": 5000})
	if !waitMet(out) {
		t.Fatalf("spinner hidden: %q", out)
	}
	if out := a.mustTool(s.ID, "wait", map[string]any{"ref": spinner, "timeout_ms": 500}); out != "timed out after 0.5 s waiting for "+spinner+" to be visible; now: "+spinner+" is on the page but hidden" {
		t.Errorf("hidden: %q", out)
	}
	for _, act := range a.m.Activity(s.ID) {
		if strings.Contains(act.What, "Hello") || strings.Contains(act.What, "Done") {
			t.Errorf("the activity log has the text: %q", act.What)
		}
	}
}

// TestPageToolsClickableDivs is PROJ-124 on a real browser: a control
// built from a div or a span, which only its pointer cursor or tabindex
// gives away, is a generic line with a ref, once (a span Chromium leaves
// out of the AX tree too); what is inside it isn't listed again; a
// decorative pointer div, a link's children and a pointer div holding a
// real button aren't generics; act clicks the refs, and text and
// role=generic targets find the same elements.
func TestPageToolsClickableDivs(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "g")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/divs"})
	snap := a.mustTool(s.ID, "snapshot", nil)
	t.Logf("snapshot:\n%s", snap)
	lines := strings.Split(snap, "\n")
	count := func(match func(l string) bool) int {
		n := 0
		for _, l := range lines {
			if match(l) {
				n++
			}
		}
		return n
	}
	ref := regexp.MustCompile(`^[a-z][a-z0-9]e[0-9]+ `)
	for _, want := range []string{`generic "Archive (3)"`, `generic "Mark read"`, `generic "Star it"`, `link "More options"`, `button "Save"`} {
		if n := count(func(l string) bool { return ref.MatchString(l) && strings.HasSuffix(l, " "+want) }); n != 1 {
			t.Errorf("%d lines with a ref end in %s", n, want)
		}
	}
	// The inner spans, the text again, the link's children, the button's
	// div, the empty div: none of them is a line of its own.
	for _, what := range []string{"Archive", "(3)", "Mark read", "Star it", "More", "options", "Save"} {
		if n := count(func(l string) bool { return strings.Contains(l, what) }); n != 1 {
			t.Errorf("%q is on %d lines", what, n)
		}
	}
	if n := count(func(l string) bool { return strings.Contains(l, " generic ") }); n != 3 {
		t.Errorf("%d generic lines, want 3", n)
	}
	if count(func(l string) bool { return l == "Liked by 2 people." }) != 1 {
		t.Errorf("the text around the span is lost")
	}
	waitEvent := func(e string) bool {
		deadline := time.Now().Add(5 * time.Second)
		for !site.HasEvent(e) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		return site.HasEvent(e)
	}
	for _, c := range []struct{ line, event string }{{`generic "Archive (3)"`, "a"}, {`generic "Mark read"`, "b"}, {`generic "Star it"`, "c"}} {
		out := a.mustTool(s.ID, "act", map[string]any{"steps": []map[string]any{{"action": "click", "ref": refFor(t, snap, c.line)}}, "changes": false})
		if !waitEvent(c.event) {
			t.Errorf("clicking %s: %s; events %v", c.line, out, site.Events())
		}
	}
	// Targets name the generics as the snapshot does: text inside one is
	// the generic, and role=generic goes by the text it shows.
	for _, c := range []struct{ target, line string }{
		{"text=Archive", `generic "Archive (3)"`},
		{`role=generic[name="Mark read"]`, `generic "Mark read"`},
		{"text=Star it", `generic "Star it"`},
	} {
		out := a.mustTool(s.ID, "act", map[string]any{"steps": []map[string]any{{"action": "click", "target": c.target}}, "changes": false})
		if !strings.Contains(out, "clicked "+refFor(t, snap, c.line)+" ") {
			t.Errorf("%s: %s", c.target, out)
		}
	}
	for _, e := range []string{"decor", "card", "save", "link"} {
		if site.HasEvent(e) {
			t.Errorf("%s was clicked: %v", e, site.Events())
		}
	}
}

// TestCloudflareChallengeNote is PROJ-120 on a real browser: navigate,
// snapshot, find and page_content spot a challenge page and say where its
// checkbox is; a /computer click there lands on it, at instant pace and,
// with the page zoomed, at human pace; the MCP pixels are the MCP tools' own scaling;
// a site's own Turnstile gets no note.
func TestCloudflareChallengeNote(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "cf")
	sw, sh := a.screenSize(t, s.ID)
	at := regexp.MustCompile(`computer click at screen x=(\d+), y=(\d+) \(MCP: click at (\d+), (\d+) in screenshot pixels`)
	// note checks the answer has the note in its fence and returns its
	// screen point.
	note := func(what, out string) (int, int) {
		t.Helper()
		m := at.FindStringSubmatch(out)
		i, end := strings.Index(out, "Cloudflare challenge."), strings.LastIndex(out, "</untrusted-content-")
		if m == nil || i < 0 || i > end {
			t.Fatalf("%s: no note in the fence:\n%s", what, out)
		}
		n := make([]int, 4)
		for k := range n {
			n[k], _ = strconv.Atoi(m[k+1])
		}
		// The screenshot's pixels are the MCP tools' own scaling of it.
		sc := screenshot.Screen{W: sw, H: sh}
		if mx, my := sc.FromScreen(n[0], n[1]); mx != n[2] || my != n[3] {
			t.Errorf("%s: MCP pixels %d, %d; want %d, %d", what, n[2], n[3], mx, my)
		}
		return n[0], n[1]
	}
	// click clicks there at a pace and checks it landed on the checkbox,
	// within 4 px of its middle (7 px at human pace, whose landing scatters
	// up to about 3 px), as the page reports both.
	click := func(what string, x, y int, pace string) {
		t.Helper()
		before := len(site.Events())
		near := 4.0
		if pace == "human" {
			near = 7
		}
		body := fmt.Sprintf(`{"action": "click", "x": %d, "y": %d, "pace": %q}`, x, y, pace)
		if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/computer", body, nil); code != http.StatusNoContent {
			t.Fatalf("%s: click: %d", what, code)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			for _, e := range site.Events()[before:] {
				var cx, cy, bx, by float64
				if _, err := fmt.Sscanf(e, "cf-click %g %g %g %g", &cx, &cy, &bx, &by); err != nil {
					continue
				}
				if math.Abs(cx-bx) > near || math.Abs(cy-by) > near {
					t.Errorf("%s: the click at screen %d, %d landed at %g, %g; the checkbox is at %g, %g", what, x, y, cx, cy, bx, by)
				}
				return
			}
		}
		t.Fatalf("%s: no click reached the page; events %v", what, site.Events())
	}

	x, y := note("navigate", a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/cf-challenge"}))
	click("at 100%", x, y, "instant")
	for _, out := range []string{
		a.mustTool(s.ID, "snapshot", nil),
		a.mustTool(s.ID, "find", map[string]any{"text": "verify you are human"}),
		a.contentText(s.ID, map[string]any{"format": "markdown"}),
		a.contentText(s.ID, map[string]any{"format": "readability"}),
	} {
		if sx, sy := note("a read", out); sx != x || sy != y {
			t.Errorf("the reads disagree: %d, %d and %d, %d", sx, sy, x, y)
		}
	}

	// Zoomed in (ctrl+= twice: 125%), the point moves and still lands, at
	// human pace too.
	for range 2 {
		if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/computer", `{"action": "key", "keys": "ctrl+equal", "pace": "instant"}`, nil); code != http.StatusNoContent {
			t.Fatalf("zoom: %d", code)
		}
	}
	var zx, zy int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if zx, zy = note("zoomed snapshot", a.mustTool(s.ID, "snapshot", map[string]any{"max_chars": 0})); zx != x || time.Now().After(deadline) {
			break
		}
	}
	t.Logf("the checkbox at 100%%: %d, %d; at 125%%: %d, %d (screen %dx%d)", x, y, zx, zy, sw, sh)
	if zx == x && zy == y {
		t.Errorf("zooming didn't move the point (%d, %d)", x, y)
	}
	click("at 125%", zx, zy, "human")
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/computer", `{"action": "key", "keys": "ctrl+0", "pace": "instant"}`, nil); code != http.StatusNoContent {
		t.Fatalf("unzoom: %d", code)
	}

	// A site's own Turnstile, shaped as Turnstile renders it, isn't a
	// challenge page; nor is one whose widget has been answered.
	for _, out := range []string{
		a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/turnstile-login"}),
		a.mustTool(s.ID, "snapshot", nil),
		a.contentText(s.ID, map[string]any{"format": "markdown"}),
	} {
		if strings.Contains(out, "Cloudflare") {
			t.Errorf("a Turnstile in a form has the note:\n%s", out)
		}
	}
	if out := a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/cf-challenge-passed"}); strings.Contains(out, "Cloudflare") {
		t.Errorf("an answered challenge has the note:\n%s", out)
	}
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
