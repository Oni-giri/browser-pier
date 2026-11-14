//go:build docker

package cdp_test

import (
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	host := siteHost(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.site, "http://"))
	ip := otherAddress(t, s.name, host, port)
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

// otherAddress is another name for the test site's host, as a different
// site: what host resolves to in the browser's container when it's a name
// (host.docker.internal under rootless Podman), or else, since an address
// has no name to resolve (getent would fail on Docker's bridge gateway,
// CI's default), another IPv4 address of this machine that the container
// reaches the site at (the test site listens on every interface). Not
// every one does: rootless Podman's network namespace holds a copy of the
// host's LAN address, so the container can't reach the host there.
func otherAddress(t *testing.T, container, host, port string) string {
	t.Helper()
	if net.ParseIP(host) == nil {
		out, err := exec.Command("docker", "exec", "browserpier-"+container, "getent", "hosts", host).Output()
		if err != nil || len(strings.Fields(string(out))) == 0 {
			t.Fatalf("can't resolve %s in the container: %v %q", host, err, out)
		}
		return strings.Fields(string(out))[0]
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.To4() == nil || !n.IP.IsGlobalUnicast() || n.IP.Equal(net.ParseIP(host)) || n.Contains(net.ParseIP(host)) {
			continue
		}
		probe := exec.Command("docker", "exec", "browserpier-"+container, "timeout", "3", "bash", "-c", "exec 3<>/dev/tcp/"+n.IP.String()+"/"+port)
		if probe.Run() != nil {
			t.Logf("the container can't reach %s; trying another address", n.IP)
			continue
		}
		return n.IP.String()
	}
	t.Fatalf("no address of this machine but %s that the browser's container reaches; set BROWSERPIER_TEST_SITE_HOST to a name", host)
	return ""
}
