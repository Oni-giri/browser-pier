//go:build docker

package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestPageToolsConsentFrame is PROJ-110's case on a real browser: a
// consent banner in an iframe from another site, fixed on top of the
// page's own button. The snapshot marks what it covers, and only that;
// act's error names the iframe and points to a handoff.
func TestPageToolsConsentFrame(t *testing.T) {
	a := newRealAPI(t)
	host := browsertest.SiteHost(t)
	site := browsertest.NewPageSite(t, host)
	s := a.startAPIBrowser(t, "f")
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(site.URL, "http://"))
	other := strings.Replace(site.URL, host, browsertest.OtherAddress(t, "browserpier-"+s.Profile, host, port), 1)
	if other == site.URL {
		t.Fatalf("no second site from %s", site.URL)
	}
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/consent?frame=" + url.QueryEscape(other+"/consent-frame")})
	snap := a.mustTool(s.ID, "snapshot", nil)
	for _, want := range []string{`button "Yes, I agree" [under an iframe]`, "iframe " + other + " (not read)"} {
		if !strings.Contains(snap, want) {
			t.Errorf("missing %q in\n%s", want, snap)
		}
	}
	// Clear of the banner, on top of an iframe with a negative z-index,
	// under one that clicks go through: not marked.
	for _, clear := range []string{`button "Menu"`, `button "Watch"`, `button "Behind the ad"`} {
		if !strings.Contains(snap, clear+"\n") {
			t.Errorf("%s is marked, or missing:\n%s", clear, snap)
		}
	}
	// What the snapshot leaves unmarked, act clicks.
	for _, name := range []string{`button "Watch"`, `button "Behind the ad"`} {
		if code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, name)})); code != http.StatusOK {
			t.Errorf("%s: %d %s", name, code, msg)
		}
	}
	code, msg := a.tool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `button "Yes, I agree"`)}))
	if code != http.StatusConflict {
		t.Fatalf("covered button: %d %s", code, msg)
	}
	for _, want := range []string{"is covered by <untrusted-content-", `iframe "SP Consent Message" from ` + other, "an iframe in the way", "request_handoff", "mode visual"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if site.HasEvent("page agree") {
		t.Error("the covered button was clicked")
	}
}
