package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
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
	mcp      *Server
	srv      *httptest.Server
	all      string // a token for every profile
	scoped   string // a token for google only

	mu     sync.Mutex
	clicks []string // what the fake browser was asked to click
	types  []string // and to type
	opens  []string // and to open
	// onClick, if set, runs as the fake browser clicks: a download, say.
	onClick func()
	// pageURL is the one tab's address, as /json/list answers it (a
	// handoff's until_url watch); "" for a sign-in page.
	pageURL string
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
	rt.Control = cdptest.New(f.targets).Handler(f.browser())
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
	f.mcp = New(routes, web, log, "test")
	f.mcp.Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// targets answers the fake browser's CDP: Target.getTargets only.
func (f *fixture) targets(m cdptest.Message) (any, error) {
	if m.Method != "Target.getTargets" {
		return nil, cdptest.Error{Message: "not in the fake: " + m.Method}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"targetInfos": []map[string]any{{"targetId": "p1", "type": "page", "url": f.pageURL}}}, nil
}

// browser is a fake browser's control socket: a 1440×900 screen.
func (f *fixture) browser() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /cdp/json/list", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		page := f.pageURL
		f.mu.Unlock()
		if page == "" {
			page = "https://accounts.example.com/signin?x=1"
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "p1", "type": "page", "url": page}})
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
	mux.HandleFunc("POST /input/type", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.types = append(f.types, string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /input/open", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.opens = append(f.opens, string(b))
		f.mu.Unlock()
		if strings.Contains(string(b), "covered") {
			http.Error(w, "a pop-up window covers the browser's address bar: finish in it or close it", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /input/click", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.clicks = append(f.clicks, string(b))
		onClick := f.onClick
		f.mu.Unlock()
		if onClick != nil {
			onClick()
		}
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
	if got := cs.InitializeResult().ProtocolVersion; got != version {
		f.t.Fatalf("asked for %s, got %s", version, got)
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

// versions are the protocols the endpoint serves. It is stateful, so a
// 2026-07-28 client falls back to 2025-11-25.
var versions = []string{"2025-11-25"}

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

// A headless client declines without asking anyone: the agent gets the
// link to pass on, and can withdraw it.
func TestDeclinedHandoffGivesTheLinkAndCanBeCancelled(t *testing.T) {
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			f := newFixture(t)
			cs := f.connect(f.all, v, func(*sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				return &sdk.ElicitResult{Action: "decline"}, nil
			})
			s, _, _ := f.m.Start(context.Background(), "google", "")
			_, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "captcha", "detail": "Solve the CAPTCHA"})
			open, _ := f.handoffs.Pending(context.Background())
			if len(open) != 1 || !strings.Contains(out, "/h/") || !strings.Contains(out, "wait_for_handoff") || !strings.Contains(out, open[0].ID) {
				t.Fatalf("request_handoff: %s (pending %+v)", out, open)
			}
			_, out = call(t, cs, "cancel_handoff", map[string]any{"handoff_id": open[0].ID})
			if !strings.Contains(out, "Withdrawn") {
				t.Fatalf("cancel_handoff: %s", out)
			}
			if live, _ := f.m.Live("google"); live.State != sessions.Running {
				t.Fatalf("browser still waiting: %s", live.State)
			}
		})
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

func TestClickIsScaledToTheScreen(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	s, _, _ := f.m.Start(context.Background(), "google", "")
	// The centre of a 1152×720 screenshot is the centre of the 1440×900 screen.
	_, out := call(t, cs, "click", map[string]any{"session_id": s.ID, "x": 576, "y": 360, "double": true})
	if !strings.Contains(out, "Clicked") {
		t.Fatalf("click: %s", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.clicks) != 1 || !strings.Contains(f.clicks[0], `"x":720`) || !strings.Contains(f.clicks[0], `"y":450`) || !strings.Contains(f.clicks[0], `"double":true`) {
		t.Fatalf("the browser was asked to click %v", f.clicks)
	}
}

// Human-paced typing through MCP stops at 150 characters a call, so the
// call ends (about 40 s) before a client's request timeout; instant pace,
// and act's instant type steps, aren't held to it.
func TestHumanTypingThroughMCPIsCapped(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	s, _, _ := f.m.Start(context.Background(), "google", "")
	try := func(name string, args map[string]any) (bool, string) {
		t.Helper()
		args["session_id"] = s.ID
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError, res.Content[0].(*sdk.TextContent).Text
	}
	long, most := strings.Repeat("é", 151), strings.Repeat("a", 150)
	if isErr, out := try("type_text", map[string]any{"text": long}); !isErr || !strings.Contains(out, "151 characters at human pace, at most 150") || !strings.Contains(out, "pace instant") {
		t.Errorf("151 characters at human pace: %v %s", isErr, out)
	}
	if isErr, out := try("type_text", map[string]any{"text": long, "pace": "human"}); !isErr || !strings.Contains(out, "at most 150") {
		t.Errorf("151 characters at pace human: %v %s", isErr, out)
	}
	if isErr, out := try("type_text", map[string]any{"text": most}); isErr {
		t.Errorf("150 characters: %s", out)
	}
	if isErr, out := try("type_text", map[string]any{"text": long, "pace": "instant"}); isErr {
		t.Errorf("151 characters at instant pace: %s", out)
	}
	f.mu.Lock()
	if len(f.types) != 2 {
		t.Errorf("the browser was asked to type %d times, want 2", len(f.types))
	}
	f.mu.Unlock()

	step := func(text, pace string) map[string]any {
		return map[string]any{"action": "type", "ref": "e1", "text": text, "pace": pace}
	}
	isErr, out := try("act", map[string]any{"steps": []any{step(strings.Repeat("a", 100), ""), step(strings.Repeat("b", 60), "human")}})
	if !isErr || !strings.Contains(out, "type steps: 160 characters at human pace, at most 150") || !strings.Contains(out, "use fill") {
		t.Errorf("act, 160 characters at human pace: %v %s", isErr, out)
	}
	// With one step instant, the call passes the cap (and this fixture's
	// Visual-mode browser refuses it for what it is).
	if _, out := try("act", map[string]any{"steps": []any{step(strings.Repeat("a", 100), ""), step(strings.Repeat("b", 60), "instant")}}); strings.Contains(out, "at most 150") {
		t.Errorf("act, 100 characters at human pace: %s", out)
	}
}

// open_url opens the address through the browser's address bar
// (PROJ-164): the browser gets the URL, and a refusal comes back as the
// tool's error.
func TestOpenURL(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	s, _, _ := f.m.Start(context.Background(), "google", "")
	try := func(args map[string]any) (bool, string) {
		t.Helper()
		args["session_id"] = s.ID
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "open_url", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError, res.Content[0].(*sdk.TextContent).Text
	}
	if isErr, out := try(map[string]any{"url": "https://example.com/a"}); isErr || !strings.Contains(out, "Opened it in the address bar") {
		t.Fatalf("open: %v %s", isErr, out)
	}
	if isErr, out := try(map[string]any{"url": "https://example.com/covered"}); !isErr || !strings.Contains(out, "pop-up window covers") {
		t.Errorf("covered: %v %s", isErr, out)
	}
	if isErr, out := try(map[string]any{"url": "javascript:alert(1)"}); !isErr || !strings.Contains(out, "only http and https") {
		t.Errorf("javascript: %v %s", isErr, out)
	}
	// The URL goes in at once at either pace: no 150-character cap.
	long := "https://example.com/" + strings.Repeat("a", 280)
	if isErr, out := try(map[string]any{"url": long}); isErr {
		t.Errorf("300 characters at human pace: %s", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{`{"url":"https://example.com/a"}`, `{"url":"https://example.com/covered"}`, `{"url":"` + long + `"}`}
	if !slices.Equal(f.opens, want) {
		t.Errorf("the browser was asked to open %q", f.opens)
	}
}

// A client whose link dialog fails still leaves the agent the handoff's
// ID and link, so the browser isn't stuck for 15 minutes.
func TestFailedElicitationStillGivesTheLink(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], func(*sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return nil, errors.New("the dialog was closed")
	})
	s, _, _ := f.m.Start(context.Background(), "google", "")
	res, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "captcha", "detail": "Solve the CAPTCHA"})
	open, _ := f.handoffs.Pending(context.Background())
	if res.IsError || len(open) != 1 || !strings.Contains(out, "/h/") || !strings.Contains(out, open[0].ID) {
		t.Fatalf("request_handoff: %s (pending %+v)", out, open)
	}
	if _, out := call(t, cs, "cancel_handoff", map[string]any{"handoff_id": open[0].ID}); !strings.Contains(out, "Withdrawn") {
		t.Fatalf("cancel_handoff: %s", out)
	}
}

// request_handoff passes until_url, and wait_for_handoff says the handoff
// ended by itself when the page got there, without quoting the address.
func TestHandoffUntilURLOverMCP(t *testing.T) {
	f := newFixture(t)
	f.mu.Lock()
	f.pageURL = "https://site.example/login"
	f.mu.Unlock()
	cs := f.connect(f.all, versions[0], nil)
	s, _, _ := f.m.Start(context.Background(), "google", "")
	res, out := call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "sensitive_input", "detail": "Sign in", "until_url": "/account?x"})
	if !res.IsError || !strings.Contains(out, "until_url") {
		t.Fatalf("request_handoff with a query in until_url: %s", out)
	}
	_, out = call(t, cs, "request_handoff", map[string]any{"session_id": s.ID, "reason": "sensitive_input", "detail": "Sign in", "until_url": "site.example/account"})
	open, _ := f.handoffs.Pending(context.Background())
	if len(open) != 1 || open[0].UntilURL != "site.example/account" || !strings.Contains(out, "wait_for_handoff") {
		t.Fatalf("request_handoff: %s (pending %+v)", out, open)
	}
	// The person opens the link: the watch looks from then.
	if err := f.handoffs.Unlock(context.Background(), open[0], "the person's browser"); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(time.Second)
		f.mu.Lock()
		f.pageURL = "https://site.example/account/overview?session=s3cr3t"
		f.mu.Unlock()
	}()
	_, out = call(t, cs, "wait_for_handoff", map[string]any{"handoff_id": open[0].ID, "seconds": 20})
	if !strings.Contains(out, "Handoff done") || !strings.Contains(out, "until_url") || strings.Contains(out, "s3cr3t") || strings.Contains(out, "overview") ||
		strings.Contains(out, "The person's note") {
		t.Fatalf("wait_for_handoff: %s", out)
	}
	if live, _ := f.m.Live("google"); live.State != sessions.Running {
		t.Fatalf("browser not given back: %s", live.State)
	}
}

// start_browser says whether ads and trackers are blocked: not when the
// profile blocks but no list has been downloaded yet, which it says.
func TestStartBrowserSaysWhetherAdsAreBlocked(t *testing.T) {
	f := newFixture(t)
	var list []byte
	f.m.SetBlocklist(func() []byte { return list })
	cs := f.connect(f.all, versions[0], nil)
	_, out := call(t, cs, "start_browser", map[string]any{"profile": "google"})
	if strings.Contains(out, "Ads and trackers are blocked") || !strings.Contains(out, "no list has been downloaded yet, so nothing is blocked") {
		t.Errorf("start_browser without a list: %s", out)
	}
	s, _ := f.m.Live("google")
	call(t, cs, "stop_browser", map[string]any{"session_id": s.ID})
	list = []byte("# browserpier domain set 1\nads.example.com\n")
	_, out = call(t, cs, "start_browser", map[string]any{"profile": "google"})
	if !strings.Contains(out, "Ads and trackers are blocked") || strings.Contains(out, "no list") {
		t.Errorf("start_browser with a list: %s", out)
	}
}
