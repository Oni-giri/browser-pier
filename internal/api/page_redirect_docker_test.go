//go:build docker

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestNavigateFollowsRedirectChains is PROJ-121 on a real browser:
// navigate waits out a chain of script redirects (a sign-in bounce) and
// answers with where it landed, stops at 15 s on a page that never stops
// redirecting, doesn't wait for a fragment, and names the address that
// failed after a redirect.
func TestNavigateFollowsRedirectChains(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "chain")
	nav := func(path string) (string, time.Duration) {
		t.Helper()
		start := time.Now()
		out := a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + path})
		return out, time.Since(start)
	}
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/login"}) // a warm browser

	// Two script hops: the answer is the last page, with PROJ-110's note.
	out, took := nav("/hop1")
	t.Logf("two hops: %v", took)
	if took > 3*time.Second {
		t.Errorf("the chain took %v", took)
	}
	if !strings.Contains(out, "URL: "+site.URL+"/landed\n") || !strings.Contains(out, "Title: Landed\n") || strings.Contains(out, "still") {
		t.Errorf("not the chain's last page:\n%s", out)
	}
	if i := strings.Index(out, "Redirected from: "+site.URL+"/hop1\n"); i < 0 || i > strings.LastIndex(out, "</untrusted-content-") {
		t.Errorf("no redirect note in the fence:\n%s", out)
	}
	// A snapshot right after reads the landed page, not a stale one.
	if snap := a.mustTool(s.ID, "snapshot", nil); pageOf(t, snap).URL != site.URL+"/landed" || !strings.Contains(snap, "LANDED-PAGE") {
		t.Errorf("snapshot after the chain:\n%s", snap)
	}

	// A fragment of the current page: no wait, no note.
	nav("/sections")
	out, took = nav("/sections#section")
	if took > 500*time.Millisecond {
		t.Errorf("a fragment took %v", took)
	}
	if !strings.Contains(out, "URL: "+site.URL+"/sections#section\n") || strings.Contains(out, "Redirected") || strings.Contains(out, "still") {
		t.Errorf("a fragment:\n%s", out)
	}

	// An address that can't be reached, after a server or a script
	// redirect: navigate says which.
	for _, path := range []string{"/redirect-fail", "/hop-fail"} {
		code, msg := a.tool(s.ID, "navigate", map[string]string{"url": site.URL + path})
		t.Logf("%s: %d %s", path, code, msg)
		if code != http.StatusBadGateway || !strings.Contains(msg, "after a redirect") || !strings.Contains(msg, "Failed at: http://169.254.169.254/sso") {
			t.Errorf("%s: %d %s", path, code, msg)
		}
		if i := strings.Index(msg, "Failed at"); i > strings.LastIndex(msg, "</untrusted-content-") {
			t.Errorf("%s: the address is outside the fence: %s", path, msg)
		}
	}

	// A page that never stops: 15 s, then says so; the session still works.
	out, took = nav("/loop")
	if took < 15*time.Second || took > 22*time.Second {
		t.Errorf("the loop took %v", took)
	}
	if !strings.Contains(out, "still redirecting after 15 s") || !strings.Contains(out, "URL: "+site.URL+"/loop?n=") {
		t.Errorf("the loop:\n%s", out)
	}
	if out, _ := nav("/landed"); !strings.Contains(out, "Title: Landed") || strings.Contains(out, "still") {
		t.Errorf("after the loop:\n%s", out)
	}
}
