//go:build docker

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// withID posts body to a session's tool with a call id, as a client that
// waits at most timeout (0: as long as it takes).
func (a agent) withID(id, tool, callID string, body any, timeout time.Duration) (code int, replayed bool, out struct {
	Call        *callJSON
	Text, Error string
}) {
	a.t.Helper()
	raw, _ := json.Marshal(body)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/api/sessions/"+id+"/"+tool, strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set(IdempotencyHeader, callID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false, out // the client gave up
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, resp.Header.Get(ReplayedHeader) == "true", out
}

// callState is a call's receipt's state, once it has ended.
func (a agent) callState(t *testing.T, id, callID string) string {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var out struct{ Call callJSON }
		if code := a.do(http.MethodGet, "/api/sessions/"+id+"/calls/"+callID, "", &out); code != http.StatusOK {
			t.Errorf("call %s: %d", callID, code)
			return ""
		}
		if st := out.Call.State; (st != "waiting" && st != "running") || time.Now().After(deadline) {
			return st
		}
	}
}

// count is how many times a page reported e.
func count(site *browsertest.PageSite, e string) int {
	n := 0
	for _, got := range site.Events() {
		if got == e {
			n++
		}
	}
	return n
}

// TestARepeatedActRunsOnce (PROJ-156): an act sent twice with the same
// call id clicks once; the second answer says it ran already.
func TestARepeatedActRunsOnce(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "i")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/counter"})
	click := steps(map[string]any{"action": "click", "target": `role=button[name="Add one"]`})
	code, replayed, out := a.withID(s.ID, "act", "add-1", click, 0)
	if code != http.StatusOK || replayed || !strings.Contains(out.Text, "clicked") {
		t.Fatalf("act: %d %+v", code, out)
	}
	code, replayed, out = a.withID(s.ID, "act", "add-1", click, 0)
	if code != http.StatusOK || !replayed || !strings.Contains(out.Text, "ran already") || out.Call == nil || out.Call.State != "ok" {
		t.Errorf("act again: %d %t %+v", code, replayed, out)
	}
	time.Sleep(time.Second)
	if n := count(site, "counted"); n != 1 {
		t.Fatalf("the button was clicked %d times", n)
	}
	// A new id is a new click.
	if code, _, out := a.withID(s.ID, "act", "add-2", click, 0); code != http.StatusOK {
		t.Fatalf("act add-2: %d %+v", code, out)
	}
	for deadline := time.Now().Add(5 * time.Second); count(site, "counted") != 2 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if n := count(site, "counted"); n != 2 {
		t.Fatalf("after add-2 the button was clicked %d times", n)
	}
}

// TestARepeatedVisualClickRunsOnce: the same in Visual mode, with a
// /computer click.
func TestARepeatedVisualClickRunsOnce(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("j%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l", "pace": "instant"}`,
		fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, site.URL+"/counter"),
		`{"action": "key", "keys": "Return", "pace": "instant"}`,
	} {
		if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/computer", body, nil); code != http.StatusNoContent {
			t.Fatalf("%s: %d", body, code)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); !site.HasEvent("counter loaded"); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the page never loaded")
		}
	}
	time.Sleep(500 * time.Millisecond)
	sw, sh := a.screenSize(t, s.ID)
	click := map[string]any{"action": "click", "x": sw / 2, "y": sh / 2}
	if code, replayed, out := a.withID(s.ID, "computer", "add-1", click, 0); code != http.StatusNoContent || replayed {
		t.Fatalf("click: %d %+v", code, out)
	}
	code, replayed, out := a.withID(s.ID, "computer", "add-1", click, 0)
	if code != http.StatusOK || !replayed || !strings.Contains(out.Text, "ran already") || out.Call == nil || out.Call.State != "ok" {
		t.Errorf("click again: %d %t %+v", code, replayed, out)
	}
	time.Sleep(time.Second)
	if n := count(site, "counted"); n != 1 {
		t.Fatalf("the button was clicked %d times", n)
	}
	// Still Visual mode's rule: nothing attached to the page.
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("a CDP client is attached to %s", tg.URL)
		}
	}
}

// TestARepeatAfterAnInterruptedTypeDoesNotTypeAgain: a human-paced type
// whose client closed the connection is cut short; the same call again
// answers interrupted and types nothing more.
func TestARepeatAfterAnInterruptedTypeDoesNotTypeAgain(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "k")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/login"})
	email := refFor(t, a.mustTool(s.ID, "snapshot", nil), `"Email"`)
	value := func() string {
		t.Helper()
		m := regexp.MustCompile(`"Email" = "([^"]*)"`).FindStringSubmatch(a.mustTool(s.ID, "snapshot", nil))
		if m == nil {
			return ""
		}
		return m[1]
	}
	text := strings.Repeat("a", 80) // about 20 s at human pace
	typing := steps(map[string]any{"action": "type", "ref": email, "text": text})
	if code, _, _ := a.withID(s.ID, "act", "type-1", typing, 3*time.Second); code != 0 {
		t.Fatalf("the type answered %d within 3 s", code)
	}
	if st := a.callState(t, s.ID, "type-1"); st != "interrupted" {
		t.Errorf("the cut type ended %q", st)
	}
	typed := value()
	if typed == "" || len(typed) >= len(text) {
		t.Fatalf("the cut type typed %q", typed)
	}
	// Were it run again, it would type for 20 s: 3 s show it.
	code, replayed, out := a.withID(s.ID, "act", "type-1", typing, 3*time.Second)
	if code != http.StatusConflict || !replayed || out.Call == nil || out.Call.State != "interrupted" || !strings.Contains(out.Error, "new call id") {
		t.Errorf("the type again: %d %t %+v", code, replayed, out)
	}
	time.Sleep(2 * time.Second)
	if again := value(); again != typed {
		t.Fatalf("the field grew from %q to %q", typed, again)
	}
	t.Logf("the cut type typed %d of %d characters, and nothing after", len(typed), len(text))
}

// TestARepeatedNewTabOpensOne: tabs new sent twice with the same call id
// opens one tab.
func TestARepeatedNewTabOpensOne(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "l")
	tabs := func() int { return strings.Count(a.mustTool(s.ID, "tabs", nil), "\nt") + 1 }
	before := tabs()
	body := map[string]string{"action": "new", "url": site.URL + "/counter"}
	if code, replayed, out := a.withID(s.ID, "tabs", "tab-1", body, 0); code != http.StatusOK || replayed {
		t.Fatalf("tabs new: %d %+v", code, out)
	}
	if code, replayed, out := a.withID(s.ID, "tabs", "tab-1", body, 0); code != http.StatusOK || !replayed || out.Call == nil || out.Call.State != "ok" {
		t.Errorf("tabs new again: %d %t %+v", code, replayed, out)
	}
	if n := tabs(); n != before+1 {
		t.Fatalf("%d tabs, from %d:\n%s", n, before, a.mustTool(s.ID, "tabs", nil))
	}
}

// TestALoginByActWithACallIDSurvivesAStop is AGENTS.md's run for
// PROJ-156: sign in to a test site with an act carrying a call id, send it
// again and see it not run, stop the browser, start it again, and find the
// login still there.
func TestALoginByActWithACallIDSurvivesAStop(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "m")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/login"})
	var boxes []string
	for _, l := range strings.Split(a.mustTool(s.ID, "snapshot", nil), "\n") {
		if strings.Contains(l, "textbox") {
			boxes = append(boxes, refFor(t, l, "textbox"))
		}
	}
	if len(boxes) != 2 {
		t.Fatalf("textboxes: %v", boxes)
	}
	login := steps(
		map[string]any{"action": "fill", "ref": boxes[0], "text": "pier"},
		map[string]any{"action": "fill", "ref": boxes[1], "text": "hunter2"},
		map[string]any{"action": "click", "target": `role=button[name="Sign in"]`},
	)
	if code, replayed, out := a.withID(s.ID, "act", "login-1", login, 0); code != http.StatusOK || replayed {
		t.Fatalf("login: %d %+v", code, out)
	}
	for deadline := time.Now().Add(10 * time.Second); site.Issued() == "" && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	token := site.Issued()
	if token == "" {
		t.Fatal("the site issued no session")
	}
	if code, replayed, out := a.withID(s.ID, "act", "login-1", login, 0); code != http.StatusOK || !replayed || !strings.Contains(out.Text, "wasn't run again") {
		t.Fatalf("login again: %d %t %+v", code, replayed, out)
	}
	if site.Issued() != token {
		t.Fatal("the login ran twice")
	}
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	var next sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, s.Profile), &next); code != http.StatusCreated || next.ID == s.ID {
		t.Fatalf("start again: %d %+v", code, next)
	}
	// A new session: the old call id is gone with the old one.
	if code := a.do(http.MethodGet, "/api/sessions/"+next.ID+"/calls/login-1", "", nil); code != http.StatusNotFound {
		t.Fatalf("login-1 in the new session: %d", code)
	}
	a.mustTool(next.ID, "navigate", map[string]string{"url": site.URL + "/account"})
	if snap := a.mustTool(next.ID, "snapshot", nil); !strings.Contains(snap, "Signed in as pier") || site.Seen() != token {
		t.Fatalf("after stop and start the site saw %q, want %q:\n%s", site.Seen(), token, snap)
	}
}
