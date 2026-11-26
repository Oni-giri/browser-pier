package api

import (
	"context"
	"database/sql"
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
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// browser emulates the image's control socket.
type browser struct {
	mu     sync.Mutex
	inputs []string // path and body of each input request
	leaks  []string // credentials that reached the browser
	// noScreen makes screenshots fail.
	noScreen bool
	// onInput, if set, runs as each input action does, with its path:
	// a click that starts a download, say.
	onInput func(path string)
	// pageURL is the one tab's address, as /json/list and
	// Target.getTargets answer it (a handoff's until_url watch reads the
	// former); other messages on the browser's WebSocket are echoed.
	pageURL string
	// methods are the CDP methods sent on the browser's WebSocket.
	methods []string
	// choosers is what GET /chooser answers (JSON), "" for none open;
	// noPortal makes it 503. chosen records each POST /chooser/{id}.
	choosers string
	noPortal bool
	chosen   []string
	// chooserStatus, if set, is what POST /chooser/{id} answers.
	chooserStatus int
}

func (b *browser) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /cdp/json/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Browser":"Chrome/156","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`)
	})
	mux.HandleFunc("GET /cdp/json/list", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		page := b.pageURL
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "p1", "type": "page", "url": page,
			"webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/page/p1", "devtoolsFrontendUrl": "/devtools/inspector.html?ws=127.0.0.1:9222/devtools/page/p1"}})
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
			var call struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(msg, &call) == nil && call.Method != "" {
				b.mu.Lock()
				b.methods = append(b.methods, call.Method)
				page := b.pageURL
				b.mu.Unlock()
				if call.Method == "Target.getTargets" {
					out, _ := json.Marshal(map[string]any{"id": call.ID, "result": map[string]any{"targetInfos": []map[string]any{{"targetId": "p1", "type": "page", "url": page}}}})
					_ = conn.Write(r.Context(), typ, out)
					continue
				}
				if call.Method == "Target.attachToTarget" {
					// The page tools get no further: they answer at once
					// that the tab can't be reached.
					out, _ := json.Marshal(map[string]any{"id": call.ID, "error": map[string]any{"code": -32000, "message": "No target with given id found"}})
					_ = conn.Write(r.Context(), typ, out)
					continue
				}
			}
			_ = conn.Write(r.Context(), typ, append([]byte("echo:"), msg...))
		}
	})
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		broken := b.noScreen
		b.mu.Unlock()
		if broken {
			http.Error(w, "no display", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Screen-Width", "1440")
		w.Header().Set("X-Screen-Height", "900")
		if width := r.URL.Query().Get("width"); width != "" {
			_, _ = io.WriteString(w, "PNG:"+width)
			return
		}
		_, _ = w.Write(screenPNG) // full size: zoom crops it
	})
	input := func(w http.ResponseWriter, r *http.Request) {
		b.checkLeaks(r)
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.inputs = append(b.inputs, r.URL.Path+" "+string(body))
		onInput := b.onInput
		b.mu.Unlock()
		if onInput != nil {
			onInput(r.URL.Path)
		}
		if strings.Contains(string(body), "boom") {
			http.Error(w, "xdotool failed", http.StatusInternalServerError)
			return
		}
		if strings.Contains(string(body), "unbound") {
			// A type that failed before any key was pressed (PROJ-195).
			w.Header().Set("X-Nothing-Typed", "1")
			http.Error(w, "typing failed before anything was typed, so it can be retried, with the same call id if it had one: the keyboard map has no spare key to bind a character to", http.StatusInternalServerError)
			return
		}
		if strings.Contains(string(body), "99999") {
			http.Error(w, "(99999, 1) is outside the 1440x900 screen", http.StatusBadRequest)
			return
		}
		if strings.Contains(string(body), "slowly") {
			// A long human-paced action: it lasts until it's cut short.
			select {
			case <-r.Context().Done():
				b.mu.Lock()
				b.inputs = append(b.inputs, "cut short")
				b.mu.Unlock()
			case <-time.After(30 * time.Second):
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
	for _, p := range []string{"click", "type", "scroll", "key", "drag"} {
		mux.HandleFunc("POST /input/"+p, input)
	}
	mux.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.noPortal {
			http.Error(w, "the file chooser portal isn't running in this browser", http.StatusServiceUnavailable)
			return
		}
		list := b.choosers
		if list == "" {
			list = "[]"
		}
		_, _ = io.WriteString(w, `{"choosers":`+list+`}`)
	})
	// Control changing hands restarts the choosers' time limit (PROJ-181).
	mux.HandleFunc("POST /chooser/restart", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.chosen = append(b.chosen, r.PathValue("id")+" "+string(body))
		switch {
		case b.noPortal:
			http.Error(w, "the file chooser portal isn't running in this browser", http.StatusServiceUnavailable)
		case b.chooserStatus != 0:
			http.Error(w, "refused", b.chooserStatus)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

func (b *browser) checkLeaks(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := r.Header.Get("Authorization"); v != "" {
		b.leaks = append(b.leaks, "Authorization: "+v)
	}
	for _, k := range []string{"token", "ticket"} {
		if v := r.URL.Query().Get(k); v != "" {
			b.leaks = append(b.leaks, k+"="+v)
		}
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
	web      *auth.Web
	handoffs *handoff.Service
	api      *API
	db       *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureLogging(t, slog.New(slog.DiscardHandler))
}

// newFixtureLogging is newFixture, with the manager and the API logging
// to log.
func newFixtureLogging(t *testing.T, log *slog.Logger) *fixture {
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
	f := &fixture{t: t, b: &browser{}, rt: sessionstest.New(dir), tokens: auth.NewTokens(d), profiles: profiles.NewStore(d), db: d}
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
	f.web = web
	mux := http.NewServeMux()
	a := New(f.m, web, files.New(f.m.Layout().FilesDir, 1<<20, os.Getuid(), os.Getgid()), log, "")
	f.handoffs = handoff.New(d, f.m, log)
	a.SetHandoffs(f.handoffs)
	a.Register(mux)
	f.api = a
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

// ticket mints a CDP ticket for the session and returns its cdp_url.
func (f *fixture) ticket(token, sessionID string) string {
	f.t.Helper()
	var out struct {
		Ticket  string `json:"ticket"`
		CDPURL  string `json:"cdp_url"`
		Expires string `json:"expires_at"`
	}
	if code := f.call(token, http.MethodPost, "/api/sessions/"+sessionID+"/cdp-ticket", "", &out); code != http.StatusCreated {
		f.t.Fatalf("cdp-ticket: %d %+v", code, out)
	}
	if !strings.HasPrefix(out.Ticket, auth.TicketPrefix) || !strings.HasSuffix(out.CDPURL, "?ticket="+out.Ticket) || out.Expires == "" {
		f.t.Fatalf("cdp-ticket: %+v", out)
	}
	return out.CDPURL
}

func TestEveryRouteNeedsAToken(t *testing.T) {
	f := newFixture(t)
	for _, r := range [][2]string{
		{"GET", "/api/profiles"}, {"POST", "/api/profiles"}, {"POST", "/api/sessions"}, {"GET", "/api/sessions"},
		{"GET", "/api/sessions/x"}, {"DELETE", "/api/sessions/x"}, {"GET", "/api/sessions/x/screenshot"},
		{"POST", "/api/sessions/x/click"}, {"POST", "/api/sessions/x/type"}, {"POST", "/api/sessions/x/computer"}, {"GET", "/cdp/google"}, {"GET", "/cdp/google/json/version"},
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
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "y", "local_network": true}`, nil); code != http.StatusBadRequest {
		t.Fatalf("local network through the API: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "y", "local_network": false}`, &p); code != http.StatusCreated || p.LocalNetwork {
		t.Fatalf("local network off through the API: %d %+v", code, p)
	}
	// Ad and tracker blocking: on for new profiles; an agent may ask for
	// it, never turn it off for good.
	if !p.BlockAds {
		t.Fatalf("a new profile doesn't block ads: %+v", p)
	}
	// Recording is the admin's alone (PROJ-23): no field an agent can set.
	for _, field := range []string{"record", "record_sessions", "Record", "recording"} {
		if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "r", "`+field+`": true}`, nil); code != http.StatusBadRequest {
			t.Fatalf("%s through the API: %d", field, code)
		}
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "z", "block_ads": false}`, nil); code != http.StatusBadRequest {
		t.Fatalf("block_ads off through the API: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "z", "block_ads": true}`, &p); code != http.StatusCreated || !p.BlockAds {
		t.Fatalf("block_ads on through the API: %d %+v", code, p)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "Bad Name"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	// The main site and fallback icon.
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "mail", "site": "https://Mail.Google.com/mail/", "icon_glyph": "envelope", "icon_color": "sky"}`, &p); code != http.StatusCreated ||
		p.Site != "mail.google.com" || p.IconGlyph != "envelope" || p.IconColor != "sky" {
		t.Fatalf("site and icon: %d %+v", code, p)
	}
	if stored, _ := f.m.Profiles().Get(context.Background(), "mail"); stored.Site != "mail.google.com" {
		t.Fatalf("stored site %q", stored.Site)
	}
	for _, body := range []string{`{"name": "s1", "site": "not a site"}`, `{"name": "s2", "site": "a_b.com"}`,
		`{"name": "s3", "icon_glyph": "unicorn"}`, `{"name": "s4", "icon_color": "orange"}`} {
		if code := f.call(f.all, http.MethodPost, "/api/profiles", body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	// The site inferred from the history isn't the agent's to see.
	if err := f.m.Profiles().SetSiteInferred(context.Background(), "linkedin", "linkedin.com"); err != nil {
		t.Fatal(err)
	}
	var raw struct{ Profiles []map[string]any }
	f.call(f.all, http.MethodGet, "/api/profiles", "", &raw)
	for _, pr := range raw.Profiles {
		if pr["name"] == "linkedin" {
			if _, ok := pr["site"]; ok {
				t.Fatalf("an inferred site shown: %v", pr)
			}
		}
		for k := range pr {
			if strings.Contains(k, "inferred") {
				t.Fatalf("field %s shown", k)
			}
		}
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
	if s.CDPURL != "ws://"+host+"/cdp/google" || s.LiveViewURL != "http://"+host+"/profiles/google" {
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
	if s.State != "failed" || !strings.HasPrefix(s.Error, "the browser exited unexpectedly (exit status 1): <untrusted-content-") || !strings.Contains(s.Error, "profile /profile is not writable") {
		t.Fatalf("failed session %+v; want the reason fenced after the error", s)
	}
	if !strings.HasPrefix(s.LastLog, "<untrusted-content-") || !strings.Contains(s.LastLog, f.rt.LastLog) {
		t.Fatalf("last_log %q isn't fenced", s.LastLog)
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

	// The browser WebSocket, with a ticket in the query, as Puppeteer would.
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	//nolint:bodyclose // after an upgrade the connection owns the body
	conn, _, err := websocket.Dial(dctx, f.ticket(f.all, s.ID), nil)
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

func TestCDPConnectionsCloseOnHandoffAndRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	host := strings.TrimPrefix(f.srv.URL, "http://")
	dial := func(token string) *websocket.Conn {
		t.Helper()
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		//nolint:bodyclose // after an upgrade the connection owns the body
		conn, _, err := websocket.Dial(dctx, "ws://"+host+"/cdp/google", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	closed := func(conn *websocket.Conn) bool {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, _, err := conn.Read(rctx)
		return err != nil && rctx.Err() == nil
	}

	s := f.start(f.all, "google", "api")
	conn := dial(f.all)
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if !closed(conn) {
		t.Fatal("the agent's CDP connection stayed open while a person has the browser")
	}
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}

	tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn = dial(secret)
	waitFor(t, func() bool { live, _ := f.m.Live("google"); return len(live.InUseBy) == 1 })
	if err := f.tokens.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	a := New(f.m, f.web, files.New(f.m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), slog.New(slog.DiscardHandler), "")
	a.checkRevocations(ctx)
	if !closed(conn) {
		t.Fatal("a revoked token's CDP connection stayed open")
	}
}

func TestNoRoomNamesOnlyVisibleProfiles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const gib = 1 << 30
	f.rt.Memory = gib
	f.m.SetMemoryLimit(gib+gib/2, gib) // room for one
	gh := f.start(f.all, "github", "")
	_, release, err := f.m.Attach(ctx, gh.ID, "t", "claude")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var e map[string]string
	if code := f.call(f.scoped, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &e); code != http.StatusServiceUnavailable {
		t.Fatalf("start with no room: %d %v", code, e)
	}
	if strings.Contains(e["error"], "github") || strings.Contains(e["error"], "claude") || !strings.Contains(e["error"], "1 more") {
		t.Fatalf("a scoped token was told about other profiles: %q", e["error"])
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &e); code != http.StatusServiceUnavailable || !strings.Contains(e["error"], "github (1.0 GB, in use by claude)") {
		t.Fatalf("all-profiles token: %d %q", code, e["error"])
	}
}

// With nothing running to stop, the refusal doesn't tell the agent to stop one.
func TestNoRoomOnAShortHost(t *testing.T) {
	f := newFixture(t)
	f.m.SetMemoryLimit(0, 1<<30)
	f.m.SetHostFree(func() (int64, error) { return 1 << 29, nil })
	var e map[string]string
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &e); code != http.StatusServiceUnavailable {
		t.Fatalf("start on a short host: %d %v", code, e)
	}
	if want := "not enough memory for another browser: the host has 0.5 GB free, other apps included, and a browser needs about 1.0 GB, with no browser running to stop"; e["error"] != want {
		t.Fatalf("error = %q, want %q", e["error"], want)
	}
}

// A profile being exported can't start; an agent is told why.
func TestStartWhileHeldIsAConflict(t *testing.T) {
	f := newFixture(t)
	release, err := f.m.Hold("google")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var e struct{ Error string }
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &e); code != http.StatusConflict || !strings.Contains(e.Error, "exported") {
		t.Fatalf("start while held: %d %s", code, e.Error)
	}
}

// TestSessionWithoutAdBlocking: an agent turns blocking off for one
// session; the profile keeps it.
func TestSessionWithoutAdBlocking(t *testing.T) {
	f := newFixture(t)
	var list []byte // none downloaded yet
	f.m.SetBlocklist(func() []byte { return list })
	// Without a list, a profile that blocks starts a session that blocks
	// nothing, and says why.
	var nl sessionJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "github"}`, &nl); code != http.StatusCreated || nl.BlockAds || !nl.AdListMissing {
		t.Fatalf("a start without a list: %d %+v", code, nl)
	}
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+nl.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	list = []byte("# browserpier domain set 1\nads.example.com\n")
	var s sessionJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google", "block_ads": false}`, &s); code != http.StatusCreated || s.BlockAds {
		t.Fatalf("start without blocking: %d %+v", code, s)
	}
	if p, _ := f.m.Profiles().Get(context.Background(), "google"); !p.BlockAds {
		t.Fatal("the profile's setting changed")
	}
	var again sessionJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google", "block_ads": true}`, &again); code != http.StatusOK || again.BlockAds {
		t.Fatalf("block_ads true turned blocking back on: %d %+v", code, again)
	}
	var gh sessionJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "github"}`, &gh); code != http.StatusCreated || !gh.BlockAds || gh.AdListMissing {
		t.Fatalf("a plain start doesn't block: %d %+v", code, gh)
	}
}
