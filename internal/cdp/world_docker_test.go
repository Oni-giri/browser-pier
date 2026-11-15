//go:build docker

package cdp_test

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/cdp"
)

// TestAnOldWorldNumberThatNamesAPageWorldNeverRunsCode is the PROJ-21 review's case on a real
// browser: an isolated world is made on one site, then the tab navigates
// cross-site (a new renderer, whose context numbers start again) to a page
// with eight same-site iframes, so the old world's number can name an
// iframe's main world. Nothing must run there: not code by that number,
// not Eval, not ResolveNode, not a handle the bootstrap read by it.
func TestAnOldWorldNumberThatNamesAPageWorldNeverRunsCode(t *testing.T) {
	s := startBrowser(t)
	// The same server under another site: its address instead of its name,
	// or, when the site is already an address, another of this machine's.
	host := browsertest.SiteHost(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.site, "http://"))
	ip := browsertest.OtherAddress(t, "browserpier-"+s.name, host, port)
	other := strings.Replace(s.site, host, ip, 1)
	if other == s.site {
		t.Fatalf("no second site from %s", s.site)
	}
	named := 0
	for trial := range 3 {
		c := s.dial()
		tb := s.firstTab(c)
		tb.navigate(s.site + "/blank")
		old := tb.world()
		tb.navigate(other + "/frames")
		time.Sleep(500 * time.Millisecond) // the iframes load
		if _, err := tb.IsolatedWorld(ctx(5), func() string { f, _, _ := tb.frame(); return f }()); err != nil {
			t.Fatalf("trial %d: a world on the new page: %v", trial, err)
		}
		// Code by number is refused before it is sent.
		if err := tb.Call(ctx(5), "Runtime.evaluate", map[string]any{"expression": "typeof marker", "contextId": old}, nil); !errors.Is(err, cdp.ErrNotAllowed) {
			t.Errorf("trial %d: code by the old number: %v", trial, err)
		}
		// Eval goes through the old world's handle, which is gone.
		var v any
		if err := tb.Eval(ctx(5), old, "typeof marker", &v); !errors.Is(err, cdp.ErrWorldGone) {
			t.Errorf("trial %d: Eval in the old world gave %v, %v", trial, v, err)
		}
		if _, err := tb.ResolveNode(ctx(5), old, 1); err == nil {
			t.Errorf("trial %d: ResolveNode in the old world worked", trial)
		}
		// The bootstrap read is allowed by number (it runs nothing), but
		// what it gives isn't trusted with code.
		var g struct{ Result cdp.RemoteObject }
		if tb.Call(ctx(5), "Runtime.evaluate", map[string]any{"expression": "this", "contextId": old}, &g) == nil {
			named++
			if err := tb.Call(ctx(5), "Runtime.callFunctionOn", map[string]any{"objectId": g.Result.ObjectID,
				"functionDeclaration": "function() { return typeof marker; }", "returnByValue": true}, nil); !errors.Is(err, cdp.ErrNotAllowed) {
				t.Errorf("trial %d: code on a global read by the old number: %v", trial, err)
			}
		}
		// The page tools' way still works on the new page.
		var title string
		if err := tb.Eval(ctx(5), tb.world(), "document.title + ' ' + typeof marker", &title); err != nil || title != "Frames undefined" {
			t.Errorf("trial %d: Eval in the new page's isolated world: %q %v", trial, title, err)
		}
		_ = c.Close()
	}
	t.Logf("the old world's number named another context in %d of 3 trials", named)
}
