package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
)

// callIDTools are the tools that take a call_id, with arguments.
var callIDTools = map[string]map[string]any{
	"click":      {"session_id": "s1", "x": 1, "y": 1, "screenshot": true},
	"type_text":  {"session_id": "s1", "text": "hi", "screenshot": true},
	"press_keys": {"session_id": "s1", "keys": "Return", "screenshot": true},
	"scroll":     {"session_id": "s1", "x": 1, "y": 1, "down": 1, "screenshot": true},
	"drag":       {"session_id": "s1", "from_x": 1, "from_y": 1, "to_x": 2, "to_y": 2, "screenshot": true},
	"act":        {"session_id": "s1", "steps": []map[string]any{{"action": "click", "ref": "a1"}}},
	"navigate":   {"session_id": "s1", "url": "https://example.com/"},
	"tabs":       {"session_id": "s1", "action": "new"},
}

// recordingAPI stands in for the agent API: it records the call id each
// request carried, and answers an action as done, or, once replay is
// set, as a repeat of a call that ended replay.
type recordingAPI struct {
	mu     sync.Mutex
	ids    []string // "POST path id" for each request with a call id, "path" for one without
	replay string
}

func (r *recordingAPI) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	id := req.Header.Get(api.IdempotencyHeader)
	r.ids = append(r.ids, strings.TrimSpace(req.Method+" "+req.URL.Path+" "+id))
	replay := r.replay
	r.mu.Unlock()
	if strings.HasSuffix(req.URL.Path, "/screenshot") || replay == "" && strings.HasSuffix(req.URL.Path, "/computer") {
		var b bytes.Buffer
		_ = png.Encode(&b, image.NewGray(image.Rect(0, 0, 16, 10)))
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Screen-Width", "1440")
		w.Header().Set("X-Screen-Height", "900")
		_, _ = w.Write(b.Bytes())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if replay == "" {
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "Clicked a1"})
		return
	}
	w.Header().Set(api.ReplayedHeader, "true")
	out := map[string]any{"call": map[string]string{"call_id": id, "tool": "act", "state": replay}}
	if replay == "ok" {
		out["text"] = "This act call " + id + " ran already and ended ok; it wasn't run again."
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	out["error"] = "This act call " + id + " is " + replay + "; it wasn't run again."
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(out)
}

// connectOver serves the MCP endpoint over h, standing in for the agent
// API, and connects a client.
func connectOver(t *testing.T, h http.Handler) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	tokens := auth.NewTokens(d)
	_, token, err := tokens.Create(ctx, "claude", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(h, auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"}), log, "test")
	mux := http.NewServeMux()
	srv.Register(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: hs.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call_id is on the tools that change the page, and only on them.
func TestCallIDIsOnTheActionsOnly(t *testing.T) {
	cs := connectOver(t, &recordingAPI{})
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tool := range list.Tools {
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(raw, &schema)
		p, has := schema.Properties["call_id"]
		_, want := callIDTools[tool.Name]
		switch {
		case tool.Name == "call_status":
			continue // asks about one
		case has != want:
			t.Errorf("%s: call_id %t, want %t", tool.Name, has, want)
		case has && !strings.Contains(p.Description, "must not happen twice"):
			t.Errorf("%s: call_id says %q", tool.Name, p.Description)
		}
		if want {
			seen++
		}
	}
	if seen != len(callIDTools) {
		t.Errorf("%d of the %d tools listed", seen, len(callIDTools))
	}
}

// Each tool passes its call_id on as the header, on the action's request
// only, and turns a repeat's answer into its result: an error unless the
// first call ended ok, and never an image.
func TestToolsPassTheirCallIDOn(t *testing.T) {
	rec := &recordingAPI{}
	cs := connectOver(t, rec)
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range callIDTools {
			if !yield(n) {
				return
			}
		}
	})
	for _, name := range names {
		args := map[string]any{}
		for k, v := range callIDTools[name] {
			args[k] = v
		}
		args["call_id"] = "id-" + name
		rec.mu.Lock()
		rec.ids, rec.replay = nil, ""
		rec.mu.Unlock()
		if res, out := call(t, cs, name, args); res.IsError {
			t.Fatalf("%s: %s", name, out)
		}
		rec.mu.Lock()
		ids := slices.Clone(rec.ids)
		rec.mu.Unlock()
		var withID []string
		for _, r := range ids {
			if strings.HasSuffix(r, " id-"+name) {
				withID = append(withID, r)
			}
		}
		if len(withID) != 1 || !strings.HasPrefix(withID[0], "POST ") {
			t.Fatalf("%s: the call id went with %q", name, ids)
		}
		for _, state := range []string{"ok", "waiting", "running", "error", "interrupted"} {
			rec.mu.Lock()
			rec.replay = state
			rec.mu.Unlock()
			res, out := call(t, cs, name, args)
			if res.IsError != (state != "ok") || !strings.Contains(out, "wasn't run again") {
				t.Errorf("%s repeated, first %s: error %t, %q", name, state, res.IsError, out)
			}
			for _, c := range res.Content {
				if _, ok := c.(*sdk.ImageContent); ok {
					t.Errorf("%s repeated, first %s: an image", name, state)
				}
			}
			if strings.Contains(out, "Clicked a1") {
				t.Errorf("%s repeated: the first result came again: %q", name, out)
			}
		}
	}
	// A tool without one sends none.
	rec.mu.Lock()
	rec.ids, rec.replay = nil, ""
	rec.mu.Unlock()
	call(t, cs, "snapshot", map[string]any{"session_id": "s1"})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.ids) != 1 || rec.ids[0] != "POST /api/sessions/s1/snapshot" {
		t.Errorf("snapshot sent %q", rec.ids)
	}
}

// Over the real API: a repeated click runs once, and call_status and
// session_status say what happened, to the token that did it.
func TestARepeatedClickRunsOnceOverMCP(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"session_id": s.ID, "x": 576, "y": 360, "call_id": "pay-1", "screenshot": true}
	res, out := call(t, cs, "click", args)
	if res.IsError || !strings.Contains(out, "Clicked") || len(res.Content) != 2 {
		t.Fatalf("click: %s", out)
	}
	res, out = call(t, cs, "click", args)
	if res.IsError || !strings.Contains(out, "ran already") || len(res.Content) != 1 {
		t.Fatalf("click again: %s", out)
	}
	f.mu.Lock()
	clicks := len(f.clicks)
	f.mu.Unlock()
	if clicks != 1 {
		t.Fatalf("the browser clicked %d times", clicks)
	}
	// An empty call_id isn't taken as none: the agent meant to be covered.
	for tool, args := range map[string]map[string]any{
		"click": {"session_id": s.ID, "x": 1, "y": 1, "call_id": ""},
		"act":   {"session_id": s.ID, "steps": []map[string]any{{"action": "click", "ref": "a1"}}, "call_id": ""},
	} {
		res, out := call(t, cs, tool, args)
		if !res.IsError || !strings.Contains(out, "call_id") {
			t.Fatalf("%s with an empty call_id: %s", tool, out)
		}
	}
	f.mu.Lock()
	clicks = len(f.clicks)
	f.mu.Unlock()
	if clicks != 1 {
		t.Fatalf("an empty call_id clicked: %d clicks", clicks)
	}
	if res, out := call(t, cs, "call_status", map[string]any{"session_id": s.ID, "call_id": "pay-1"}); res.IsError || !strings.Contains(out, "Call pay-1 (computer): ok.") {
		t.Fatalf("call_status: %s", out)
	}
	if res, out := call(t, cs, "call_status", map[string]any{"session_id": s.ID, "call_id": "never"}); !res.IsError || !strings.Contains(out, "no call with this id") {
		t.Fatalf("call_status of an unknown id: %s", out)
	}
	res, out = call(t, cs, "session_status", map[string]any{"session_id": s.ID})
	if res.IsError || !strings.Contains(out, "running, visual mode") || !strings.Contains(out, "None of your calls is under way") ||
		!strings.Contains(out, "Clicked at 720, 450 (call_id pay-1: ok)") {
		t.Fatalf("session_status: %s", out)
	}

	// Another token sees neither the call nor another profile's session.
	other := f.connect(f.scoped, versions[0], nil)
	if res, out := call(t, other, "call_status", map[string]any{"session_id": s.ID, "call_id": "pay-1"}); !res.IsError {
		t.Fatalf("another token's call_status: %s", out)
	}
	if _, out := call(t, other, "session_status", map[string]any{"session_id": s.ID}); strings.Contains(out, "pay-1") || strings.Contains(out, "Clicked") {
		t.Fatalf("another token's session_status: %s", out)
	}
	g, _, err := f.m.Start(context.Background(), "github", "")
	if err != nil {
		t.Fatal(err)
	}
	for tool, args := range map[string]map[string]any{"session_status": {"session_id": g.ID}, "call_status": {"session_id": g.ID, "call_id": "pay-1"}} {
		if res, out := call(t, other, tool, args); !res.IsError || !strings.Contains(out, "no such session") {
			t.Fatalf("%s of another profile's session: %s", tool, out)
		}
	}
}
