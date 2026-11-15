//go:build docker

package api

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestFilledSecretIsHiddenEverywhere is PROJ-125 on a real browser: a
// secret BrowserPier filled (registered through the internal hook PROJ-57
// will call) goes through a GET form, so the page echoes it in its title,
// its text, a link and its URL; snapshot, find, page_content, tabs, act's
// changes and navigate (with Redirected from) say <hidden>, never the
// value in any encoding, nor does the activity log. After a stop and a
// start the set is empty, and the same page shows the value in clear,
// which proves the page really echoes it.
func TestFilledSecretIsHiddenEverywhere(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "scrub")
	const secret = "Pier Secret+4&2é"
	if err := a.m.RegisterSecret(s.ID, secret); err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(secret)
	leaks := []string{secret, url.QueryEscape(secret), url.PathEscape(secret), html.EscapeString(secret),
		strings.Trim(string(quoted), `"`), "Pier Secret", "Pier+Secret", "Pier%20Secret", "Secret+4", "Secret%2B4"}
	check := func(what, out string) {
		t.Helper()
		for _, l := range leaks {
			if strings.Contains(out, l) {
				t.Errorf("%s leaks %q:\n%s", what, l, out)
			}
		}
		if !strings.Contains(out, "<hidden>") {
			t.Errorf("%s doesn't say <hidden>:\n%s", what, out)
		}
	}

	// The sign-in sends the password in its URL: the agent fills it here,
	// standing in for a vault fill, which types what the agent never sees.
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/get-signin"})
	snap := a.mustTool(s.ID, "snapshot", nil)
	a.mustTool(s.ID, "act", steps(map[string]any{"action": "fill", "ref": refFor(t, snap, `textbox "Password"`), "text": secret}))
	acted := a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Sign in"`)}))
	a.mustTool(s.ID, "wait", map[string]any{"text": "You said", "timeout_ms": 10000})
	if !strings.Contains(acted, "navigated to") {
		t.Errorf("act's changes don't say it navigated: %s", acted)
	}
	check("act's changes", acted)

	check("snapshot", a.mustTool(s.ID, "snapshot", nil))
	check("find", a.mustTool(s.ID, "find", map[string]any{"text": "You said"}))
	check("page_content", a.mustTool(s.ID, "content", map[string]any{"format": "markdown"}))
	check("readability", a.mustTool(s.ID, "content", map[string]any{"format": "readability"}))
	check("tabs", a.mustTool(s.ID, "tabs", nil))
	check("navigate reload", a.mustTool(s.ID, "navigate", map[string]string{"action": "reload"}))
	bounced := a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/bounce?p=" + url.QueryEscape(secret)})
	if !strings.Contains(bounced, "Redirected from: ") {
		t.Errorf("no redirect note: %s", bounced)
	}
	check("navigate with a redirect", bounced)
	for _, act := range a.m.Activity(s.ID) {
		for _, l := range leaks {
			if strings.Contains(act.What, l) {
				t.Errorf("the activity log leaks %q: %q", l, act.What)
			}
		}
	}

	// Stop and start: the set goes with the session.
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if n := a.m.SecretCount(s.ID); n != 0 {
		t.Errorf("%d secrets outlived the session", n)
	}
	var s2 sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, s.Profile), &s2); code != http.StatusCreated {
		t.Fatalf("start again: %d %+v", code, s2)
	}
	if n := a.m.SecretCount(s2.ID); n != 0 {
		t.Errorf("the new session starts with %d secrets", n)
	}
	a.mustTool(s2.ID, "navigate", map[string]string{"url": site.URL + "/echo?p=" + url.QueryEscape(secret)})
	if snap := a.mustTool(s2.ID, "snapshot", nil); !strings.Contains(snap, "You said: "+secret) || strings.Contains(snap, "<hidden>") {
		t.Errorf("with no secret registered, the page should show it in clear:\n%s", snap)
	}
}
