package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestNoToolTouchesTheStickyToken: no MCP tool is about a profile's exit
// or its sticky token, and none of their answers holds the token.
func TestNoToolTouchesTheStickyToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, _ := f.m.Profiles().Get(ctx, "google")
	p.Proxy = "http://pier-{sticky}:pw@proxy.example:8080"
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	tok, err := f.m.Profiles().StickyToken(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	cs := f.connect(f.all, versions[0], nil)
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		if l := strings.ToLower(tl.Name + " " + tl.Description); strings.Contains(l, "sticky") || strings.Contains(l, "exit ip") || strings.Contains(l, "new exit") {
			t.Errorf("tool %s: %s", tl.Name, tl.Description)
		}
	}
	var replies []string
	_, out := call(t, cs, "list_profiles", nil)
	replies = append(replies, out)
	_, out = call(t, cs, "start_browser", map[string]any{"profile": "google"})
	replies = append(replies, out)
	s, _ := f.m.Live("google")
	_, out = call(t, cs, "stop_browser", map[string]any{"session_id": s.ID})
	replies = append(replies, out)
	for _, r := range replies {
		if strings.Contains(r, tok) {
			t.Errorf("a reply holds the token: %s", r)
		}
	}
}
