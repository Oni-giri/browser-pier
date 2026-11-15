//go:build docker

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// steps makes an act body.
func steps(s ...map[string]any) map[string]any { return map[string]any{"steps": s} }

// TestPageToolsAct is PROJ-21's acting scenarios on a real browser.
func TestPageToolsAct(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "a")
	open := func(path string) string {
		t.Helper()
		a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + path})
		return a.mustTool(s.ID, "snapshot", nil)
	}
	waitEvent := func(e string) bool {
		deadline := time.Now().Add(5 * time.Second)
		for !site.HasEvent(e) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		return site.HasEvent(e)
	}

	// 3 and 7. Log in with one act, straight after navigate (the focus is
	// in the address bar): the site gets the credentials, nothing shows
	// the password.
	snap := open("/login")
	out := a.mustTool(s.ID, "act", steps(
		map[string]any{"action": "fill", "ref": refFor(t, snap, `"Email"`), "text": "ana@example.com"},
		map[string]any{"action": "fill", "ref": refFor(t, snap, `"Password"`), "text": "PASSWORD-hunter2"},
		map[string]any{"action": "check", "ref": refFor(t, snap, `"Remember me"`)},
		map[string]any{"action": "click", "ref": refFor(t, snap, `button "Sign in"`)},
	))
	for _, want := range []string{`textbox "Email" (typed 15 characters)`, `textbox "Password" (typed 16 characters)`, `checkbox "Remember me"`, `clicked`} {
		if !strings.Contains(out, want) {
			t.Errorf("login: missing %q in\n%s", want, out)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(site.Logins()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := site.Logins(); len(got) != 1 || got[0] != "ana@example.com:PASSWORD-hunter2" {
		t.Fatalf("the site got %v", got)
	}
	if strings.Contains(out, "PASSWORD") {
		t.Errorf("the password is in the result:\n%s", out)
	}

	// Fill replaces what is there, and type adds key by key.
	snap = open("/login")
	email := refFor(t, snap, `"Email"`)
	a.mustTool(s.ID, "act", steps(
		map[string]any{"action": "type", "ref": email, "text": "first"},
		map[string]any{"action": "fill", "ref": email, "text": "Second@Example.com"},
		map[string]any{"action": "type", "ref": email, "text": "!"},
	))
	if snap := a.mustTool(s.ID, "snapshot", nil); !strings.Contains(snap, `"Email" = "Second@Example.com!"`) {
		t.Errorf("fill and type:\n%s", snap)
	}

	// 4. A covered button fails, naming the overlay, and isn't clicked; a
	// styled checkbox (its input under its label) works.
	snap = open("/overlay")
	code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Buy now"`)}))
	if code != http.StatusConflict || !strings.Contains(msg, "is covered by <untrusted-content-") || !strings.Contains(msg, `region "Promotion overlay"`) {
		t.Errorf("covered button: %d %s", code, msg)
	}
	time.Sleep(500 * time.Millisecond)
	if site.HasEvent("buy clicked") || site.HasEvent("overlay clicked") {
		t.Errorf("something was clicked: %v", site.Events())
	}
	snap = open("/checkbox")
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "check", "ref": refFor(t, snap, `checkbox "I accept the terms"`)}))
	if !strings.Contains(out, "checked") || !waitEvent("terms true") {
		t.Errorf("styled checkbox: %s %v", out, site.Events())
	}
	// A display:none checkbox has no ref: the agent finds its label and clicks it.
	if strings.Contains(snap, `checkbox "Send me news"`) || !strings.Contains(snap, "Send me news") {
		t.Errorf("hidden checkbox:\n%s", snap)
	}
	label := refFor(t, a.mustTool(s.ID, "find", map[string]any{"text": "send me news"}), "Send me news")
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": label}))
	if !waitEvent("news true") {
		t.Errorf("clicking the label didn't check the hidden checkbox: %v", site.Events())
	}

	// Buttons in a closed shadow root: the free one is clicked, the
	// covered one isn't.
	snap = open("/shadow")
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Free in shadow"`)}))
	if !waitEvent("shadow free clicked") {
		t.Errorf("the free button in a closed shadow root wasn't clicked: %v", site.Events())
	}
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Covered in shadow"`)})); code != http.StatusConflict || !strings.Contains(msg, "Shadow cover") {
		t.Errorf("the covered button in a closed shadow root: %d %s", code, msg)
	}

	// 5. An SPA re-renders: a removed node's ref is stale; a reused node's
	// step echoes its new name.
	snap = open("/spa")
	one, two := refFor(t, snap, `button "Message one"`), refFor(t, snap, `button "Message two"`)
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Next page"`)}))
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": two})); code != http.StatusConflict || !strings.Contains(msg, "stale ref "+two) {
		t.Errorf("removed node: %d %s", code, msg)
	}
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": one}))
	if !strings.Contains(out, `button "Message three"`) || !waitEvent("opened Message three") {
		t.Errorf("reused node: %s %v", out, site.Events())
	}

	// Focus taken away while typing: stopped, and said so.
	snap = open("/thief")
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "type", "ref": refFor(t, snap, `textbox "First"`), "text": "abc"})); code != http.StatusConflict || !strings.Contains(msg, "the focus left") || !strings.Contains(msg, `textbox "Second"`) {
		t.Errorf("focus thief: %d %s", code, msg)
	}

	// Select, hover, press and unsupported inputs.
	snap = open("/select")
	out = a.mustTool(s.ID, "act", steps(
		map[string]any{"action": "select", "ref": refFor(t, snap, `combobox "Country"`), "option": "Latvia"},
		map[string]any{"action": "hover", "ref": refFor(t, snap, `button "Hover me"`)},
	))
	if !strings.Contains(out, `selected "Latvia"`) || !waitEvent("country lv") || !waitEvent("hovered") {
		t.Errorf("select and hover: %s %v", out, site.Events())
	}
	// A file input opens a chooser nothing could close: refused.
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, "Upload")})); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "file chooser") {
		t.Errorf("clicking a file input: %d %s", code, msg)
	}

	// 8. find and wait_for: text that appears after 2 s, an element hidden after 2 s.
	open("/later")
	spinner := refFor(t, a.mustTool(s.ID, "find", map[string]any{"text": "loading spinner"}), "Loading spinner")
	start := time.Now()
	if out := a.mustTool(s.ID, "wait", map[string]any{"text": "Payment confirmed"}); out != "ok" || time.Since(start) < time.Second {
		t.Errorf("wait for text: %q after %s", out, time.Since(start))
	}
	if out := a.mustTool(s.ID, "wait", map[string]any{"ref": spinner, "state": "hidden", "timeout_ms": 5000}); out != "ok" {
		t.Errorf("wait for hidden: %q", out)
	}
	if out := a.mustTool(s.ID, "find", map[string]any{"text": "payment confirmed"}); !strings.Contains(out, "1 match.") {
		t.Errorf("find after the wait: %s", out)
	}
	if out := a.mustTool(s.ID, "wait", map[string]any{"text": "never there", "timeout_ms": 500}); out != "timed out after 500ms" {
		t.Errorf("timeout: %q", out)
	}

	// 9. A handoff opened during a 20 s wait ends it at once; after
	// hand-back, the old refs are stale.
	snap = open("/login")
	old := refFor(t, snap, `button "Sign in"`)
	done := make(chan [2]any, 1)
	go func() {
		code, msg := a.tool(s.ID, "wait", map[string]any{"text": "never there", "timeout_ms": 20000})
		done <- [2]any{code, msg}
	}()
	time.Sleep(time.Second)
	start = time.Now()
	if _, err := a.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r[0] != http.StatusConflict || !strings.Contains(r[1].(string), "handoff") || time.Since(start) > 2*time.Second {
			t.Errorf("wait during a handoff: %v after %s", r, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait went on through the handoff")
	}
	if _, err := a.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": old})); code != http.StatusConflict || !strings.Contains(msg, "stale ref") {
		t.Errorf("old ref after hand-back: %d %s", code, msg)
	}

	// 12. An alert: the page stops answering, and the tools say a dialog
	// may be open. A person dismisses it during a handoff; the tools work
	// again.
	snap = open("/alert")
	code, msg = a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Show an alert"`)}))
	const stuck = "the page is busy or has a dialog open; if it stays that way, request a handoff."
	if code != http.StatusGatewayTimeout || !strings.Contains(msg, stuck) {
		t.Errorf("click that opens an alert: %d %s", code, msg)
	}
	if code, msg := a.tool(s.ID, "snapshot", nil); code != http.StatusGatewayTimeout || msg != stuck {
		t.Errorf("snapshot with the alert open: %d %s", code, msg)
	}
	if _, err := a.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	// The person presses Enter on the dialog, through the screen.
	resp, err := browsertest.ControlClient(a.m.Layout().ControlSocket(s.Profile)).Post("http://control/input/key", "application/json", strings.NewReader(`{"keys": "Return"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, err := a.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	if snap := a.mustTool(s.ID, "snapshot", nil); !strings.Contains(snap, "Show an alert") {
		t.Errorf("after the dialog closed:\n%s", snap)
	}

	for _, act := range a.m.Activity(s.ID) {
		if strings.Contains(act.What, "PASSWORD") || strings.Contains(act.What, "ana@") || strings.Contains(act.What, "Message") {
			t.Errorf("the activity log has typed or page text: %q", act.What)
		}
	}
	for _, tg := range targets(t, a.m.Layout().ControlSocket(s.Profile)) {
		if tg.Attached {
			t.Errorf("%s is still attached", tg.URL)
		}
	}
}

// TestActInputLooksHuman is PROJ-113's check for act in API mode: what a
// page's own script records of act's clicks and typing looks like a hand.
// (CDP's input events are trusted too, but that isn't the point here.)
func TestActInputLooksHuman(t *testing.T) {
	a := newRealAPI(t)
	det := browsertest.NewDetector(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "m")
	a.mustTool(s.ID, "navigate", map[string]string{"url": det.InputURL()})
	snap := a.mustTool(s.ID, "snapshot", nil)
	btnA, btnB, text := refFor(t, snap, `button "Button A"`), refFor(t, snap, `button "Button B"`), refFor(t, snap, `textbox "Text"`)
	ups := func(n int) func([]browsertest.InputEvent) bool {
		return func(ev []browsertest.InputEvent) bool { return len(ofType(ev, "mouseup")) >= n }
	}
	settle := func() []browsertest.InputEvent {
		time.Sleep(300 * time.Millisecond)
		ev := det.Events()
		det.ResetEvents()
		return ev
	}

	// The buttons are 400 px apart: a glide from one to the other.
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": btnA}))
	det.WaitEvents(t, "the first click", ups(1))
	settle()
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": btnB}))
	det.WaitEvents(t, "the 400 px click", ups(1))
	looksLikeAHandsClick(t, settle())

	// Ten clicks on one button land on more than one pixel, all on it.
	var ten []map[string]any
	for range 10 {
		ten = append(ten, map[string]any{"action": "click", "ref": btnB})
	}
	a.mustTool(s.ID, "act", steps(ten...))
	ev := det.WaitEvents(t, "ten clicks", ups(10))
	seen := map[[2]float64]bool{}
	for _, d := range ofType(ev, "mousedown") {
		seen[[2]float64{d.X, d.Y}] = true
		if d.Target != "b" {
			t.Errorf("a click on button B landed on %q at (%v, %v)", d.Target, d.X, d.Y)
		}
	}
	if len(seen) < 2 {
		t.Errorf("ten clicks on a button all landed on %v", seen)
	}
	holdsLikeAHand(t, settle())

	// Typing 30 characters: a typist's uneven rhythm.
	const typed = "the quick brown fox jumps over"
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "type", "ref": text, "text": typed}))
	ev = det.WaitEvents(t, "30 keystrokes", func(ev []browsertest.InputEvent) bool { return len(ofType(ev, "keyup")) >= len(typed) })
	typesLikeATypist(t, ev, typed)

	for _, tg := range targets(t, a.m.Layout().ControlSocket(s.Profile)) {
		if tg.Attached {
			t.Errorf("%s is still attached", tg.URL)
		}
	}
}

// TestPageToolsUpload is PROJ-117 on a real browser: files put in the
// session with the REST upload, set on the page's file inputs with act's
// upload step (directly, through a styled label, several at once), then
// posted by clicking the form's button: the site gets the same bytes.
func TestPageToolsUpload(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "u")
	waitEvent := func(e string) bool {
		deadline := time.Now().Add(5 * time.Second)
		for !site.HasEvent(e) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		return site.HasEvent(e)
	}
	upload := func(name string, content []byte) string {
		t.Helper()
		var up struct{ Name, Path string }
		if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/files?name="+url.QueryEscape(name), string(content), &up); code != http.StatusCreated || up.Path != "/files/uploads/"+up.Name {
			t.Fatalf("upload %s: %d %+v", name, code, up)
		}
		return up.Name
	}
	report := make([]byte, 300<<10)
	if _, err := rand.Read(report); err != nil {
		t.Fatal(err)
	}
	photo := []byte("\x89PNG\r\n\x1a\n not really a photo")
	hash := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	reportName, photoName := upload("report.bin", report), upload("photo.png", photo)

	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/upload"})
	snap := a.mustTool(s.ID, "snapshot", nil)
	t.Logf("snapshot of /upload:\n%s", snap)
	doc, more, send := refFor(t, snap, `"Document"`), refFor(t, snap, `"Attachments"`), refFor(t, snap, `button "Send"`)
	out := a.mustTool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": doc, "files": []string{reportName}}))
	if !strings.Contains(out, "uploaded 1 file to "+doc) || !waitEvent("doc 1") {
		t.Errorf("upload to the input: %s %v", out, site.Events())
	}
	// The photo input is transparent over its label: the snapshot lists
	// it, named by the label, and its own ref sets it.
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": refFor(t, snap, `button "Attach a photo"`), "files": []string{photoName}}))
	if !strings.Contains(out, "uploaded 1 file to") || !waitEvent("photo 1") {
		t.Errorf("upload to the transparent input: %s %v", out, site.Events())
	}
	// The avatar's is transparent and aria-hidden, the CV's display:none:
	// the snapshot doesn't list them, and their labels' refs (find gives
	// them, for the labels' text) set them.
	for _, c := range []struct{ text, event string }{{"attach an avatar", "avatar 1"}, {"attach a cv", "cv 1"}} {
		found := a.mustTool(s.ID, "find", map[string]any{"text": c.text})
		label := refFor(t, found, "Attach a")
		if strings.Contains(snap, label+" ") {
			t.Errorf("%s: the snapshot lists %s:\n%s", c.text, label, snap)
		}
		out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": label, "files": []string{photoName}}))
		if !strings.Contains(out, "uploaded 1 file to "+label) || !waitEvent(c.event) {
			t.Errorf("upload through the label (%s): %s %v", c.text, out, site.Events())
		}
	}
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": more, "files": []string{reportName, photoName}}))
	if !strings.Contains(out, "uploaded 2 files to "+more) || !waitEvent("more 2") {
		t.Errorf("upload two: %s %v", out, site.Events())
	}
	// Refused: two files to a single input, a page's own chooser button,
	// a file that isn't there.
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": doc, "files": []string{reportName, photoName}})); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "takes one file") {
		t.Errorf("two files to a single input: %d %s", code, msg)
	}
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": refFor(t, snap, `button "Choose a file"`), "files": []string{reportName}})); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "file_upload handoff") {
		t.Errorf("a chooser button: %d %s", code, msg)
	}
	if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "upload", "ref": doc, "files": []string{"nothing.pdf"}})); code != http.StatusUnprocessableEntity || !strings.Contains(msg, `no uploaded file named "nothing.pdf"; upload it first with upload_file`) {
		t.Errorf("a missing file: %d %s", code, msg)
	}

	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": send}))
	deadline := time.Now().Add(10 * time.Second)
	for len(site.Uploads()) < 6 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	got := map[string][]browsertest.Upload{}
	for _, u := range site.Uploads() {
		got[u.Field] = append(got[u.Field], u)
	}
	want := map[string][]browsertest.Upload{
		"doc":    {{Field: "doc", Name: "report.bin", Size: int64(len(report)), SHA256: hash(report)}},
		"photo":  {{Field: "photo", Name: "photo.png", Size: int64(len(photo)), SHA256: hash(photo)}},
		"avatar": {{Field: "avatar", Name: "photo.png", Size: int64(len(photo)), SHA256: hash(photo)}},
		"cv":     {{Field: "cv", Name: "photo.png", Size: int64(len(photo)), SHA256: hash(photo)}},
		"more": {{Field: "more", Name: "report.bin", Size: int64(len(report)), SHA256: hash(report)},
			{Field: "more", Name: "photo.png", Size: int64(len(photo)), SHA256: hash(photo)}},
	}
	for field, w := range want {
		if fmt.Sprint(got[field]) != fmt.Sprint(w) {
			t.Errorf("the site got %v for %s, want %v", got[field], field, w)
		}
	}
	if snap := a.mustTool(s.ID, "snapshot", nil); !strings.Contains(snap, "sha256 "+hash(report)) {
		t.Errorf("the echo page:\n%s", snap)
	}
}

// TestActCoveredClicks is PROJ-115 on a real browser: a sticky header, an
// overlay fading out and a wrapped link no longer make act refuse or miss;
// a dialog that stays is named in the refusal.
func TestActCoveredClicks(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "c")
	open := func(path string) string {
		t.Helper()
		a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + path})
		return a.mustTool(s.ID, "snapshot", nil)
	}
	count := func(e string) int {
		n := 0
		for _, got := range site.Events() {
			if got == e {
				n++
			}
		}
		return n
	}
	waitEvent := func(e string) bool {
		deadline := time.Now().Add(5 * time.Second)
		for !site.HasEvent(e) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		return site.HasEvent(e)
	}

	// A. Scrolling just into view leaves the button under a sticky
	// header: act scrolls it to the middle and clicks it, once.
	snap := open("/sticky")
	out := a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Under the header"`)}))
	if !strings.Contains(out, `clicked`) || !waitEvent("sticky clicked") {
		t.Errorf("sticky header: %s %v", out, site.Events())
	}
	time.Sleep(300 * time.Millisecond)
	if n := count("sticky clicked"); n != 1 || site.HasEvent("header clicked") {
		t.Errorf("sticky header: the button's handler ran %d times; events %v", n, site.Events())
	}

	// B. A backdrop fading out after load: a click right after waits it
	// out (instant pace, to time the wait alone)...
	snap = open("/fading")
	start := time.Now()
	code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Behind the backdrop"`), "pace": "instant"}))
	d := time.Since(start)
	t.Logf("fading backdrop: clicked after %s", d)
	if code != http.StatusOK || d > 1500*time.Millisecond {
		t.Errorf("fading backdrop: %d after %s: %s", code, d, msg)
	}
	if !waitEvent("fading clicked") || site.HasEvent("backdrop clicked") {
		t.Errorf("fading backdrop: %v", site.Events())
	}
	// ...and with cover_wait_ms 0 it is refused, naming the backdrop.
	snap = open("/fading")
	code, msg = a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Behind the backdrop"`), "pace": "instant", "cover_wait_ms": 0}))
	if code != http.StatusConflict || !strings.Contains(msg, "is covered by <untrusted-content-") || !strings.Contains(msg, `region "Fading backdrop"`) {
		t.Errorf("fading backdrop, cover_wait_ms 0: %d %s", code, msg)
	}
	time.Sleep(time.Second)
	if n := count("fading clicked"); n != 1 || site.HasEvent("backdrop clicked") {
		t.Errorf("fading backdrop, cover_wait_ms 0: clicked %d times in all; events %v", n, site.Events())
	}

	// C. A cookie dialog that stays: refused after the wait, naming the
	// dialog by its title, fenced.
	snap = open("/cookie-dialog")
	start = time.Now()
	code, msg = a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Behind the cookie dialog"`)}))
	d = time.Since(start)
	t.Logf("cookie dialog: refused after %s", d)
	if code != http.StatusConflict || d > 1300*time.Millisecond {
		t.Errorf("cookie dialog: %d after %s: %s", code, d, msg)
	}
	if !fencedIn(msg, `dialog "Cookie settings" is open`) {
		t.Errorf("cookie dialog: the title isn't named, fenced: %s", msg)
	}
	time.Sleep(300 * time.Millisecond)
	if site.HasEvent("behind cookie clicked") || site.HasEvent("cookie backdrop clicked") {
		t.Errorf("cookie dialog: something was clicked: %v", site.Events())
	}

	// D. A wrapped link whose first line is a 3 px sliver: the click
	// lands in its long second line.
	snap = open("/wrapped")
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `link "Read the whole article about wrapped links here"`)}))
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for got == "" && time.Now().Before(deadline) {
		for _, e := range site.Events() {
			if strings.HasPrefix(e, "wrapped click ") {
				got = e
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	target, raw, _ := strings.Cut(strings.TrimPrefix(got, "wrapped click "), " ")
	var click []json.RawMessage
	var x, y float64
	var rects [][4]float64
	if err := json.Unmarshal([]byte(raw), &click); err != nil || len(click) != 3 ||
		json.Unmarshal(click[0], &x) != nil || json.Unmarshal(click[1], &y) != nil || json.Unmarshal(click[2], &rects) != nil {
		t.Fatalf("wrapped link: no click recorded (%q): %v", got, site.Events())
	}
	if len(rects) != 2 || rects[0][2] > 10 {
		t.Fatalf("wrapped link: the page didn't wrap as meant: line boxes %v", rects)
	}
	r := rects[1]
	if target != "link" || x < r[0] || x > r[0]+r[2] || y < r[1] || y > r[1]+r[3] {
		t.Errorf("wrapped link: the click hit %s at (%v, %v), not inside the second line %v", target, x, y, r)
	}
}

// fencedIn reports whether text is in msg inside one fence: after a
// fence's opening tag, before that fence's closing line.
func fencedIn(msg, text string) bool {
	i := strings.Index(msg, text)
	if i < 0 {
		return false
	}
	open := strings.LastIndex(msg[:i], "<untrusted-content-")
	if open < 0 {
		return false
	}
	id, _, ok := strings.Cut(msg[open+len("<untrusted-content-"):], " ")
	return ok && strings.Contains(msg[i:], "\n</untrusted-content-"+id+">")
}
