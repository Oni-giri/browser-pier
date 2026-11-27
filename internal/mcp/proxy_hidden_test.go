package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

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

	tok, err := f.m.Profiles().StickyToken(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	secret := []string{"hidden-proxy.example", "31877", "acct-9f3k2", "pw-7q2x", "sticky", tok}
	// A browser's last log line quoting its own proxy, as an older
	// image's dial error did.
	f.rt.LastLog = `level=ERROR msg=exiting err="dial tcp: lookup hidden-proxy.example: no such host; ` +
		`http://acct-9f3k2-` + tok + `:pw-7q2x@hidden-proxy.example:31877"`
	clean := func(tool string, res *sdk.CallToolResult, out string) {
		t.Helper()
		all := out
		if res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			all += string(b)
		}
		for _, part := range secret {
			if strings.Contains(all, part) {
				t.Errorf("%s holds %q: %s", tool, part, all)
			}
		}
	}
	res, out := call(t, cs, "start_browser", map[string]any{"profile": "google"})
	clean("start_browser", res, out)
	s, ok := f.m.Live("google")
	if !ok || res.IsError {
		t.Fatalf("start_browser: %s", out)
	}
	_, out = call(t, cs, "list_profiles", nil)
	check(out)
	if !strings.Contains(out, "session_id") {
		t.Errorf("google isn't running: %s", out)
	}
	res, out = call(t, cs, "session_status", map[string]any{"session_id": s.ID})
	clean("session_status", res, out)
	res, out = call(t, cs, "stop_browser", map[string]any{"session_id": s.ID})
	clean("stop_browser", res, out)
	res, out = call(t, cs, "session_status", map[string]any{"session_id": s.ID})
	clean("session_status after the stop", res, out)

	// A start that fails says why, without the proxy.
	status := 1
	f.rt.ExitOnStart = &status
	res, out = call(t, cs, "start_browser", map[string]any{"profile": "google"})
	if !res.IsError || !strings.Contains(out, "no such host") {
		t.Fatalf("a failed start_browser: %s", out)
	}
	clean("a failed start_browser", res, out)
}
