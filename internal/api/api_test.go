package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// browser emulates the image's control socket.
type browser struct {
	mu     sync.Mutex
	inputs []string // path and body of each input request
	leaks  []string // credentials that reached the browser
}

func (b *browser) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /cdp/json/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Browser":"Chrome/156","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`)
	})
	mux.HandleFunc("GET /cdp/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"p1","type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/p1","devtoolsFrontendUrl":"/devtools/inspector.html?ws=127.0.0.1:9222/devtools/page/p1"}]`)
	})
	mux.HandleFunc("GET /cdp/devtools/browser/abc", func(w http.ResponseWriter, r *http.Request) {
		b.checkLeaks(r)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			typ, msg, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			_ = conn.Write(r.Context(), typ, append([]byte("echo:"), msg...))
		}
	})
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Screen-Width", "1440")
		w.Header().Set("X-Screen-Height", "900")
		_, _ = io.WriteString(w, "PNG:"+r.URL.Query().Get("width"))
	})
	input := func(w http.ResponseWriter, r *http.Request) {
		b.checkLeaks(r)
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.inputs = append(b.inputs, r.URL.Path+" "+string(body))
		b.mu.Unlock()
		if strings.Contains(string(body), "99999") {
			http.Error(w, "(99999, 1) is outside the 1440x900 screen", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("POST /input/click", input)
	mux.HandleFunc("POST /input/type", input)
	return mux
}

func (b *browser) checkLeaks(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := r.Header.Get("Authorization"); v != "" {
		b.leaks = append(b.leaks, "Authorization: "+v)
	}
	if v := r.URL.Query().Get("token"); v != "" {
		b.leaks = append(b.leaks, "token="+v)
	}
	if v := r.Header.Get("Cookie"); v != "" {
		b.leaks = append(b.leaks, "Cookie: "+v)
	}
}

type fixture struct {
	t        *testing.T
	srv      *httptest.Server
	m        *sessions.Manager
	rt       *sessionstest.FakeRuntime
	b        *browser
	all      string // token for all profiles
	scoped   string // token for google only
	tokens   *auth.Tokens
	profiles *profiles.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "bp")
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
	f := &fixture{t: t, b: &browser{}, rt: sessionstest.New(dir), tokens: auth.NewTokens(d), profiles: profiles.NewStore(d)}
	f.rt.Control = f.b.handler()
	f.m, err = sessions.NewManager(d, f.rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.m.Close)
	for _, n := range []string{"google", "github"} {
		p := profiles.New(n)
		p.DefaultMode = profiles.ModeAPI
		p.Proxy = "http://user:s3cret@proxy.example:8080"
		if _, err := f.profiles.Create(ctx, p); err != nil {
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
	web := auth.NewWeb(admin, f.tokens, log, auth.WebOptions{CookieSecure: "auto"})
	mux := http.NewServeMux()
	New(f.m, web, log, "").Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// call makes an API request and decodes a JSON answer into out (if not nil).
func (f *fixture) call(token, method, path, body string, out any) int {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

func (f *fixture) start(token, profile, mode string) sessionJSON {
	f.t.Helper()
	var s sessionJSON
	if code := f.call(token, http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": %q}`, profile, mode), &s); code != http.StatusCreated {
		f.t.Fatalf("start %s: %d %+v", profile, code, s)
	}
	return s
}

func TestEveryRouteNeedsAToken(t *testing.T) {
	f := newFixture(t)
	for _, r := range [][2]string{
		{"GET", "/api/profiles"}, {"POST", "/api/profiles"}, {"POST", "/api/sessions"}, {"GET", "/api/sessions"},
		{"GET", "/api/sessions/x"}, {"DELETE", "/api/sessions/x"}, {"GET", "/api/sessions/x/screenshot"},
		{"POST", "/api/sessions/x/click"}, {"POST", "/api/sessions/x/type"}, {"GET", "/cdp/google"}, {"GET", "/cdp/google/json/version"},
	} {
		if code := f.call("", r[0], r[1], "{}", nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", r[0], r[1], code)
		}
	}
}

func TestProfiles(t *testing.T) {
	f := newFixture(t)
	var list struct{ Profiles []profileJSON }
	f.call(f.all, http.MethodGet, "/api/profiles", "", &list)
	if len(list.Profiles) != 2 || list.Profiles[0].Status != "stopped" {
		t.Fatalf("all-profiles token sees %+v", list.Profiles)
	}
	if p := list.Profiles[0].Proxy; strings.Contains(p, "s3cret") || !strings.Contains(p, "proxy.example:8080") {
		t.Fatalf("proxy shown as %q", p)
	}
	f.call(f.scoped, http.MethodGet, "/api/profiles", "", &list)
	if len(list.Profiles) != 1 || list.Profiles[0].Name != "google" {
		t.Fatalf("scoped token sees %+v", list.Profiles)
	}

	var p profileJSON
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "linkedin", "screen": {"width": 1280, "height": 800}}`, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if p.DefaultMode != "visual" || p.AutoStopMinutes != 15 || p.Screen.Width != 1280 || p.HandoffPolicy != "pin" {
		t.Fatalf("created %+v", p)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "linkedin"}`, nil); code != http.StatusConflict {
		t.Fatalf("duplicate: %d", code)
	}
	if code := f.call(f.scoped, http.MethodPost, "/api/profiles", `{"name": "x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("scoped create: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "y", "proxy": "http://evil:1"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("proxy through the API: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "Bad Name"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	// An agent can make handoffs stricter, never looser.
	f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "loose", "handoff_policy": "link"}`, &p)
	if p.HandoffPolicy != "pin" {
		t.Fatalf("an agent lowered the handoff policy to %s", p.HandoffPolicy)
	}
	f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "strict", "handoff_policy": "signin"}`, &p)
	if p.HandoffPolicy != "signin" {
		t.Fatalf("an agent couldn't raise the handoff policy: %s", p.HandoffPolicy)
	}
}

func TestSessions(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "")
	if s.State != "running" || s.Mode != "api" || s.Attached {
		t.Fatalf("started %+v", s)
	}
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if s.CDPURL != "ws://"+host+"/cdp/google" || s.LiveViewURL != "http://"+host+"/live/google" {
		t.Fatalf("URLs %q, %q", s.CDPURL, s.LiveViewURL)
	}

	var again sessionJSON
	if code := f.call(f.scoped, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &again); code != http.StatusOK || !again.Attached || again.ID != s.ID {
		t.Fatalf("second start: %d %+v", code, again)
	}
	if code := f.call(f.scoped, http.MethodPost, "/api/sessions", `{"profile": "github"}`, nil); code != http.StatusNotFound {
		t.Fatalf("scoped token starting another profile: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "nope"}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google", "mode": "cdp"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", code)
	}

	gh := f.start(f.all, "github", "visual")
	if gh.CDPURL != "" {
		t.Fatalf("a Visual-mode session offers a CDP URL: %q", gh.CDPURL)
	}
	var list struct{ Sessions []sessionJSON }
	f.call(f.scoped, http.MethodGet, "/api/sessions", "", &list)
	if len(list.Sessions) != 1 || list.Sessions[0].Profile != "google" {
		t.Fatalf("scoped list %+v", list.Sessions)
	}
	if code := f.call(f.scoped, http.MethodGet, "/api/sessions/"+gh.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("scoped token reading another profile's session: %d", code)
	}
	if code := f.call(f.scoped, http.MethodDelete, "/api/sessions/"+gh.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("scoped token stopping another profile's session: %d", code)
	}

	var stopped sessionJSON
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", &stopped); code != http.StatusOK || stopped.State != "stopped" {
		t.Fatalf("stop: %d %+v", code, stopped)
	}
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &stopped); code != http.StatusOK || stopped.State != "stopped" || stopped.CDPURL != "" {
		t.Fatalf("stopped session: %d %+v", code, stopped)
	}
}

func TestFailedStartReportsWhy(t *testing.T) {
	f := newFixture(t)
	status := 1
	f.rt.ExitOnStart = &status
	f.rt.LastLog = `level=ERROR msg=exiting err="profile /profile is not writable"`
	var s sessionJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &s); code != http.StatusBadGateway {
		t.Fatalf("failed start: %d", code)
	}
	if s.State != "failed" || s.Error == "" || s.LastLog != f.rt.LastLog {
		t.Fatalf("failed session %+v", s)
	}
}

func TestCDPProxy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if code := f.call(f.all, http.MethodGet, "/cdp/google/json/version", "", nil); code != http.StatusConflict {
		t.Fatalf("CDP with no session: %d", code)
	}
	s := f.start(f.all, "google", "api")
	host := strings.TrimPrefix(f.srv.URL, "http://")

	var version map[string]any
	f.call(f.all, http.MethodGet, "/cdp/google/json/version", "", &version)
	if version["webSocketDebuggerUrl"] != "ws://"+host+"/cdp/google/devtools/browser/abc" {
		t.Fatalf("version %v", version)
	}
	var targets []map[string]any
	f.call(f.all, http.MethodGet, "/cdp/google/json/list", "", &targets)
	if len(targets) != 1 || targets[0]["webSocketDebuggerUrl"] != "ws://"+host+"/cdp/google/devtools/page/p1" || targets[0]["devtoolsFrontendUrl"] != nil {
		t.Fatalf("targets %v", targets)
	}

	// A client that can only put the token in the URL gets URLs that carry it.
	resp, err := http.Get(f.srv.URL + "/cdp/google/json/version?token=" + f.all)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&version)
	_ = resp.Body.Close()
	if version["webSocketDebuggerUrl"] != "ws://"+host+"/cdp/google/devtools/browser/abc?token="+f.all {
		t.Fatalf("query-token version %v", version)
	}

	// The browser WebSocket, with the token in the query, as Puppeteer would.
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	//nolint:bodyclose // after an upgrade the connection owns the body
	conn, _, err := websocket.Dial(dctx, "ws://"+host+"/cdp/google?token="+f.all, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(dctx, websocket.MessageText, []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := conn.Read(dctx); err != nil || string(msg) != `echo:{"id":1}` {
		t.Fatalf("relay: %q, %v", msg, err)
	}
	if live, _ := f.m.Live("google"); strings.Join(live.InUseBy, ",") != "claude" {
		t.Fatalf("in use by %v while connected", live.InUseBy)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// Page targets with the token in a header, as Playwright would.
	//nolint:bodyclose // as above
	conn, _, err = websocket.Dial(dctx, "ws://"+host+"/cdp/google/devtools/browser/abc", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + f.all}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	f.b.mu.Lock()
	leaks := f.b.leaks
	f.b.mu.Unlock()
	if len(leaks) != 0 {
		t.Fatalf("agent credentials reached the browser: %v", leaks)
	}
	waitFor(t, func() bool { live, _ := f.m.Live("google"); return len(live.InUseBy) == 0 })

	if code := f.call(f.scoped, http.MethodGet, "/cdp/github/json/version", "", nil); code != http.StatusNotFound {
		t.Fatalf("scoped token on another profile's CDP: %d", code)
	}
	if code := f.call(f.all, http.MethodGet, "/cdp/google/json/new", "", nil); code != http.StatusNotFound {
		t.Fatalf("unrelayed endpoint: %d", code)
	}

	// No CDP in Visual mode, and none while a person has the browser.
	f.start(f.all, "github", "visual")
	if code := f.call(f.all, http.MethodGet, "/cdp/github/json/version", "", nil); code != http.StatusConflict {
		t.Fatalf("CDP to a Visual-mode session: %d", code)
	}
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if code := f.call(f.all, http.MethodGet, "/cdp/google/json/version", "", nil); code != http.StatusConflict {
		t.Fatalf("CDP during a handoff: %d", code)
	}
}

func TestVisualEndpoints(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/sessions/"+s.ID+"/screenshot?width=640", nil)
	req.Header.Set("Authorization", "Bearer "+f.all)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "PNG:640" || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Screen-Width") != "1440" {
		t.Fatalf("screenshot: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID+"/screenshot?width=nope", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad width: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 10, "y": 20}`, nil); code != http.StatusNoContent {
		t.Fatalf("click: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/type", `{"text": "hello"}`, nil); code != http.StatusNoContent {
		t.Fatalf("type: %d", code)
	}
	var e map[string]string
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 99999, "y": 1}`, &e); code != http.StatusBadRequest || !strings.Contains(e["error"], "outside") {
		t.Fatalf("click off screen: %d %v", code, e)
	}
	f.b.mu.Lock()
	inputs := strings.Join(f.b.inputs, "\n")
	f.b.mu.Unlock()
	if !strings.Contains(inputs, `/input/click {"x": 10, "y": 20}`) || !strings.Contains(inputs, `/input/type {"text": "hello"}`) {
		t.Fatalf("inputs %q", inputs)
	}

	// While a person has the browser, agents can look but not touch.
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 1, "y": 1}`, nil); code != http.StatusConflict {
		t.Fatalf("click during a handoff: %d", code)
	}
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID+"/screenshot", "", nil); code != http.StatusOK {
		t.Fatalf("screenshot during a handoff: %d", code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
