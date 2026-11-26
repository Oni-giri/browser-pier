package mcp

import (
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/notify"
)

// TestHandoffSaysTheAdminWasNotified: a headless client gets the link,
// and is told the admin's webhook was told too, so it waits.
func TestHandoffSaysTheAdminWasNotified(t *testing.T) {
	for _, set := range []bool{false, true} {
		f := newFixture(t)
		if set {
			d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			n, err := notify.New(context.Background(), d, slog.New(slog.DiscardHandler), notify.Options{
				Lookup: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := n.Save(context.Background(), notify.Change{URL: "https://hooks.example.com/x", On: map[notify.Kind]bool{notify.HandoffRequested: true}}); err != nil {
				t.Fatal(err)
			}
			f.handoffs.SetNotifier(n)
		}
		cs := f.connect(f.all, versions[0], nil)
		s, _, _ := f.m.Start(context.Background(), "google", "")
		_, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "captcha", "detail": "Solve the CAPTCHA"})
		if !strings.Contains(out, "/h/") || strings.Contains(out, "notified by webhook") != set {
			t.Errorf("webhook set %t: %s", set, out)
		}
	}
}

// TestNoToolIsAboutTheWebhook: no MCP tool reads or sets it.
func TestNoToolIsAboutTheWebhook(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) < 10 {
		t.Fatalf("only %d tools", len(list.Tools))
	}
	for _, tool := range list.Tools {
		l := strings.ToLower(tool.Name + " " + tool.Description)
		if strings.Contains(l, "webhook") || strings.Contains(l, "notif") || strings.Contains(l, "setting") {
			t.Errorf("tool %s: %s", tool.Name, tool.Description)
		}
	}
}
