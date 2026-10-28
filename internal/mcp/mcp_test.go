package mcp

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

type fixture struct {
	t        *testing.T
	m        *sessions.Manager
	tokens   *auth.Tokens
	handoffs *handoff.Service
	srv      *httptest.Server
	all      string // a token for every profile
	scoped   string // a token for google only

	mu     sync.Mutex
	clicks []string // what the fake browser was asked to click
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	f := &fixture{t: t, tokens: auth.NewTokens(d)}
	rt := sessionstest.New(dir)
	rt.Control = f.browser()
	f.m, err = sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.m.Close)
	for _, n := range []string{"google", "github"} {
		if _, err := f.m.Profiles().Create(ctx, profiles.New(n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, f.all, err = f.tokens.Create(ctx, "claude", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, f.scoped, err = f.tokens.Create(ctx, "helper", false, []string{"google"}); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.SetPIN(ctx, "4821"); err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, f.tokens, log, auth.WebOptions{CookieSecure: "auto"})
	a := api.New(f.m, web, files.New(f.m.Layout().FilesDir, 1<<20, os.Getuid(), os.Getgid()), log, "")
	f.handoffs = handoff.New(d, f.m, log)
	a.SetHandoffs(f.handoffs)
	routes := http.NewServeMux()
	a.Register(routes)
	mux := http.NewServeMux()
	a.Register(mux)
	New(routes, web, log, "test").Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// browser is a fake browser's control socket: a 1440×900 screen.
func (f *fixture) browser() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /cdp/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"type":"page","url":"https://accounts.example.com/signin?x=1"}]`)
	})
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		width, height := 1440, 900
		if n, err := strconv.Atoi(r.URL.Query().Get("width")); err == nil && n < width {
			width, height = n, 900*n/1440
		}
		var b bytes.Buffer
		_ = png.Encode(&b, image.NewGray(image.Rect(0, 0, width, height)))
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Screen-Width", "1440")
		w.Header().Set("X-Screen-Height", "900")
		_, _ = w.Write(b.Bytes())
	})
	mux.HandleFunc("POST /input/click", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.clicks = append(f.clicks, string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// bearer adds a token to every request.
type bearer struct {
	token string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// connect opens an MCP client session with a token, on a protocol
// version, optionally able to open URLs for its user.
func (f *fixture) connect(token, version string, elicit func(*sdk.ElicitRequest) (*sdk.ElicitResult, error)) *sdk.ClientSession {
	f.t.Helper()
	opts := &sdk.ClientOptions{}
	if elicit != nil {
		opts.Capabilities = &sdk.ClientCapabilities{Elicitation: &sdk.ElicitationCapabilities{URL: &sdk.URLElicitationCapabilities{}}}
		opts.ElicitationHandler = func(_ context.Context, r *sdk.ElicitRequest) (*sdk.ElicitResult, error) { return elicit(r) }
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "1"}, opts)
	tr := &sdk.StreamableClientTransport{Endpoint: f.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}
	cs, err := c.Connect(context.Background(), tr, &sdk.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		f.t.Fatalf("connect (%s): %v", version, err)
	}
	f.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) (*sdk.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return res, b.String()
}

var versions = []string{"2025-11-25", "2026-07-28"}

func TestStartScreenshotStop(t *testing.T) {
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			f := newFixture(t)
			cs := f.connect(f.all, v, nil)
			_, out := call(t, cs, "list_profiles", nil)
			if !strings.Contains(out, "google: stopped") || !strings.Contains(out, "github") {
				t.Fatalf("list_profiles: %s", out)
			}
			_, out = call(t, cs, "start_browser", map[string]any{"profile": "google"})
			s, ok := f.m.Live("google")
			if !ok || !strings.Contains(out, "session_id: "+s.ID) || !strings.Contains(out, "visual mode") {
				t.Fatalf("start_browser: %s", out)
			}
			res, out := call(t, cs, "screenshot", map[string]any{"session_id": s.ID})
			if !strings.Contains(out, "1152×720") || len(res.Content) != 2 {
				t.Fatalf("screenshot: %s", out)
			}
			img, ok := res.Content[1].(*sdk.ImageContent)
			if !ok {
				t.Fatalf("no image: %T", res.Content[1])
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(img.Data))
			if err != nil || cfg.Width != 1152 || cfg.Height != 720 {
				t.Fatalf("image %dx%d, %v", cfg.Width, cfg.Height, err)
			}
			_, out = call(t, cs, "stop_browser", map[string]any{"session_id": s.ID})
			if _, live := f.m.Live("google"); live || !strings.Contains(out, "Stopped") {
				t.Fatalf("stop_browser: %s", out)
			}
		})
	}
}

func TestScopedTokenSeesOnlyItsProfiles(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.scoped, versions[0], nil)
	_, out := call(t, cs, "list_profiles", nil)
	if strings.Contains(out, "github") {
		t.Fatalf("scoped list_profiles shows github: %s", out)
	}
	res, out := call(t, cs, "start_browser", map[string]any{"profile": "github"})
	if !res.IsError {
		t.Fatalf("scoped token started github: %s", out)
	}
}

func TestSessionsDontCrossTokens(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	// Another token presenting this MCP session's ID is refused.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.scoped)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", cs.ID())
	req.Header.Set("Mcp-Protocol-Version", versions[0])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("another token used this MCP session")
	}
	// No token at all.
	resp, err = http.Post(f.srv.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestHandoffThroughURLElicitation(t *testing.T) {
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			f := newFixture(t)
			var shown *sdk.ElicitParams
			cs := f.connect(f.all, v, func(r *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				shown = r.Params
				// The person opens the link, does it and hands back.
				go func() {
					time.Sleep(200 * time.Millisecond)
					h, err := f.handoffs.Get(context.Background(), r.Params.ElicitationID)
					if err == nil {
						_, _ = f.handoffs.Finish(context.Background(), h, handoff.Done, "used a backup code")
					}
				}()
				return &sdk.ElicitResult{Action: "accept"}, nil
			})
			s, _, _ := f.m.Start(context.Background(), "google", "")
			_, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "sensitive_input", "detail": "Enter the 2FA code"})
			if shown == nil || shown.Mode != "url" || !strings.Contains(shown.URL, "/h/") || !strings.Contains(shown.Message, "Enter the 2FA code") ||
				!strings.Contains(shown.Message, "accounts.example.com") || strings.Contains(shown.Message, "x=1") {
				t.Fatalf("elicitation %+v", shown)
			}
			if !strings.Contains(out, "done") || !strings.Contains(out, "used a backup code") || !strings.Contains(out, "re-read") {
				t.Fatalf("request_handoff: %s", out)
			}
			if live, _ := f.m.Live("google"); live.State != sessions.Running {
				t.Fatalf("browser not given back: %s", live.State)
			}
		})
	}
}

func TestDeclinedHandoffGivesTheBrowserBack(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], func(*sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return &sdk.ElicitResult{Action: "decline"}, nil
	})
	s, _, _ := f.m.Start(context.Background(), "google", "")
	_, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "captcha", "detail": "Solve the CAPTCHA"})
	if !strings.Contains(out, "declined") {
		t.Fatalf("request_handoff: %s", out)
	}
	if live, _ := f.m.Live("google"); live.State != sessions.Running {
		t.Fatalf("browser still waiting: %s", live.State)
	}
}

func TestHandoffWithoutElicitationGivesTheLink(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	s, _, _ := f.m.Start(context.Background(), "google", "")
	_, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "captcha", "detail": "Solve the CAPTCHA"})
	if !strings.Contains(out, "/h/") || !strings.Contains(out, "wait_for_handoff") {
		t.Fatalf("request_handoff: %s", out)
	}
	open, _ := f.handoffs.Pending(context.Background())
	if len(open) != 1 {
		t.Fatalf("pending: %+v", open)
	}
	_, out = call(t, cs, "wait_for_handoff", map[string]any{"handoff_id": open[0].ID, "seconds": 1})
	if !strings.Contains(out, "Still waiting") {
		t.Fatalf("wait_for_handoff while pending: %s", out)
	}
	if _, err := f.handoffs.Finish(context.Background(), open[0], handoff.Cant, "no phone here"); err != nil {
		t.Fatal(err)
	}
	_, out = call(t, cs, "wait_for_handoff", map[string]any{"handoff_id": open[0].ID})
	if !strings.Contains(out, "cant") || !strings.Contains(out, "no phone here") {
		t.Fatalf("wait_for_handoff after: %s", out)
	}
}

func TestScreenToScreenshotScaling(t *testing.T) {
	sc := screen{1440, 900}
	if w, h := sc.shot(); w != 1152 || h != 720 {
		t.Fatalf("shot %dx%d", w, h)
	}
	if x, y := sc.toScreen(576, 360); x != 720 || y != 450 {
		t.Fatalf("centre maps to %d,%d", x, y)
	}
	if x, y := sc.toScreen(5000, 5000); x != 1439 || y != 899 {
		t.Fatalf("off the edge maps to %d,%d", x, y)
	}
	if w, h := (screen{1280, 720}).shot(); w != 1280 || h != 720 {
		t.Fatalf("a small screen is scaled: %dx%d", w, h)
	}
}
