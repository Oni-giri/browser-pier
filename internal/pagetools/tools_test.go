package pagetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// loginPage is a sign-in form in main, with a header link.
func loginPage() *fnode {
	email := el("input", "textbox", "Email", box(10, 100, 200, 20)).attr("id", "email").attr("name", "email")
	email.value = new("ana@example.com")
	pw := el("input", "textbox", "Password", box(10, 130, 200, 20)).attr("type", "password").attr("id", "pw")
	pw.value = new("hunter2")
	return el("#document", "RootWebArea", "Sign in", box(0, 0, 1280, 800),
		el("header", "banner", "", box(0, 0, 1280, 40), el("a", "link", "Home", box(0, 0, 40, 20), txt("Home", box(0, 0, 40, 20)))),
		el("main", "main", "", box(0, 40, 1280, 2000),
			el("h1", "heading", "Sign in", box(10, 50, 300, 30), txt("Sign in", box(10, 50, 100, 30))),
			email, pw,
			el("input", "checkbox", "Remember me", box(10, 160, 20, 20)).with(func(n *fnode) { n.props = map[string]string{"checked": "true"} }),
			el("button", "button", "Sign in", box(10, 190, 80, 30), txt("Sign in", box(10, 190, 80, 30))),
			el("p", "paragraph", "", box(10, 1500, 600, 20), txt("Far below the fold, in main.", box(10, 1500, 300, 20))),
		),
		el("footer", "contentinfo", "", box(0, 2100, 1280, 40), txt("Footer text elsewhere.", box(0, 2100, 300, 20))),
	)
}

func (f *fixture) snapshot(maxChars *int) (string, error) {
	return f.tools.Snapshot(context.Background(), f.session(), f.who, SnapshotIn{MaxChars: maxChars})
}

func (f *fixture) mustSnapshot(maxChars *int) string {
	f.t.Helper()
	out, err := f.snapshot(maxChars)
	if err != nil {
		f.t.Fatal(err)
	}
	f.closedAfter()
	return out
}

var refRE = regexp.MustCompile(`(?m)^([a-z][a-z0-9]e[0-9]+) `)

func TestSnapshotReadsTheActiveTab(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/login?next=%2Faccount", loginPage(), true)
	out := f.mustSnapshot(nil)
	if !strings.HasPrefix(out, "<untrusted-content-") || strings.Count(out, "</untrusted-content-") != 2 { // the preamble names the end tag too
		t.Fatalf("not one fenced block:\n%s", out)
	}
	for _, want := range []string{
		"URL: https://example.com/login?next=%2Faccount", "Title: Page https://example.com",
		`textbox "Email" = "ana@example.com"`, `textbox "Password" = [filled]`, `checkbox "Remember me" [checked]`,
		`button "Sign in"`, `link "Home"`, "Far below the fold, in main.", "Footer text elsewhere.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("the password's value is in the snapshot:\n%s", out)
	}
	// In view, then main, then the rest; controls before text in each.
	order := []string{"-- in view --", `link "Home"`, `button "Sign in"`, "Sign in\n", "-- in the main content --", "Far below", "-- elsewhere --", "Footer text"}
	at := 0
	for _, w := range order {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("%q out of order in\n%s", w, out)
		}
		at += i
	}
	refs := refRE.FindAllStringSubmatch(out, -1)
	if len(refs) != 5 {
		t.Fatalf("want 5 refs, got %v", refs)
	}
	epoch := refs[0][1][:2]
	for _, r := range refs {
		if r[1][:2] != epoch {
			t.Errorf("refs %s and %s have different epochs", refs[0][1], r[1])
		}
	}
	// The same nodes keep their refs; the label's text isn't repeated as text.
	again := f.mustSnapshot(nil)
	if fmt.Sprint(refRE.FindAllStringSubmatch(again, -1)) != fmt.Sprint(refs) {
		t.Errorf("refs changed between snapshots:\n%s\n%s", out, again)
	}
	if acts := f.m.Activity(f.sess.ID); len(acts) == 0 || acts[0].What != "Read the page" {
		t.Errorf("activity: %+v", acts)
	}
}

func TestSnapshotOnlySendsAllowedMethods(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.mustSnapshot(nil)
	if _, err := f.tools.Find(context.Background(), f.session(), f.who, FindIn{Text: "sign"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: "https://example.com/b"}); err != nil {
		t.Fatal(err)
	}
	f.closedAfter()
	checkSent(t, f.fb)
}

// checkSent checks the fake browser was sent only allow-listed methods,
// no enable or DOM.getDocument, and scripts only in isolated worlds.
func checkSent(t *testing.T, fb *fakeBrowser) {
	t.Helper()
	allowed := AllowedMethods()
	for _, m := range fb.Received() {
		if !slices.Contains(allowed, m.Method) || strings.HasSuffix(m.Method, ".enable") || m.Method == "DOM.getDocument" {
			t.Errorf("sent %s", m.Method)
		}
		if m.Method == "Runtime.evaluate" {
			// Code never runs by context number: only the bootstrap does.
			var p struct {
				Expression    string `json:"expression"`
				ReturnByValue bool   `json:"returnByValue"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.Expression != "this" || p.ReturnByValue {
				t.Errorf("evaluated by context number: %s", m.Params)
			}
		}
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.mainRuns > 0 {
		t.Errorf("%d calls ran in a page's own world", fb.mainRuns)
	}
}

// TestAWorldNumberThatNamesAnotherWorldIsNeverUsed: after a cross-process
// navigation the renderer numbers its contexts again, and the number of
// the old isolated world names a frame's main world. A wait polling
// across that navigation must make its world again, not run there.
func TestAWorldNumberThatNamesAnotherWorldIsNeverUsed(t *testing.T) {
	f := newFixture(t)
	f.fb.reuseIDs = true
	tb := f.fb.addTab("https://a.example/", loginPage(), true)
	f.fb.pages = func(string) *fnode {
		return el("#document", "RootWebArea", "", box(0, 0, 1280, 800), el("p", "paragraph", "", box(0, 0, 100, 20), txt("Arrived", box(0, 0, 100, 20))))
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.fb.mu.Lock()
		f.fb.go_(tb, "https://b.example/frames")
		f.fb.mu.Unlock()
	}()
	out, err := f.tools.WaitFor(context.Background(), f.session(), f.who, WaitIn{Text: "arrived", TimeoutMS: 5000})
	if err != nil || out != "ok" {
		t.Fatalf("%v %q", err, out)
	}
	f.closedAfter()
	if out := f.mustSnapshot(nil); !strings.Contains(out, "URL: https://b.example/frames") {
		t.Errorf("snapshot after:\n%s", out)
	}
	checkSent(t, f.fb)
}

func TestSnapshotBudget(t *testing.T) {
	f := newFixture(t)
	var kids []*fnode
	for i := range 60 {
		kids = append(kids, el("button", "button", fmt.Sprintf("Button %d", i), box(10, float64(10+i*30), 100, 20)))
	}
	long := strings.Repeat("words ", 2000)
	kids = append(kids, el("p", "paragraph", "", box(10, 3000, 600, 20), txt(long, box(10, 3000, 600, 400))))
	f.fb.addTab("https://example.com/"+strings.Repeat("q", 400), el("#document", "RootWebArea", "", box(0, 0, 1280, 800), kids...), true)

	zero := 0
	out := f.mustSnapshot(&zero)
	if refRE.MatchString(out) || strings.Contains(out, "Button") {
		t.Errorf("max_chars 0 gave more than the URL and title:\n%s", out)
	}
	if !strings.Contains(out, "URL: https://example.com/qqq") || !strings.Contains(out, "q…\n") {
		t.Errorf("the URL isn't capped at 256 characters:\n%s", out)
	}
	if u := regexp.MustCompile(`URL: (\S+)`).FindStringSubmatch(out); len([]rune(u[1])) != maxHeader {
		t.Errorf("URL is %d characters, want %d", len([]rune(u[1])), maxHeader)
	}

	small := 300
	out = f.mustSnapshot(&small)
	m := regexp.MustCompile(`…\((\d+) controls and ([\d,]+) characters omitted; use find\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no omission marker:\n%s", out)
	}
	shown := len(refRE.FindAllString(out, -1))
	if fmt.Sprint(60-shown) != m[1] {
		t.Errorf("shown %d controls, marker says %s omitted", shown, m[1])
	}
	body := out[strings.Index(out, "Title:"):strings.LastIndex(out, "</untrusted")]
	if n := len([]rune(body)); n > 300+300 {
		t.Errorf("a 300-character budget gave %d characters", n)
	}
	for _, bad := range []int{-1, MaxMaxChars + 1} {
		if _, err := f.snapshot(&bad); !isStatus(err, 400) {
			t.Errorf("max_chars %d: %v", bad, err)
		}
	}
	big := MaxMaxChars
	out = f.mustSnapshot(&big)
	if strings.Contains(out, "controls and") {
		t.Errorf("all 60 controls should fit in %d:\n%s", big, out)
	}
}

func isStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

func TestSnapshotTiersAndCaps(t *testing.T) {
	f := newFixture(t)
	longName := strings.Repeat("N", 200)
	field := el("input", "textbox", longName, box(10, 1200, 100, 20))
	field.value = new(strings.Repeat("v", 200))
	doc := el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		el("a", "link", "In view", box(10, 10, 50, 20)),
		el("main", "main", "", box(0, 900, 1280, 2000), field, el("p", "", "", box(0, 1300, 100, 20), txt("main text", box(0, 1300, 100, 20)))),
		el("a", "link", "Elsewhere", box(10, 5000, 50, 20)),
		el("dialog", "dialog", "Cookies", box(100, 100, 400, 300),
			el("button", "button", "Accept", box(110, 110, 50, 20)), txt("We use cookies", box(110, 140, 100, 20)),
		).with(func(n *fnode) { n.props = map[string]string{"modal": "true"} }),
		el("iframe", "Iframe", "", box(10, 50, 300, 200)).attr("src", "https://other.example/embed?x=1"),
	)
	f.fb.addTab("https://example.com/", doc, true)
	out := f.mustSnapshot(nil)
	order := []string{"-- in the open dialog --", `button "Accept"`, "We use cookies", "-- in view --", `link "In view"`, "iframe https://other.example (not read)", "-- in the main content --", "textbox", "main text", "-- elsewhere --", `link "Elsewhere"`}
	at := 0
	for _, w := range order {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("%q missing or out of order in\n%s", w, out)
		}
		at += i
	}
	if !strings.Contains(out, `"`+strings.Repeat("N", 79)+`…"`) || strings.Contains(out, strings.Repeat("N", 81)) {
		t.Errorf("the name isn't capped at 80:\n%s", out)
	}
	if !strings.Contains(out, `= "`+strings.Repeat("v", 79)+`…"`) {
		t.Errorf("the value isn't capped at 80:\n%s", out)
	}
	if !refRE.MatchString(strings.Split(out, "-- in view --\n")[1]) {
		t.Errorf("the iframe has no ref:\n%s", out)
	}
}

func TestSnapshotHiddenContent(t *testing.T) {
	f := newFixture(t)
	p := func(s string, b *[4]float64) *fnode { return el("p", "paragraph", "", b, txt(s, b)) }
	hiddenBox := box(0, 0, 100, 20)
	doc := el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		p("Visible text", box(0, 0, 100, 20)),
		// display:none: not laid out, and AX-ignored.
		el("p", "paragraph", "", nil, txt("DISPLAY-NONE", nil).with(func(n *fnode) { n.ignored = true })),
		// visibility:hidden by its computed style (even if AX didn't say).
		el("p", "paragraph", "", hiddenBox, txt("VISIBILITY-HIDDEN", hiddenBox).with(func(n *fnode) { n.visib = "hidden" })),
		// AX-ignored (aria-hidden).
		el("p", "paragraph", "", box(0, 30, 100, 20), txt("ARIA-HIDDEN", box(0, 30, 100, 20)).with(func(n *fnode) { n.ignored = true })),
		// Zero-size.
		p("ZERO-SIZE", box(0, 60, 0, 0)),
		el("button", "button", "ZERO-BUTTON", box(0, 60, 0, 0), txt("ZERO-BUTTON-TEXT", box(0, 60, 80, 20))),
		// Kept: opacity:0 and off-screen look the same as visible to the snapshot.
		p("OPACITY-ZERO", box(0, 90, 100, 20)),
		p("OFF-SCREEN", box(-9999, 0, 100, 20)),
		// Names from aria-label, and aria-labelledby to a hidden node.
		el("button", "button", "LABELLEDBY-HIDDEN", box(0, 120, 50, 20)),
		el("button", "button", "ARIA-LABEL", box(0, 150, 50, 20)),
	)
	f.fb.addTab("https://example.com/", doc, true)
	out := f.mustSnapshot(nil)
	for _, gone := range []string{"DISPLAY-NONE", "VISIBILITY-HIDDEN", "ARIA-HIDDEN", "ZERO-SIZE", "ZERO-BUTTON"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s is in the snapshot:\n%s", gone, out)
		}
	}
	for _, kept := range []string{"OPACITY-ZERO", "OFF-SCREEN", `button "LABELLEDBY-HIDDEN"`, `button "ARIA-LABEL"`} {
		if !strings.Contains(out, kept) {
			t.Errorf("%s is missing:\n%s", kept, out)
		}
	}
	if !strings.HasPrefix(out, "<untrusted-content-") {
		t.Error("not fenced")
	}
}

func TestSecretFieldsShowNoValue(t *testing.T) {
	f := newFixture(t)
	field := func(name string, value string, attrs ...string) *fnode {
		n := el("input", "textbox", name, box(0, 0, 100, 20))
		n.value = new(value)
		for i := 0; i+1 < len(attrs); i += 2 {
			n.attr(attrs[i], attrs[i+1])
		}
		return n
	}
	doc := el("#document", "RootWebArea", "", box(0, 0, 1280, 800),
		field("Password", "SECRET-1", "type", "password"),
		field("Code", "SECRET-2", "autocomplete", "one-time-code"),
		field("PIN", "SECRET-3", "name", "pin"),
		field("Card code", "SECRET-4", "aria-label", "CVV"),
		field("Empty password", "", "type", "password"),
		field("Name", "Ana"),
	)
	f.fb.addTab("https://example.com/", doc, true)
	out := f.mustSnapshot(nil)
	if regexp.MustCompile(`SECRET-\d`).MatchString(out) {
		t.Errorf("a secret value leaked:\n%s", out)
	}
	if strings.Count(out, "[filled]") != 4 || !strings.Contains(out, `"Empty password" = [empty]`) || !strings.Contains(out, `"Name" = "Ana"`) {
		t.Errorf("secret fields:\n%s", out)
	}
}

func TestSnapshotTooLarge(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.fb.bigDOM = maxDOMNodes + 1
	_, err := f.snapshot(nil)
	if !isStatus(err, 422) || err.Error() != "page too large to snapshot; use find" {
		t.Fatalf("got %v", err)
	}
	f.closedAfter()
	for _, m := range f.fb.Methods() {
		if m == "Accessibility.getFullAXTree" {
			t.Error("asked for the AX tree of a page past the cap")
		}
	}
	// find still works, on the text alone.
	out, err := f.tools.Find(context.Background(), f.session(), f.who, FindIn{Text: "below the fold"})
	if err != nil || !strings.Contains(out, "Far below the fold") || !strings.Contains(out, "too large") {
		t.Fatalf("find on a large page: %v\n%s", err, out)
	}
}

func TestFind(t *testing.T) {
	f := newFixture(t)
	var kids []*fnode
	for i := range 80 {
		s := fmt.Sprintf("Row %d: %s the Needle here %s", i, strings.Repeat("a", 100), strings.Repeat("b", 100))
		kids = append(kids, el("p", "paragraph", "", box(0, float64(i*20), 100, 20), txt(s, box(0, float64(i*20), 100, 20))))
	}
	kids = append(kids, el("button", "button", "NEEDLE button", box(0, 0, 50, 20)))
	f.fb.addTab("https://example.com/", el("#document", "RootWebArea", "", box(0, 0, 1280, 800), kids...), true)
	find := func(in FindIn) (string, error) {
		out, err := f.tools.Find(context.Background(), f.session(), f.who, in)
		f.closedAfter()
		return out, err
	}
	out, err := find(FindIn{Text: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	lines := refRE.FindAllString(out, -1)
	if len(lines) != DefaultFindLimit || !strings.Contains(out, "10 of 81 matches.") {
		t.Errorf("default limit: %d lines\n%s", len(lines), out)
	}
	// 80 characters each side: " the " and 75 a's before, " here " and 74 b's after.
	if !regexp.MustCompile(`(?m) …a{75} the Needle here b{74}…$`).MatchString(out) {
		t.Errorf("no ±80 context:\n%s", out)
	}
	if out, _ := find(FindIn{Text: "needle button"}); !strings.Contains(out, `button "NEEDLE button"`) || !strings.Contains(out, "1 match.") {
		t.Errorf("controls aren't searched:\n%s", out)
	}
	out, _ = find(FindIn{Text: "NEEDLE", Limit: 100})
	if out != "" {
		t.Errorf("limit 100 accepted:\n%s", out)
	}
	out, err = find(FindIn{Text: "needle", Limit: MaxFindLimit})
	if err != nil || len(refRE.FindAllString(out, -1)) != MaxFindLimit {
		t.Errorf("limit %d: %v", MaxFindLimit, err)
	}
	if _, err := find(FindIn{Text: " "}); !isStatus(err, 400) {
		t.Errorf("empty text: %v", err)
	}
	if out, _ := find(FindIn{Text: "absent"}); out != "No match." {
		t.Errorf("no match: %q", out)
	}
}

func TestNavigate(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	nav := func(in NavigateIn) (string, error) {
		out, err := f.tools.Navigate(context.Background(), f.session(), f.who, in)
		f.closedAfter()
		return out, err
	}
	for _, bad := range []NavigateIn{
		{URL: "javascript:alert(1)"}, {URL: "file:///etc/passwd"}, {URL: "chrome://settings"}, {URL: "data:text/html,hi"},
		{URL: "https://user:pass@example.com/"}, {URL: "https://user@example.com/"}, {URL: "ftp://example.com/"},
		{Action: "sideways"}, {}, {URL: "https://example.com", Action: "back"},
	} {
		if _, err := nav(bad); !isStatus(err, 400) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if n := len(f.fb.Received()); n != 0 {
		t.Errorf("refused navigations reached the browser (%d calls)", n)
	}
	before := f.mustSnapshot(nil)
	f.fb.tabs[0].readyAfter = 3
	out, err := nav(NavigateIn{URL: "https://example.com/next?q=1"})
	if err != nil || !strings.Contains(out, "URL: https://example.com/next?q=1") || !strings.Contains(out, "Title: Page https://example.com/next") {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.fb.tabs[0].readyAfter != 0 {
		t.Error("navigate didn't wait for the page to load")
	}
	// The old document's refs are stale; the isolated world was made again.
	ref := refRE.FindStringSubmatch(before)[1]
	if _, err := f.tools.state(f.sess.ID, 0).refs.lookup(ref); err == nil {
		t.Errorf("%s still names something after navigating", ref)
	}
	out, err = nav(NavigateIn{Action: "back"})
	if err != nil || !strings.Contains(out, "URL: https://example.com/\n") {
		t.Fatalf("back: %v\n%s", err, out)
	}
	if _, err = nav(NavigateIn{Action: "back"}); !isStatus(err, 409) {
		t.Errorf("back from the first page: %v", err)
	}
	if out, err = nav(NavigateIn{Action: "forward"}); err != nil || !strings.Contains(out, "next?q=1") {
		t.Fatalf("forward: %v\n%s", err, out)
	}
	if _, err = nav(NavigateIn{Action: "reload"}); err != nil {
		t.Fatal(err)
	}
	worlds := 0
	for _, m := range f.fb.Received() {
		if m.Method == "Page.createIsolatedWorld" {
			worlds++
		}
	}
	if worlds < 4 {
		t.Errorf("the isolated world wasn't made again after navigations (%d made)", worlds)
	}
	checkSent(t, f.fb)
}

func TestNewTabNotice(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.mustSnapshot(nil)
	// The page opens a popup, which comes to the front.
	pop := f.fb.addTab("https://accounts.example/signin?client=x", loginPage(), true)
	pop.opener = "T1"
	out := f.mustSnapshot(nil)
	// Its origin and path; not its query, which tabs lists.
	if !strings.Contains(out, "a new tab opened: t2 (https://accounts.example/signin)\n") {
		t.Fatalf("no notice:\n%s", out)
	}
	if !strings.Contains(out, "URL: https://accounts.example/signin") {
		t.Errorf("the tools didn't follow the front tab:\n%s", out)
	}
	if i, j := strings.Index(out, "a new tab"), strings.LastIndex(out, "</untrusted"); i > j {
		t.Error("the notice is outside the fence")
	}
	if out := f.mustSnapshot(nil); strings.Contains(out, "a new tab opened") {
		t.Errorf("the notice came twice:\n%s", out)
	}
}

// When other sessions' large pages hold every read slot, a read waits a
// bounded time and answers busy (503); a handoff still ends the wait.
func TestBusyReadSlots(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	for range maxReads {
		f.tools.reads <- struct{}{}
	}
	f.tools.readWait = 200 * time.Millisecond
	start := time.Now()
	if _, err := f.snapshot(nil); !isStatus(err, http.StatusServiceUnavailable) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %s", d)
	}
	f.closedAfter()
	// Tools that don't read the whole page don't wait.
	if _, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: "https://example.com/b"}); err != nil {
		t.Errorf("navigate: %v", err)
	}
	f.tools.readWait = 20 * time.Second
	done := make(chan error, 1)
	go func() { _, err := f.snapshot(nil); done <- err }()
	time.Sleep(100 * time.Millisecond)
	if _, err := f.m.SetNeedsHelp(f.sess.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !isStatus(err, http.StatusConflict) {
			t.Errorf("a handoff during the wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handoff didn't end the wait")
	}
	if _, err := f.m.SetNeedsHelp(f.sess.ID, false); err != nil {
		t.Fatal(err)
	}
	for range maxReads {
		<-f.tools.reads
	}
	f.mustSnapshot(nil)
}

func TestADialogMakesThePageUnresponsive(t *testing.T) {
	f := newFixture(t)
	tb := f.fb.addTab("https://example.com/", loginPage(), true)
	f.mustSnapshot(nil)
	tb.stuck = true
	start := time.Now()
	_, err := f.snapshot(nil)
	if !isStatus(err, http.StatusGatewayTimeout) || err.Error() != "the page is busy or has a dialog open; if it stays that way, request a handoff." {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	f.closedAfter()
	tb.stuck = false
	f.mustSnapshot(nil)
}

func TestVisualModeIsRefused(t *testing.T) {
	f := newFixture(t)
	s, _, err := f.m.Start(context.Background(), "shot", "")
	if err != nil {
		t.Fatal(err)
	}
	calls := []func() error{
		func() error { _, err := f.tools.Snapshot(context.Background(), s, f.who, SnapshotIn{}); return err },
		func() error { _, err := f.tools.Find(context.Background(), s, f.who, FindIn{Text: "x"}); return err },
		func() error {
			_, err := f.tools.Navigate(context.Background(), s, f.who, NavigateIn{URL: "https://example.com"})
			return err
		},
	}
	for i, c := range calls {
		if err := c(); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "Visual mode") {
			t.Errorf("tool %d: %v", i, err)
		}
	}
	if f.fb.Dials() != 0 {
		t.Error("a tool connected to a Visual-mode browser")
	}
}

func TestCallsAreRefusedAndCancelled(t *testing.T) {
	f := newFixture(t)
	tb := f.fb.addTab("https://example.com/", loginPage(), true)
	// A person in the live view.
	if err := f.m.TakeControl(f.sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.snapshot(nil); !isStatus(err, http.StatusConflict) {
		t.Errorf("while a person drives: %v", err)
	}
	f.m.ReleaseControl(f.sess.ID)
	// A hold, and a revocation, cut a stuck call short.
	f.tools.quick, f.tools.slow = 10*time.Second, 10*time.Second
	tb.stuck = true
	for _, tc := range []struct {
		name   string
		cut    func() func()
		status int
	}{
		{"hold", func() func() { r, _ := f.m.HoldSession(f.sess.ID); return r }, http.StatusConflict},
		{"revoke", func() func() { f.m.DisconnectToken("tok"); return func() {} }, http.StatusUnauthorized},
	} {
		done := make(chan error, 1)
		go func() { _, err := f.snapshot(nil); done <- err }()
		time.Sleep(200 * time.Millisecond)
		start := time.Now()
		release := tc.cut()
		select {
		case err := <-done:
			if !isStatus(err, tc.status) || time.Since(start) > 2*time.Second {
				t.Errorf("%s: %v after %s", tc.name, err, time.Since(start))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s didn't cut the call short", tc.name)
		}
		release()
		f.closedAfter()
	}
}

func TestRefs(t *testing.T) {
	r := newRefs()
	if !regexp.MustCompile(`^[a-z][a-z0-9]$`).MatchString(r.epoch) {
		t.Fatalf("epoch %q", r.epoch)
	}
	k := refKey{target: "T1", frame: "F", loader: "L1", backend: 5}
	a := r.ref(k)
	if a != r.epoch+"e1" || r.ref(k) != a {
		t.Fatalf("ref %s", a)
	}
	if got, err := r.lookup(a); err != nil || got != k {
		t.Fatalf("lookup: %v %v", got, err)
	}
	// Another map's ref (another session, or before a restart) is stale.
	other := newRefs()
	for other.epoch == r.epoch {
		other = newRefs()
	}
	if _, err := r.lookup(other.ref(k)); !isStatus(err, 409) || !strings.Contains(err.Error(), "stale ref") {
		t.Errorf("another map's ref: %v", err)
	}
	if _, err := r.lookup("button"); !isStatus(err, 400) {
		t.Errorf("not a ref: %v", err)
	}
	// Dropped refs are never given again.
	r.dropStale("T1", "L2")
	if _, err := r.lookup(a); err == nil {
		t.Error("a ref to an old document still resolves")
	}
	if b := r.ref(k); b == a {
		t.Errorf("ref %s reused", a)
	}
	// Capped, oldest document first.
	r = newRefs()
	first := r.ref(refKey{target: "T1", loader: "old", backend: 1})
	for i := range maxRefs {
		r.ref(refKey{target: "T1", loader: "new", backend: int64(i + 1)})
	}
	if len(r.byRef) > maxRefs {
		t.Errorf("%d live refs", len(r.byRef))
	}
	if _, err := r.lookup(first); err == nil {
		t.Error("the oldest document's ref survived the cap")
	}
	for i := range 10 {
		r.ref(refKey{target: "T1", loader: "new", backend: int64(maxRefs + i + 1)})
	}
	if len(r.byRef) != maxRefs {
		t.Errorf("%d live refs after more on one document", len(r.byRef))
	}
	// The document being read is never the one dropped, even if it was
	// first seen long ago: using it makes it the most recent.
	r = newRefs()
	r.ref(refKey{target: "T1", loader: "read", backend: 1})
	for i := range maxRefs - 1 {
		r.ref(refKey{target: "T2", loader: "other", backend: int64(i + 1)})
	}
	fresh := r.ref(refKey{target: "T1", loader: "read", backend: 2})
	if _, err := r.lookup(fresh); err != nil {
		t.Errorf("a ref just handed out was dropped: %v", err)
	}
	if _, err := r.lookup(r.byKey[refKey{target: "T1", loader: "read", backend: 1}]); len(r.byRef) > maxRefs || err != nil {
		t.Errorf("%d live refs; the read document's older ref: %v", len(r.byRef), err)
	}
}

func TestRefsGoStaleWhenAPersonTakesTheBrowser(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	out := f.mustSnapshot(nil)
	ref := refRE.FindStringSubmatch(out)[1]
	if _, err := f.m.SetNeedsHelp(f.sess.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.snapshot(nil); !isStatus(err, http.StatusConflict) {
		t.Errorf("during a handoff: %v", err)
	}
	if _, err := f.m.SetNeedsHelp(f.sess.ID, false); err != nil {
		t.Fatal(err)
	}
	f.mustSnapshot(nil)
	ls, _ := f.m.PageLease(context.Background(), f.sess.ID, "tok")
	st := f.tools.state(f.sess.ID, ls.Generation)
	ls.Release()
	if _, err := st.refs.lookup(ref); err == nil {
		t.Errorf("%s survived a handoff", ref)
	}
}

var _ = sessions.ErrNotRunning
var _ = cdptest.NoReply
