package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

const mcpSecret = `Vault Value+"&é🙂42`

// toolArgs are arguments each tool accepts, to call every tool in
// TestEveryToolIsScrubbed. A new tool must be added here.
var toolArgs = map[string]map[string]any{
	"list_profiles":    {},
	"start_browser":    {"profile": "google"},
	"stop_browser":     {"session_id": "s1"},
	"screenshot":       {"session_id": "s1"},
	"click":            {"session_id": "s1", "x": 1, "y": 1},
	"type_text":        {"session_id": "s1", "text": "hi"},
	"press_keys":       {"session_id": "s1", "keys": "Return"},
	"scroll":           {"session_id": "s1", "x": 1, "y": 1, "down": 1},
	"drag":             {"session_id": "s1", "from_x": 1, "from_y": 1, "to_x": 2, "to_y": 2},
	"wait":             {"session_id": "s1", "seconds": 0},
	"zoom":             {"session_id": "s1", "x": 0, "y": 0, "width": 10, "height": 10},
	"list_files":       {"profile": "google"},
	"upload_file":      {"session_id": "s1", "name": "a.txt", "content_base64": "aGk="},
	"choose_files":     {"session_id": "s1", "id": "c1", "files": []string{"a.txt"}},
	"snapshot":         {"session_id": "s1"},
	"find":             {"session_id": "s1", "text": "x"},
	"navigate":         {"session_id": "s1", "url": "https://example.com/"},
	"act":              {"session_id": "s1", "steps": []map[string]any{{"action": "click", "ref": "a1"}}},
	"wait_for":         {"session_id": "s1", "text": "x", "timeout_ms": 100},
	"tabs":             {"session_id": "s1"},
	"page_content":     {"session_id": "s1", "format": "markdown"},
	"request_handoff":  {"session_id": "s1", "reason": "captcha", "detail": "Solve it"},
	"wait_for_handoff": {"handoff_id": "h1", "seconds": 1},
	"cancel_handoff":   {"handoff_id": "h1"},
}

// leakyAPI stands in for the agent API with no scrub of its own: every
// answer quotes the secret, in an error or in each field a tool reads.
type leakyAPI struct {
	mu    sync.Mutex
	calls int
	ok    bool // answer 200 with the secret in every field, not an error
}

func (l *leakyAPI) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	l.mu.Lock()
	l.calls++
	ok := l.ok
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Downloads", `{"files":[{"name":"`+strings.ReplaceAll(mcpSecret, `"`, `\"`)+`.csv","size":3,"state":"downloaded"}]}`)
	if !ok {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "the page says " + mcpSecret})
		return
	}
	s := mcpSecret
	_ = json.NewEncoder(w).Encode(map[string]any{
		"text": s, "error": s, "name": s, "id": s, "profile": s, "mode": "api", "state": s, "status": s,
		"cdp_url": s, "live_view_url": s, "url": s, "path": s, "note": s, "message": s, "origin": s, "reason": s,
		"in_use_by": []string{s}, "profiles": []map[string]any{{"name": s, "status": s, "default_mode": s, "session_id": s}},
		"files": []map[string]any{{"name": s, "size": 3}},
	})
}

func (l *leakyAPI) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// leakyServer serves an MCP endpoint over a leakyAPI, with the scrub on or
// off, and connects a client.
func leakyServer(t *testing.T, scrubbing bool) (*sdk.ClientSession, *leakyAPI) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
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
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	api := &leakyAPI{}
	srv := New(api, web, log, "test")
	if scrubbing {
		sc := scrub.New([]string{mcpSecret})
		srv.SetSecrets(func(allow func(string) bool) *scrub.Scrubber {
			if !allow("google") {
				return nil
			}
			return sc
		})
	}
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
	return cs, api
}

// resultText is every piece of text in a result, structured content included.
func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		raw, _ := json.Marshal(c)
		var v struct{ Text, URI, Name string }
		_ = json.Unmarshal(raw, &v)
		b.WriteString(v.Text + " " + v.URI + " " + v.Name + "\n")
		if r, ok := c.(*sdk.EmbeddedResource); ok && r.Resource != nil {
			b.WriteString(r.Resource.Text + "\n")
		}
	}
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		b.Write(raw)
	}
	return b.String()
}

// leaks calls every tool the server lists, once with errors and once with
// answers, and returns the tools whose results held the secret (or no
// <hidden> where the API's error must have said it).
func leaks(t *testing.T, scrubbing bool) (leaked []string) {
	t.Helper()
	cs, api := leakyServer(t, scrubbing)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if toolArgs[tool.Name] == nil {
			t.Errorf("tool %s has no arguments in toolArgs: add them, so its results are checked", tool.Name)
		}
	}
	for name := range toolArgs {
		if !slices.Contains(names, name) {
			t.Errorf("toolArgs has %s, which isn't a tool any more", name)
		}
	}
	slices.Sort(names)
	for _, ok := range []bool{false, true} {
		api.mu.Lock()
		api.ok = ok
		api.mu.Unlock()
		for _, name := range names {
			if toolArgs[name] == nil {
				continue
			}
			before := api.count()
			res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: toolArgs[name]})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out := resultText(res)
			if !ok && api.count() == before {
				t.Errorf("%s didn't call the API: %s", name, out)
				continue
			}
			bad := strings.Contains(out, "Vault Value") || strings.Contains(out, `\u00e9`)
			if !ok && !strings.Contains(out, scrub.Hidden) {
				bad = true // the API's error said it: it must show hidden
			}
			if bad {
				leaked = append(leaked, name)
				t.Logf("%s (answers %t): %s", name, ok, out)
			}
		}
	}
	return leaked
}

// TestEveryToolIsScrubbed (PROJ-125) calls every tool the MCP server lists
// over an API that leaks the secret in every answer, and checks no result
// holds it: the scrub is the server's middleware, which no tool can skip.
func TestEveryToolIsScrubbed(t *testing.T) {
	if got := leaks(t, true); len(got) > 0 {
		t.Errorf("these tools' results hold the secret: %v", got)
	}
}

// TestUnscrubbedToolsAreCaught is the test of the test: without the
// scrub, every tool is caught leaking.
func TestUnscrubbedToolsAreCaught(t *testing.T) {
	got := leaks(t, false)
	slices.Sort(got)
	got = slices.Compact(got)
	if len(got) != len(toolArgs) {
		var missed []string
		for name := range toolArgs {
			if !slices.Contains(got, name) {
				missed = append(missed, name)
			}
		}
		t.Errorf("without the scrub, these tools weren't caught leaking: %v", missed)
	}
}
