package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// The states of an element on screen: visible, showing, enabled.
const onScreen = 1<<30 | 1<<25 | 1<<8 | 1<<24

// loginTree is a reader's answer for a login form; the button is at
// (615, 167), 58x22.
func loginTree(buttonStates uint64) string {
	return fmt.Sprintf(`{"doc": {"app": ":1.0", "id": "/d/1", "url": "https://example.com/login", "title": "Sign in", "x": 0, "y": 87, "w": 1440, "h": 781},
"screen_width": 1440, "screen_height": 900, "nodes": [
 {"id": "/n/1", "parent": -1, "depth": 1, "role": 110, "states": %[1]d, "x": 8, "y": 108, "w": 1424, "h": 81, "attrs": {"xml-roles": "main"}},
 {"id": "/n/2", "parent": 0, "depth": 2, "role": 79, "states": %[1]d, "name": "Email", "text": "ana@example.com", "attrs_read": true, "x": 50, "y": 167, "w": 186, "h": 22},
 {"id": "/n/3", "parent": 0, "depth": 2, "role": 40, "states": %[1]d, "name": "Password", "text": "hunter2", "filled": true, "attrs_read": true, "x": 304, "y": 167, "w": 186, "h": 22},
 {"id": "/n/4", "parent": 0, "depth": 2, "role": 43, "states": %[2]d, "name": "Sign in", "x": 615, "y": 167, "w": 58, "h": 22}
]}`, onScreen, buttonStates)
}

// nodeAt answers /tree/node for the login form's button, wherever it is
// now.
func nodeAt(x, y int, states uint64, at string) func(url.Values) string {
	return func(q url.Values) string {
		if q.Get("app") != ":1.0" || q.Get("doc") != "/d/1" || q.Get("id") != "/n/4" {
			return `{"stale": true}`
		}
		return fmt.Sprintf(`{"stale": false, "role": 43, "states": %d, "name": "Sign in", "x": %d, "y": %d, "w": 58, "h": 22,
"doc": {"app": ":1.0", "id": "/d/1", "x": 0, "y": 87, "w": 1440, "h": 781}, "screen_width": 1440, "screen_height": 900%s}`, states, x, y, at)
	}
}

func (f *fixture) setTree(tree string, node func(url.Values) string) {
	f.b.mu.Lock()
	f.b.tree, f.b.node = tree, node
	f.b.mu.Unlock()
}

// vsnapshot calls visual-snapshot, returning the status and the text or
// the error.
func (f *fixture) vsnapshot(token, id, body string) (int, string) {
	f.t.Helper()
	var out map[string]string
	code := f.call(token, http.MethodPost, "/api/sessions/"+id+"/visual-snapshot", body, &out)
	if code == http.StatusOK {
		return code, out["text"]
	}
	return code, out["error"]
}

var refOnLine = regexp.MustCompile(`(?m)^(v[a-z][a-z0-9]n[0-9]+) (.*)$`)

func refNamed(t *testing.T, snap, what string) string {
	t.Helper()
	for _, m := range refOnLine.FindAllStringSubmatch(snap, -1) {
		if strings.Contains(m[2], what) {
			return m[1]
		}
	}
	t.Fatalf("no ref for %q in\n%s", what, snap)
	return ""
}

func (f *fixture) clickBody(id, body string) (int, string) {
	f.t.Helper()
	var out map[string]string
	code := f.call(f.all, http.MethodPost, "/api/sessions/"+id+"/click", body, &out)
	if code == http.StatusOK {
		return code, out["text"]
	}
	return code, out["error"]
}

func (f *fixture) inputCount() int {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return len(f.b.inputs)
}

func TestVisualSnapshot(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.setTree(loginTree(onScreen), nil)
	code, snap := f.vsnapshot(f.all, s.ID, "")
	if code != http.StatusOK {
		t.Fatalf("visual-snapshot: %d %s", code, snap)
	}
	for _, want := range []string{"<untrusted-content-", "URL: https://example.com/login", "Title: Sign in", "-- in view --",
		`textbox "Email" = "ana@example.com" @143,178`, `textbox "Password" = [filled] @397,178`, `button "Sign in" @644,178`} {
		if !strings.Contains(snap, want) {
			t.Errorf("no %q in\n%s", want, snap)
		}
	}
	if strings.Contains(snap, "hunter2") {
		t.Fatalf("the password's text:\n%s", snap)
	}
	// The budget: 0 gives the URL and title only.
	if code, snap := f.vsnapshot(f.all, s.ID, `{"max_chars": 0}`); code != http.StatusOK || strings.Contains(snap, "textbox") {
		t.Fatalf("max_chars 0: %d %s", code, snap)
	}
	for _, body := range []string{`{"max_chars": 20001}`, `{"max_chars": -1}`, `{"query": "x", "max_chars": 0}`, `{"nope": 1}`, `{"query": "` + strings.Repeat("q", 201) + `"}`} {
		if code, msg := f.vsnapshot(f.all, s.ID, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, msg)
		}
	}
	// The reader's failures.
	f.setTree("", nil)
	if code, msg := f.vsnapshot(f.all, s.ID, ""); code != http.StatusConflict || !strings.Contains(msg, "Visual mode") {
		t.Fatalf("an image with no /tree: %d %s", code, msg)
	}
	f.setTree(`{"doc": {"url": 5}`, nil)
	if code, msg := f.vsnapshot(f.all, s.ID, ""); code != http.StatusBadGateway || !strings.Contains(msg, "<untrusted-content-") {
		t.Fatalf("a broken answer: %d %s", code, msg)
	}
}

func TestVisualSnapshotRefusals(t *testing.T) {
	f := newFixture(t)
	f.setTree(loginTree(onScreen), nodeAt(615, 167, onScreen, ""))
	api := f.start(f.all, "google", "api")
	if code, msg := f.vsnapshot(f.all, api.ID, ""); code != http.StatusConflict || !strings.Contains(msg, "snapshot") {
		t.Fatalf("API mode: %d %s", code, msg)
	}
	if code, msg := f.clickBody(api.ID, `{"ref": "vabn1"}`); code != http.StatusConflict || !strings.Contains(msg, "API mode") {
		t.Fatalf("click by ref in API mode: %d %s", code, msg)
	}
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+api.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	// Another profile's session, for a token scoped to google only.
	gh := f.start(f.all, "github", "visual")
	if code, msg := f.vsnapshot(f.scoped, gh.ID, ""); code != http.StatusNotFound {
		t.Fatalf("another profile's session: %d %s", code, msg)
	}
	s := f.start(f.all, "google", "visual")
	if code, _ := f.vsnapshot(f.all, s.ID, ""); code != http.StatusOK {
		t.Fatalf("visual: %d", code)
	}
	// A person driving from the live view, then a handoff.
	if err := takeControlSoon(f.m, s.ID); err != nil {
		t.Fatal(err)
	}
	if code, msg := f.vsnapshot(f.all, s.ID, ""); code != http.StatusConflict || !strings.Contains(msg, "person") {
		t.Fatalf("a person driving: %d %s", code, msg)
	}
	f.m.ReleaseControl(s.ID)
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if code, msg := f.vsnapshot(f.all, s.ID, ""); code != http.StatusConflict {
		t.Fatalf("during a handoff: %d %s", code, msg)
	}
	if code, msg := f.clickBody(s.ID, `{"ref": "vabn1"}`); code != http.StatusConflict {
		t.Fatalf("click during a handoff: %d %s", code, msg)
	}
}

// takeControlSoon takes control once the agent's quiet time is over.
func takeControlSoon(m *sessions.Manager, id string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := m.TakeControl(id)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestVisualSnapshotRevokedMidRead(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	hold := make(chan struct{})
	f.b.mu.Lock()
	f.b.tree, f.b.treeHold = loginTree(onScreen), hold
	f.b.mu.Unlock()
	defer close(hold)
	tok, secret, err := f.tokens.Create(context.Background(), "doomed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	var msg string
	go func() {
		code, m := f.vsnapshot(secret, s.ID, "")
		msg = m
		done <- code
	}()
	waitFor(t, func() bool { f.b.mu.Lock(); defer f.b.mu.Unlock(); return f.b.treeGets > 0 })
	f.m.DisconnectToken(tok.ID)
	select {
	case code := <-done:
		if code != http.StatusUnauthorized {
			t.Fatalf("revoked mid-read: %d %s", code, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read went on after the token was revoked")
	}
}

func TestClickByRefThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	// The button moved since the snapshot: the click goes where it is now.
	f.setTree(loginTree(onScreen), nodeAt(700, 300, onScreen, ""))
	_, snap := f.vsnapshot(f.all, s.ID, "")
	btn := refNamed(t, snap, `button "Sign in"`)
	code, msg := f.clickBody(s.ID, `{"ref": "`+btn+`", "double": true}`)
	if code != http.StatusOK || !strings.Contains(msg, "Clicked "+btn) || !strings.Contains(msg, `button "Sign in"`) || !strings.Contains(msg, "<untrusted-content-") {
		t.Fatalf("click by ref: %d %s", code, msg)
	}
	var sent struct {
		X, Y   int
		Double bool
		Ref    *string
	}
	in := f.lastInput()
	if err := json.Unmarshal([]byte(strings.TrimPrefix(in, "/input/click ")), &sent); err != nil || sent.X != 729 || sent.Y != 311 || !sent.Double || sent.Ref != nil {
		t.Fatalf("sent %s, want the centre now, (729, 311), double, and no ref", in)
	}
	// Through /computer too.
	n := f.inputCount()
	code, _, body := f.computer(s.ID, `{"action": "click", "ref": "`+btn+`"}`)
	if code != http.StatusOK || !strings.Contains(string(body), `button \"Sign in\"`) || f.inputCount() != n+1 {
		t.Fatalf("/computer click by ref: %d %s", code, body)
	}

	// Refused, with nothing sent to /input/click.
	n = f.inputCount()
	for _, c := range []struct {
		name, body string
		node       func(url.Values) string
		code       int
		want       string
	}{
		{"ref and a point", `{"ref": "` + btn + `", "x": 1, "y": 1}`, nil, http.StatusBadRequest, "not both"},
		{"a page tools ref", `{"ref": "a7e12"}`, nil, http.StatusBadRequest, "page tools"},
		{"not a ref", `{"ref": "button"}`, nil, http.StatusBadRequest, "isn't a ref"},
		{"another epoch's", `{"ref": "vzzn1"}`, nil, http.StatusConflict, "stale"},
		{"off screen", `{"ref": "` + btn + `"}`, nodeAt(615, 1200, 1<<30|1<<8, ""), http.StatusConflict, "scroll it into view"},
		{"under the taskbar", `{"ref": "` + btn + `"}`, nodeAt(615, 860, onScreen, ""), http.StatusConflict, "scroll it into view"},
		{"above the page", `{"ref": "` + btn + `"}`, nodeAt(615, 40, onScreen, ""), http.StatusConflict, "scroll it into view"},
		{"covered", `{"ref": "` + btn + `"}`, nodeAt(615, 167, onScreen, `, "at": {"id": "/n/9", "role": 16, "name": "Cookies </untrusted-content-x>"}`), http.StatusConflict, "covered"},
		// The reader's hit test failed: no confirmed hit, no click.
		{"hit test failed", `{"ref": "` + btn + `"}`, func(url.Values) string {
			return "!502 reading the accessibility bus failed: the hit test at the element's centre: NoReply"
		}, http.StatusBadGateway, "couldn't read"},
		{"hit test timed out", `{"ref": "` + btn + `"}`, func(url.Values) string { return "!504 reading the page took too long" }, http.StatusGatewayTimeout, "too long"},
		{"gone", `{"ref": "` + btn + `"}`, func(url.Values) string { return `{"stale": true}` }, http.StatusConflict, "stale"},
	} {
		if c.node != nil {
			f.setTree(loginTree(onScreen), c.node)
		}
		if c.name == "another epoch's" && strings.HasPrefix(btn, "vzz") {
			continue
		}
		code, msg := f.clickBody(s.ID, c.body)
		if code != c.code || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %d %s", c.name, code, msg)
		}
		if c.name == "covered" && strings.Count(msg, "</untrusted-content-x>") != 0 {
			t.Errorf("covered: the page's name closed a fence: %s", msg)
		}
	}
	if f.inputCount() != n {
		t.Fatalf("refused clicks reached the browser: %v", f.b.inputs[n:])
	}
	// A gone element's ref stays stale.
	f.setTree(loginTree(onScreen), nodeAt(615, 167, onScreen, ""))
	if code, msg := f.clickBody(s.ID, `{"ref": "`+btn+`"}`); code != http.StatusConflict {
		t.Fatalf("a gone element's ref, again: %d %s", code, msg)
	}
	// Scroll and type keep their coordinates.
	if code, _, body := f.computer(s.ID, `{"action": "scroll", "ref": "`+btn+`", "dy": 1}`); code != http.StatusBadRequest {
		t.Fatalf("scroll by ref: %d %s", code, body)
	}

	activity := strings.Join(f.whats(s.ID), "\n")
	if !strings.Contains(activity, "claude: Read the screen's elements") || !strings.Contains(activity, "claude: Double-clicked "+btn) || !strings.Contains(activity, "claude: Clicked "+btn) {
		t.Fatalf("activity:\n%s", activity)
	}
	for _, leak := range []string{"Sign in", "Email", "ana@example.com", "example.com/login", "Password"} {
		if strings.Contains(activity, leak) {
			t.Fatalf("tree text %q in the activity log:\n%s", leak, activity)
		}
	}
}

func TestVisualRefsGoStale(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.setTree(loginTree(onScreen), nodeAt(615, 167, onScreen, ""))
	_, snap := f.vsnapshot(f.all, s.ID, "")
	btn := refNamed(t, snap, `button "Sign in"`)
	if code, msg := f.clickBody(s.ID, `{"ref": "`+btn+`"}`); code != http.StatusOK {
		t.Fatalf("click: %d %s", code, msg)
	}
	// Another document: a snapshot of it drops the old page's refs.
	f.setTree(strings.Replace(loginTree(onScreen), `"id": "/d/1"`, `"id": "/d/2"`, 1), nodeAt(615, 167, onScreen, ""))
	f.vsnapshot(f.all, s.ID, "")
	if code, msg := f.clickBody(s.ID, `{"ref": "`+btn+`"}`); code != http.StatusConflict || !strings.Contains(msg, "stale") {
		t.Fatalf("a ref from before a navigation: %d %s", code, msg)
	}
	// A person had the browser since: every ref is stale.
	f.setTree(loginTree(onScreen), nodeAt(615, 167, onScreen, ""))
	_, snap = f.vsnapshot(f.all, s.ID, "")
	btn = refNamed(t, snap, `button "Sign in"`)
	if err := takeControlSoon(f.m, s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	if code, msg := f.clickBody(s.ID, `{"ref": "`+btn+`"}`); code != http.StatusConflict || !strings.Contains(msg, "stale") {
		t.Fatalf("a ref from before a person had the browser: %d %s", code, msg)
	}
	// An orchestrator restart: a new API on the same browsers.
	_, snap = f.vsnapshot(f.all, s.ID, "")
	btn = refNamed(t, snap, `button "Sign in"`)
	a := New(f.m, f.web, files.New(f.m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), slog.New(slog.DiscardHandler), "")
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/sessions/"+s.ID+"/click", strings.NewReader(`{"ref": "`+btn+`"}`))
	req.Header.Set("Authorization", "Bearer "+f.all)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a ref from before a restart: %d", resp.StatusCode)
	}
}

// TestVisualReadsDontKeepAPersonOut: reads don't count as the agent
// driving (PROJ-134); a click by ref does, as any click.
func TestVisualReadsDontKeepAPersonOut(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.setTree(loginTree(onScreen), nodeAt(615, 167, onScreen, ""))
	if err := takeControlSoon(f.m, s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); f.vsnapshot(f.all, s.ID, "") }()
	}
	wg.Wait()
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatalf("Take control right after reads: %v", err)
	}
	f.m.ReleaseControl(s.ID)
	_, snap := f.vsnapshot(f.all, s.ID, "")
	if code, msg := f.clickBody(s.ID, `{"ref": "`+refNamed(t, snap, "Sign in")+`"}`); code != http.StatusOK {
		t.Fatalf("click: %d %s", code, msg)
	}
	if err := f.m.TakeControl(s.ID); err == nil {
		t.Fatal("Take control right after a click by ref: want it refused for a few seconds")
	}
}

// TestClickByRefAfterAHandBack: a person takes the browser and hands it
// back between a click by ref's lookup and its dispatch. The point was
// read from the page they left, so the click is refused and never
// reaches the browser, through /click and /computer alike.
func TestClickByRefAfterAHandBack(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.setTree(loginTree(onScreen), nodeAt(615, 167, onScreen, ""))
	for _, endpoint := range []string{"click", "computer"} {
		_, snap := f.vsnapshot(f.all, s.ID, "")
		btn := refNamed(t, snap, `button "Sign in"`)
		f.api.afterLocate = func() {
			if err := takeControlSoon(f.m, s.ID); err != nil {
				t.Error(err)
			}
			f.m.ReleaseControl(s.ID)
		}
		n := f.inputCount()
		var code int
		var msg string
		if endpoint == "click" {
			code, msg = f.clickBody(s.ID, `{"ref": "`+btn+`"}`)
		} else {
			var body []byte
			code, _, body = f.computer(s.ID, `{"action": "click", "ref": "`+btn+`"}`)
			msg = string(body)
		}
		f.api.afterLocate = nil
		if code != http.StatusConflict || !strings.Contains(msg, "visual_snapshot") {
			t.Errorf("%s by ref across a hand-back: %d %s", endpoint, code, msg)
		}
		if f.inputCount() != n {
			t.Fatalf("%s: the click reached the browser: %v", endpoint, f.b.inputs[n:])
		}
		// A new snapshot's refs click again.
		_, snap = f.vsnapshot(f.all, s.ID, "")
		if code, msg := f.clickBody(s.ID, `{"ref": "`+refNamed(t, snap, `button "Sign in"`)+`"}`); code != http.StatusOK {
			t.Fatalf("%s: a new ref: %d %s", endpoint, code, msg)
		}
	}
}
