//go:build docker

package api

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// PROJ-151: while a page has the keyboard, Chrome moves the focus to the
// address bar for ctrl+l only once the page has answered the key, so text
// sent right after it went to a page whose key handler is busy. The
// container waits shortcutGap (150 ms) before an agent's next keyboard
// action; these tests drive Visual mode through /computer, back to back.

// keyRaceSlow is how long the test page's key handler takes: shortcutGap
// less 50 ms, which the gap covers. Without the gap, text sent at once
// reaches this page (images/chromium/ANTI-BOT.md).
const keyRaceSlow = 100

// keyRaceSession starts a Visual-mode session on a new 1280x800 profile
// whose pages reach the test sites.
func keyRaceSession(t *testing.T, a agent) (name string, s sessionJSON) {
	t.Helper()
	name = fmt.Sprintf("k%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual", "screen": {"width": 1280, "height": 800}}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	return name, s
}

// computer posts one /computer action.
func (a agent) computer(id, body string) {
	a.t.Helper()
	if code := a.do(http.MethodPost, "/api/sessions/"+id+"/computer", body, nil); code != http.StatusNoContent && code != http.StatusOK {
		a.t.Fatalf("%s: %d", body, code)
	}
}

// openWithCare opens url as a careful person does: a click in the address
// bar (the browser's, not the page's), a pause, the URL, Return; then it
// waits for /keys run n to have drawn.
func openWithCare(t *testing.T, a agent, id string, ks *browsertest.KeySite, url, n string) {
	t.Helper()
	a.computer(id, `{"action": "click", "x": 500, "y": 63, "pace": "instant"}`)
	time.Sleep(300 * time.Millisecond)
	a.computer(id, `{"action": "key", "keys": "ctrl+a", "pace": "instant"}`)
	time.Sleep(300 * time.Millisecond)
	a.computer(id, fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, url))
	a.computer(id, `{"action": "key", "keys": "Return", "pace": "instant"}`)
	if _, ok := ks.WaitRun(n, 15*time.Second, func(r browsertest.KeyRun) bool { return r.Ready }); !ok {
		t.Fatalf("the /keys page %s never loaded", n)
	}
}

// onlyTheShortcut checks run n's page saw no key but the shortcut's, and
// its field stayed empty.
func onlyTheShortcut(t *testing.T, ks *browsertest.KeySite, n string) {
	t.Helper()
	r := ks.Run(n)
	for _, e := range r.Keys {
		if e.Key != "Control" && !(e.Ctrl && e.Key == "l") {
			t.Errorf("run %s: the page saw %q (all: %+v)", n, e.Key, r.Keys)
			break
		}
	}
	if r.Value != "" {
		t.Errorf("run %s: the page's field has %q", n, r.Value)
	}
}

// noClientAttached checks, over a browser-level connection, that no CDP
// client is attached to any page.
func noClientAttached(t *testing.T, a agent, name string) {
	t.Helper()
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("in Visual mode, a CDP client is attached to %s", tg.URL)
		}
	}
}

func TestTextAfterShortcutReachesTheAddressBar(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	name, s := keyRaceSession(t, a)
	openWithCare(t, a, s.ID, ks, ks.Keys("k0", keyRaceSlow, "field"), "k0")
	for i := range 20 {
		n, next := fmt.Sprintf("k%d", i), fmt.Sprintf("k%d", i+1)
		to := ks.Arrived("a"+n, ks.Keys(next, keyRaceSlow, "field"))
		a.computer(s.ID, `{"action": "key", "keys": "ctrl+l", "pace": "instant"}`)
		a.computer(s.ID, fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, to))
		a.computer(s.ID, `{"action": "key", "keys": "Return", "pace": "instant"}`)
		if _, ok := ks.WaitRun("a"+n, 5*time.Second, func(r browsertest.KeyRun) bool { return r.Arrived }); !ok {
			time.Sleep(time.Second) // for the page's last reports
			r := ks.Run(n)
			t.Fatalf("round %d: the address never opened; the page saw %d keys, its field %q", i, len(r.Keys), r.Value)
		}
		if _, ok := ks.WaitRun(next, 15*time.Second, func(r browsertest.KeyRun) bool { return r.Ready }); !ok {
			t.Fatalf("round %d: the next /keys page never loaded", i)
		}
		onlyTheShortcut(t, ks, n)
	}
	noClientAttached(t, a, name)
}

func TestShortcutListKeepsItsOrder(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	name, s := keyRaceSession(t, a)
	openWithCare(t, a, s.ID, ks, ks.Keys("l0", keyRaceSlow, "field"), "l0")
	a.computer(s.ID, `{"action": "key", "keys": ["ctrl+l", "a", "b", "c"], "pace": "instant"}`)
	time.Sleep(time.Second) // the page's reports
	r := ks.Run("l0")
	for _, e := range r.Keys {
		if e.Key == "a" || e.Key == "b" || e.Key == "c" {
			t.Fatalf("the page saw %q: %+v", e.Key, r.Keys)
		}
	}
	if r.Value != "" {
		t.Fatalf("the page's field has %q", r.Value)
	}
	noClientAttached(t, a, name)
}

func TestTypingToThePageIsNotSlowed(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	name, s := keyRaceSession(t, a)
	openWithCare(t, a, s.ID, ks, ks.Keys("t0", 0, "field"), "t0")
	a.computer(s.ID, `{"action": "type", "text": "hello", "pace": "instant"}`)
	a.computer(s.ID, `{"action": "type", "text": "world", "pace": "instant"}`)
	r, ok := ks.WaitRun("t0", 5*time.Second, func(r browsertest.KeyRun) bool { return r.Value == "helloworld" })
	if !ok {
		t.Fatalf("the field has %q, want helloworld", r.Value)
	}
	var typed strings.Builder
	for i, e := range r.Keys {
		typed.WriteString(e.Key)
		// Between the calls: the time a call takes to reach xdotool, well
		// under the gap. Waiting it out here would mean typing waits.
		if i > 0 {
			if gap := e.T - r.Keys[i-1].T; gap >= 150 {
				t.Errorf("%.0f ms between %q and %q", gap, r.Keys[i-1].Key, e.Key)
			}
		}
	}
	if typed.String() != "helloworld" {
		t.Errorf("the page saw %q", typed.String())
	}
	noClientAttached(t, a, name)
}

// TestTextAfterShortcutOnARestartedBrowser: a browser restarted on its
// profile restores its tabs, still loading as the agent's first calls
// come. It doubles as AGENTS.md's persistence run: the login made before
// the stop is still there.
func TestTextAfterShortcutOnARestartedBrowser(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	name, s := keyRaceSession(t, a)
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	// Sign in over CDP, in API mode, and leave the tab on the slow keys
	// page, which takes a second to load.
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": "api"}`, name), &s); code != http.StatusCreated {
		t.Fatalf("API start: %d", code)
	}
	p := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	token := site.Login(t, p)
	p.Navigate(browsertest.Loading(ks.Keys("r0", keyRaceSlow, "field"), 1000))
	p.Close()
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}

	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": "visual"}`, name), &s); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	to := ks.Arrived("ar0", site.URL+"/account")
	a.computer(s.ID, `{"action": "key", "keys": "ctrl+l", "pace": "instant"}`)
	a.computer(s.ID, fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, to))
	a.computer(s.ID, `{"action": "key", "keys": "Return", "pace": "instant"}`)
	if _, ok := ks.WaitRun("ar0", 10*time.Second, func(r browsertest.KeyRun) bool { return r.Arrived }); !ok {
		t.Fatalf("the address never opened; the restored page saw %+v", ks.Run("r0").Keys)
	}
	deadline := time.Now().Add(10 * time.Second)
	for site.Seen() != token && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if site.Seen() != token {
		t.Fatalf("not signed in after the restart: the site saw %q, issued %q", site.Seen(), token)
	}
	noClientAttached(t, a, name)
}
