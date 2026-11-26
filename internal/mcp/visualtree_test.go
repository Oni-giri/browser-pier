package mcp

import (
	"context"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestVisualSnapshotAndClickByRef (PROJ-150): visual_snapshot is listed
// and says what it returns is untrusted; its result is fenced; click
// takes its ref, clicks the element's centre and says what it clicked.
func TestVisualSnapshotAndClickByRef(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var desc string
	for _, tl := range list.Tools {
		if tl.Name == "visual_snapshot" {
			desc = tl.Description
		}
	}
	if !strings.Contains(desc, "untrusted") || !strings.HasPrefix(desc, "Visual mode only.") {
		t.Fatalf("visual_snapshot's description: %q", desc)
	}
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	res, out := call(t, cs, "visual_snapshot", map[string]any{"session_id": s.ID})
	if res.IsError || !strings.Contains(out, "<untrusted-content-") || !strings.Contains(out, "untrusted_content-1") || strings.Count(out, "</untrusted-content-") != 2 {
		t.Fatalf("visual_snapshot: %s", out)
	}
	// The button's centre is (120, 210) on the 1440×900 screen: (96, 168)
	// in the 1152×720 screenshot every MCP tool's coordinates are in.
	if !strings.Contains(out, `button "Go`) || !strings.Contains(out, "@96,168") || strings.Contains(out, "@120,210") {
		t.Fatalf("visual_snapshot's point isn't in screenshot pixels: %s", out)
	}
	ref := regexp.MustCompile(`(?m)^(v[a-z][a-z0-9]n[0-9]+) button`).FindStringSubmatch(out)
	if ref == nil {
		t.Fatalf("no ref in %s", out)
	}
	res, out = call(t, cs, "click", map[string]any{"session_id": s.ID, "ref": ref[1]})
	if res.IsError || !strings.Contains(out, "Clicked "+ref[1]) || !strings.Contains(out, `button "Go"`) || !strings.Contains(out, "<untrusted-content-") {
		t.Fatalf("click by ref: %s", out)
	}
	f.mu.Lock()
	clicks := append([]string(nil), f.clicks...)
	f.mu.Unlock()
	if len(clicks) != 1 || !strings.Contains(clicks[0], `"x":120`) || !strings.Contains(clicks[0], `"y":210`) || strings.Contains(clicks[0], "ref") {
		t.Fatalf("the browser was asked to click %v", clicks)
	}
	// With a call_id (PROJ-156): it runs once, and a repeat says so.
	byID := map[string]any{"session_id": s.ID, "ref": ref[1], "call_id": "go-1"}
	if res, out := call(t, cs, "click", byID); res.IsError || !strings.Contains(out, "Clicked "+ref[1]) {
		t.Fatalf("click by ref with a call_id: %s", out)
	}
	if res, out := call(t, cs, "click", byID); res.IsError || !strings.Contains(out, "ran already") {
		t.Fatalf("click by ref with a call_id, again: %s", out)
	}
	f.mu.Lock()
	n := len(f.clicks)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("the browser clicked %d times, want 2", n)
	}
	if res, out := call(t, cs, "call_status", map[string]any{"session_id": s.ID, "call_id": "go-1"}); res.IsError || !strings.Contains(out, "Call go-1 (computer): ok.") {
		t.Fatalf("call_status: %s", out)
	}
	for _, args := range []map[string]any{{"session_id": s.ID, "ref": ref[1], "x": 1, "y": 1}, {"session_id": s.ID}} {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "click", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("click %v: want an error", args)
		}
	}
}
