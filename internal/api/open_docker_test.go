//go:build docker

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// PROJ-164: Visual mode's open action clicks the address bar, which moves
// the focus to it without asking the page, so a page whose key handler is
// busy can't take the address, as it takes text typed after ctrl+l
// (keyrace_docker_test.go).

// openSession starts a Visual-mode session on a new profile with a
// screen of w×h, whose pages reach the test sites.
func openSession(t *testing.T, a agent, w, h int) (name string, s sessionJSON) {
	t.Helper()
	name = fmt.Sprintf("o%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual", "screen": {"width": %d, "height": %d}}`, name, w, h), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	return name, s
}

// open sends one open action and returns its status and error.
func (a agent) open(id, url, pace string) (int, string) {
	a.t.Helper()
	body := fmt.Sprintf(`{"action": "open", "url": %q, "pace": %q}`, url, pace)
	req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+id+"/computer", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct{ Error string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Error
}

// mustOpen opens url and waits for /keys run n to have drawn.
func mustOpen(t *testing.T, a agent, id string, ks *browsertest.KeySite, url, n, pace string) {
	t.Helper()
	if code, msg := a.open(id, url, pace); code != http.StatusNoContent {
		t.Fatalf("open %s: %d %s", url, code, msg)
	}
	if _, ok := ks.WaitRun(n, 20*time.Second, func(r browsertest.KeyRun) bool { return r.Ready }); !ok {
		t.Fatalf("the /keys page %s never loaded", n)
	}
}

// noKeys checks run n's page saw no key at all and its field is empty.
func noKeys(t *testing.T, ks *browsertest.KeySite, n string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond) // the page's last reports
	if r := ks.Run(n); len(r.Keys) > 0 || r.Value != "" {
		t.Errorf("run %s: the page saw %d keys (%+v), its field %q", n, len(r.Keys), r.Keys, r.Value)
	}
}

// TestOpenBehindABusyPage is the ticket's acceptance test: behind a key
// handler that takes 400 ms a key, where ctrl+l loses every run
// (images/chromium/ANTI-BOT.md), open reaches the address 20 times of 20
// at instant pace, and 3 of 3 at human pace, and the page sees no key.
func TestOpenBehindABusyPage(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	name, s := openSession(t, a, 1280, 800)
	mustOpen(t, a, s.ID, ks, ks.Keys("b0", 400, "field"), "b0", "instant")
	round := 0
	for _, c := range []struct {
		pace string
		runs int
	}{{"instant", 20}, {"human", 3}} {
		for range c.runs {
			n, next := fmt.Sprintf("b%d", round), fmt.Sprintf("b%d", round+1)
			if r := ks.Run(n); !r.Focus || r.Active != "f" {
				t.Fatalf("round %d: the page doesn't have the keyboard (focus %t, active %q)", round, r.Focus, r.Active)
			}
			to := ks.Arrived("a"+n, ks.Keys(next, 400, "field"))
			start := time.Now()
			if code, msg := a.open(s.ID, to, c.pace); code != http.StatusNoContent {
				t.Fatalf("round %d (%s): %d %s", round, c.pace, code, msg)
			}
			took := time.Since(start)
			if _, ok := ks.WaitRun("a"+n, 5*time.Second, func(r browsertest.KeyRun) bool { return r.Arrived }); !ok {
				time.Sleep(time.Second)
				r := ks.Run(n)
				t.Fatalf("round %d (%s): the address never opened; the page saw %d keys, its field %q", round, c.pace, len(r.Keys), r.Value)
			}
			if _, ok := ks.WaitRun(next, 15*time.Second, func(r browsertest.KeyRun) bool { return r.Ready }); !ok {
				t.Fatalf("round %d: the next /keys page never loaded", round)
			}
			noKeys(t, ks, n)
			t.Logf("round %d (%s): opened in %v", round, c.pace, took.Round(time.Millisecond))
			round++
		}
	}
	noClientAttached(t, a, name)
}

// TestOpenWithSomethingInTheWay: open works with a page's alert up, and
// is refused, pressing nothing, while a pop-up window covers the address
// bar or the browser is full screen; once that is gone, it works again.
func TestOpenWithSomethingInTheWay(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	name, s := openSession(t, a, 1280, 800)
	settle := func() { time.Sleep(1500 * time.Millisecond) }

	// An alert: the address bar is still the browser's, and opening an
	// address goes past the dialog.
	if code, msg := a.open(s.ID, ks.Dialog("alert"), "instant"); code != http.StatusNoContent {
		t.Fatalf("open the alert page: %d %s", code, msg)
	}
	settle()
	a.computer(s.ID, `{"action": "click", "x": 640, "y": 400, "pace": "instant"}`)
	settle()
	mustOpen(t, a, s.ID, ks, ks.Keys("d0", 0, "field"), "d0", "instant")

	// A pop-up window as big as the browser's, over it.
	if code, msg := a.open(s.ID, ks.Dialog("popup"), "instant"); code != http.StatusNoContent {
		t.Fatalf("open the pop-up page: %d %s", code, msg)
	}
	settle()
	a.computer(s.ID, `{"action": "click", "x": 640, "y": 400, "pace": "instant"}`)
	settle()
	for _, pace := range []string{"instant", "human"} {
		if code, msg := a.open(s.ID, ks.Keys("never", 0, "field"), pace); code != http.StatusConflict || !strings.Contains(msg, "pop-up window") {
			t.Fatalf("%s, with a pop-up over the address bar: %d %q", pace, code, msg)
		}
	}
	a.computer(s.ID, `{"action": "key", "keys": "ctrl+w", "pace": "instant"}`) // closes the pop-up, in front
	settle()
	mustOpen(t, a, s.ID, ks, ks.Keys("d1", 0, "field"), "d1", "instant")

	// Full screen: no address bar.
	a.computer(s.ID, `{"action": "key", "keys": "F11", "pace": "instant"}`)
	settle()
	if code, msg := a.open(s.ID, ks.Keys("never", 0, "field"), "instant"); code != http.StatusConflict || !strings.Contains(msg, "full screen") {
		t.Fatalf("full screen: %d %q", code, msg)
	}
	time.Sleep(5 * time.Second) // the bubble saying how to leave it goes
	if code, msg := a.open(s.ID, ks.Keys("never", 0, "field"), "human"); code != http.StatusConflict || !strings.Contains(msg, "full screen") {
		t.Fatalf("full screen, its bubble gone: %d %q", code, msg)
	}
	a.computer(s.ID, `{"action": "key", "keys": "F11", "pace": "instant"}`)
	settle()
	mustOpen(t, a, s.ID, ks, ks.Keys("d2", 0, "field"), "d2", "instant")
	if r := ks.Run("never"); r.Ready {
		t.Error("a refused open opened its address")
	}
	noClientAttached(t, a, name)
}

// TestOpenAtEveryScreenSize: the address bar is where the entrypoint
// computes it on the smallest screen BrowserPier allows and on a large
// one (1280x800 is the tests above).
func TestOpenAtEveryScreenSize(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	for _, size := range [][2]int{{640, 480}, {1920, 1080}} {
		n := fmt.Sprintf("s%dx%d", size[0], size[1])
		name, s := openSession(t, a, size[0], size[1])
		mustOpen(t, a, s.ID, ks, ks.Keys(n, 400, "field"), n, "instant")
		next := n + "b"
		mustOpen(t, a, s.ID, ks, ks.Keys(next, 400, "field"), next, "human")
		noKeys(t, ks, n)
		if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
			t.Fatalf("stop: %d", code)
		}
		_ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run()
	}
}

// TestOpenSendsAQueryPastASCII: an address whose query has characters past
// ASCII reaches the site with them, as UTF-8, at either pace. They are
// percent-encoded before they are typed (weburl.Check), since xdotool's
// typing can drop such a character on its way into Chromium.
func TestOpenSendsAQueryPastASCII(t *testing.T) {
	a := newRealAPI(t)
	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	name, s := openSession(t, a, 1280, 800)
	for _, pace := range []string{"instant", "human"} {
		n := "café-日本-" + pace
		ks.Arrived(n, "")
		// Written as a person would write it, not escaped.
		if code, msg := a.open(s.ID, ks.URL+"/arrived?n="+n, pace); code != http.StatusNoContent {
			t.Fatalf("%s: open: %d %s", pace, code, msg)
		}
		if _, ok := ks.WaitRun(n, 10*time.Second, func(r browsertest.KeyRun) bool { return r.Arrived }); !ok {
			t.Fatalf("%s: the site never saw n=%s", pace, n)
		}
	}
	noClientAttached(t, a, name)
}
