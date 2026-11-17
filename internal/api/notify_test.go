package api

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/notify"
)

// webhook gives the fixture's handoffs a notifier with a webhook set and
// every event on; base is the admin's address for links. It returns what
// was queued.
func (f *fixture) webhook(base string) func() []string {
	f.t.Helper()
	var mu sync.Mutex
	var bodies []string
	n, err := notify.New(context.Background(), f.db, slog.New(slog.DiscardHandler), notify.Options{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		},
		OnQueue: func(_ notify.Kind, b []byte) {
			mu.Lock()
			defer mu.Unlock()
			bodies = append(bodies, string(b))
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	on := map[notify.Kind]bool{}
	for _, k := range notify.Toggles {
		on[k] = true
	}
	if _, err := n.Save(context.Background(), notify.Change{URL: "https://hooks.example.com/x", On: on, LinkBase: base}); err != nil {
		f.t.Fatal(err)
	}
	f.handoffs.SetNotifier(n)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func (f *fixture) askRaw(host, sessionID string) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/sessions/"+sessionID+"/handoff", strings.NewReader(`{"reason": "sensitive_input", "require": "signin"}`))
	req.Header.Set("Authorization", "Bearer "+f.all)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestHandoffSaysNotified(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "github", "visual")
	code, out := f.askRaw("", s.ID)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if _, ok := out["notified"]; ok || strings.Contains(out["message"].(string), "webhook") {
		t.Errorf("with no webhook: %v", out)
	}
	before := out["url"].(string)
	if _, err := f.handoffs.Withdraw(context.Background(), mustGet(t, f, out["id"].(string)), ""); err != nil {
		t.Fatal(err)
	}

	queued := f.webhook("https://pier.lan")
	code, out = f.askRaw("", s.ID)
	if code != http.StatusCreated || out["notified"] != true || !strings.Contains(out["message"].(string), "the admin was also notified by webhook") {
		t.Fatalf("with a webhook: %d %v", code, out)
	}
	if u, _ := out["url"].(string); !strings.HasPrefix(u, before[:strings.Index(before, "/h/")+3]) || len(u) != len(before) {
		t.Errorf("the agent's url changed: %s, was %s", u, before)
	}
	got := queued()
	if len(got) != 1 || !strings.Contains(got[0], `"url":"https://pier.lan/h/`) {
		t.Errorf("queued %v", got)
	}
	// GET on it doesn't say notified: only the answer to the request does.
	var later map[string]any
	f.call(f.all, http.MethodGet, "/api/handoffs/"+out["id"].(string), "", &later)
	if _, ok := later["notified"]; ok {
		t.Errorf("GET says notified: %v", later)
	}
}

func mustGet(t *testing.T, f *fixture, id string) handoff.Handoff {
	t.Helper()
	h, err := f.handoffs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestTheAgentsHostNeverReachesTheWebhook: with no public URL, the agent
// API builds its own link on the request's Host, which the agent chose;
// the webhook's links never are.
func TestTheAgentsHostNeverReachesTheWebhook(t *testing.T) {
	for _, base := range []string{"", "https://pier.lan"} {
		t.Run("base="+strconv.Quote(base), func(t *testing.T) {
			f := newFixture(t)
			queued := f.webhook(base)
			s := f.start(f.all, "github", "visual")
			code, out := f.askRaw("evil.example", s.ID)
			if code != http.StatusCreated {
				t.Fatalf("create: %d %v", code, out)
			}
			if !strings.Contains(out["url"].(string), "evil.example") {
				t.Errorf("the agent's own link changed: %v", out["url"])
			}
			got := queued()
			if len(got) != 1 {
				t.Fatalf("queued %d", len(got))
			}
			if strings.Contains(got[0], "evil.example") {
				t.Errorf("the webhook body holds the agent's Host: %s", got[0])
			}
		})
	}
}

// TestNoAgentRouteTouchesTheWebhook: nothing under /api/ or in MCP can
// read or set the webhook, its secret or its events. Neither package
// imports internal/notify or names its settings keys, and no route or
// tool is about it.
func TestNoAgentRouteTouchesTheWebhook(t *testing.T) {
	f := newFixture(t)
	routes := routeRecorder{}
	f.api.Register(routes)
	for p := range routes {
		l := strings.ToLower(p)
		if strings.Contains(l, "notif") || strings.Contains(l, "webhook") || strings.Contains(l, "settings") {
			t.Errorf("agent route %s", p)
		}
	}
	for _, dir := range []string{".", "../mcp"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(src), `"notify.`) || strings.Contains(strings.ToLower(string(src)), "webhook_") {
				t.Errorf("%s names the webhook's settings", name)
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range file.Imports {
				if strings.Contains(imp.Path.Value, "internal/notify") {
					t.Errorf("%s imports %s", name, imp.Path.Value)
				}
			}
		}
	}
}
