//go:build docker

package api

import (
	"net/http"
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
	site := browsertest.NewPageSite(t, siteHost(t))
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
	const stuck = "the page isn't responding; a dialog may be open. Request a handoff."
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
