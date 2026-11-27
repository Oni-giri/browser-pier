package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// TestListProfilesSaysOnlyWhetherProxied (PROJ-205): list_profiles says
// whether each profile goes through a proxy, as the REST API's "proxied"
// does, and holds no part of the proxy.
func TestListProfilesSaysOnlyWhetherProxied(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, proxy := range map[string]string{
		"google": "http://acct-9f3k2-{sticky}:pw-7q2x@hidden-proxy.example:31877",
		"github": "socks5://acct-9f3k2:pw-7q2x@hidden-proxy.example:31877",
	} {
		p, err := f.m.Profiles().Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		p.Proxy = proxy
		if _, err := f.m.Profiles().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.m.Profiles().Create(ctx, profiles.New("direct")); err != nil {
		t.Fatal(err)
	}
	cs := f.connect(f.all, versions[0], nil)
	check := func(out string) {
		t.Helper()
		for _, part := range []string{"hidden-proxy.example", "31877", "acct-9f3k2", "pw-7q2x", "sticky"} {
			if strings.Contains(out, part) {
				t.Errorf("list_profiles holds %q: %s", part, out)
			}
		}
		lines := map[string]string{}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			name, _, _ := strings.Cut(l, ":")
			lines[name] = l
		}
		for name, proxied := range map[string]bool{"google": true, "github": true, "direct": false} {
			l := lines[name]
			if proxied && !strings.Contains(l, ", through a proxy") || !proxied && !strings.Contains(l, ", no proxy") {
				t.Errorf("%s: %q, want proxied %v", name, l, proxied)
			}
		}
	}
	_, out := call(t, cs, "list_profiles", nil)
	check(out)
	_, _ = call(t, cs, "start_browser", map[string]any{"profile": "google"})
	_, out = call(t, cs, "list_profiles", nil)
	check(out)
	if !strings.Contains(out, "session_id") {
		t.Errorf("google isn't running: %s", out)
	}
}
