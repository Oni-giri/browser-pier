//go:build docker

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// activeTab is the tab tabs list marks active, like "t1".
func activeTab(t *testing.T, list string) string {
	t.Helper()
	for _, l := range strings.Split(list, "\n") {
		if strings.HasSuffix(l, "(active)") {
			return strings.Fields(l)[0]
		}
	}
	t.Fatalf("no active tab in\n%s", list)
	return ""
}

// TestPageToolsTabs is scenario 6: a target=_blank link and a window.open
// sign-in popup each give a notice; the agent switches, acts and closes,
// and the opener is active again.
func TestPageToolsTabs(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "t")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/popups"})
	snap := a.mustTool(s.ID, "snapshot", nil)
	out := a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `link "Open in a new tab"`)}))
	if !strings.Contains(out, "a new tab opened: t2 (") {
		t.Errorf("no notice for the target=_blank tab:\n%s", out)
	}
	// GET lists the tabs too.
	req, _ := http.NewRequest(http.MethodGet, a.base+"/api/sessions/"+s.ID+"/tabs", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Text string }
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = json.Unmarshal(b, &got)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got.Text, "t1 ") || !strings.Contains(got.Text, "t2 ") {
		t.Fatalf("GET tabs: %d %s", resp.StatusCode, b)
	}
	if activeTab(t, got.Text) != "t1" {
		a.mustTool(s.ID, "tabs", map[string]string{"action": "switch", "id": "t1"})
	}
	snap = a.mustTool(s.ID, "snapshot", nil)
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Sign in with a popup"`)}))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out, "a new tab opened: t3 (") && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		out += a.mustTool(s.ID, "tabs", nil)
	}
	if !strings.Contains(out, "a new tab opened: t3 (") {
		t.Fatalf("no notice for the popup:\n%s", out)
	}
	// Switch to the popup: it is in front, where a person watching sees it.
	out = a.mustTool(s.ID, "tabs", map[string]string{"action": "switch", "id": "t3"})
	if !strings.Contains(out, "the active tab is now t3") || !strings.Contains(out, "/popup-login") {
		t.Fatalf("switch: %s", out)
	}
	if out := a.mustTool(s.ID, "find", map[string]any{"text": "Continue"}); !strings.Contains(out, `button "Continue"`) {
		t.Fatalf("the tools don't read the popup: %s", out)
	}
	snap = a.mustTool(s.ID, "snapshot", nil)
	a.mustTool(s.ID, "act", steps(
		map[string]any{"action": "fill", "ref": refFor(t, snap, `textbox "Email"`), "text": "ana@example.com"},
		map[string]any{"action": "click", "ref": refFor(t, snap, `button "Continue"`)},
	))
	deadline = time.Now().Add(5 * time.Second)
	for !site.HasEvent("popup signed in") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !site.HasEvent("popup signed in") {
		t.Fatalf("the popup wasn't used: %v", site.Events())
	}
	// The popup closes itself: the opener is active again.
	deadline = time.Now().Add(5 * time.Second)
	var list string
	for time.Now().Before(deadline) {
		list = a.mustTool(s.ID, "tabs", nil)
		if !strings.Contains(list, "t3 ") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if strings.Contains(list, "t3 ") || activeTab(t, list) != "t1" {
		t.Errorf("after the popup closed:\n%s", list)
	}
	// Switch to the other tab and close it: the opener is active again.
	a.mustTool(s.ID, "tabs", map[string]string{"action": "switch", "id": "t2"})
	out = a.mustTool(s.ID, "tabs", map[string]string{"action": "close"})
	if !strings.Contains(out, "closed t2; the active tab is now t1") || !strings.Contains(out, "/popups") {
		t.Errorf("close: %s", out)
	}
	// new: a URL, or about:blank; other schemes refused.
	out = a.mustTool(s.ID, "tabs", map[string]string{"action": "new", "url": site.URL + "/login"})
	if !strings.Contains(out, "opened t4") || !strings.Contains(out, "Title: Sign in") {
		t.Errorf("new: %s", out)
	}
	if code, msg := a.tool(s.ID, "tabs", map[string]string{"action": "new", "url": "file:///etc/passwd"}); code != http.StatusBadRequest {
		t.Errorf("new file URL: %d %s", code, msg)
	}
	a.mustTool(s.ID, "tabs", map[string]string{"action": "close"})
	if code, msg := a.tool(s.ID, "tabs", map[string]string{"action": "close"}); code != http.StatusConflict {
		t.Errorf("closing the last tab: %d %s", code, msg)
	}
}

// TestPageToolsStayQuiet is scenario 14: 20 runs of each page tool, with
// nothing else attached, against the continuous detector, which must
// catch its positive control. Hits are listed in
// images/chromium/ANTI-BOT.md with a decision.
func TestPageToolsStayQuiet(t *testing.T) {
	a := newRealAPI(t)
	w := browsertest.NewWatch(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "q")
	w.Attached = attachedPages(t, a.m.Layout().ControlSocket(s.Profile))
	a.mustTool(s.ID, "navigate", map[string]string{"url": w.URL})
	w.Ready(t)
	time.Sleep(2 * time.Second)
	if !w.Calm(t, "with only the tools' navigate") {
		t.FailNow()
	}
	snap := a.mustTool(s.ID, "snapshot", nil)
	email, pw, goBtn := refFor(t, snap, `textbox "Email"`), refFor(t, snap, `textbox "Password"`), refFor(t, snap, `button "Continue"`)
	doc := refFor(t, snap, `"Document"`)
	var up struct{ Name string }
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/files?name=quiet.txt", "a file to upload", &up); code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	runs := []struct {
		tool string
		body any
		// allowed are hits the batch may cause, with a decision in
		// images/chromium/ANTI-BOT.md.
		allowed []string
	}{
		{"snapshot", nil, nil},
		{"find", map[string]any{"text": "account"}, nil},
		{"act", steps(map[string]any{"action": "click", "ref": goBtn}), nil},
		{"act", steps(map[string]any{"action": "fill", "ref": email, "text": "ana@example.com"}), nil},
		{"act", steps(map[string]any{"action": "type", "ref": pw, "text": "pw"}), nil},
		{"act", steps(map[string]any{"action": "press", "keys": "Tab"}), nil},
		{"act", steps(map[string]any{"action": "hover", "ref": goBtn}), nil},
		// What changed, explicitly (it is on by default after the
		// snapshot above, PROJ-114), and the snapshot diff.
		{"act", map[string]any{"steps": []any{map[string]any{"action": "hover", "ref": goBtn}}, "changes": true}, nil},
		{"snapshot", map[string]any{"diff": true}, nil},
		// Files set without a click to open the chooser (PROJ-117).
		{"act", steps(map[string]any{"action": "upload", "ref": doc, "files": []string{up.Name}}), []string{"file-without-click@main"}},
		{"wait", map[string]any{"text": "Account"}, nil},
		{"wait", map[string]any{"ref": goBtn}, nil},
		// The page polls every 200 ms, so load times out: what counts
		// is that reading its Resource Timing entries isn't seen
		// (PROJ-122).
		{"wait", map[string]any{"load": true, "timeout_ms": 300}, nil},
		{"tabs", map[string]any{}, nil},
	}
	// Each batch goes through w.Quiet, which tells a busy machine's
	// console-slow from one the calls cause with a control batch.
	quiet := true
	for _, r := range runs {
		raw, _ := json.Marshal(r.body)
		if _, ok := w.Quiet(t, fmt.Sprintf("20 × %s %s", r.tool, raw), 20, func() { a.mustTool(s.ID, r.tool, r.body) }, r.allowed...); !ok {
			quiet = false
		}
	}
	// navigate reloads the detector itself, so it goes last.
	if _, ok := w.Quiet(t, "20 × navigate reload", 20, func() { a.mustTool(s.ID, "navigate", map[string]string{"action": "reload"}) }); !ok {
		quiet = false
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
	if !quiet {
		t.Log("each hit above must be listed in images/chromium/ANTI-BOT.md with a decision")
	}
}

// TestRestoredTabsAfterAStop is PROJ-112's check on the page tools: with
// "continue where you left off", a browser starts again with the tabs it
// had. tabs lists them all, and snapshot reads the one in front.
func TestRestoredTabsAfterAStop(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "r")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/nav"})
	a.mustTool(s.ID, "tabs", map[string]string{"action": "new", "url": site.URL + "/login"})
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}

	var again sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, s.Profile), &again); code != http.StatusCreated {
		t.Fatalf("start again: %d %+v", code, again)
	}
	// Chromium reopens the tabs as it starts; give it a moment.
	var list string
	deadline := time.Now().Add(15 * time.Second)
	for {
		list = a.mustTool(again.ID, "tabs", nil)
		if strings.Contains(list, "/nav") && strings.Contains(list, "/login") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a stop and start the tabs aren't both back:\n%s", list)
		}
		time.Sleep(250 * time.Millisecond)
	}
	var front string
	for _, l := range strings.Split(list, "\n") {
		if strings.HasSuffix(l, "(active)") {
			front = l
		}
	}
	if !strings.Contains(front, "/login") {
		t.Errorf("the tab in front before the stop (/login) isn't the active one after it:\n%s", list)
	}
	snap := a.mustTool(again.ID, "snapshot", nil)
	if !strings.Contains(snap, site.URL+"/login") || !strings.Contains(snap, "Sign in") {
		t.Errorf("snapshot doesn't read the tab in front (/login):\n%s", snap)
	}
}
