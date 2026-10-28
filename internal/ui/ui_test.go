package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
	"github.com/Oni-giri/browser-pier/internal/settings"
)

type fixture struct {
	t        *testing.T
	m        *sessions.Manager
	handoffs *handoff.Service
	tokens   *auth.Tokens
	settings *settings.Store
	srv      *httptest.Server
	client   *http.Client
	ram      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "ui")
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
	rt := sessionstest.New(dir)
	ctl := http.NewServeMux()
	ctl.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	ctl.HandleFunc("GET /screenshot", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG fake"))
	})
	ctl.HandleFunc("GET /cdp/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"type":"page","url":"https://accounts.example.com/signin"}]`)
	})
	rt.Control = ctl
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.SetMemoryLimit(8<<30, 1<<30)
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(d)
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	f := &fixture{t: t, m: m, handoffs: handoff.New(d, m, log), tokens: tokens, settings: settings.New(d)}
	app := New(m, web, f.handoffs, f.settings, log, Options{HostMemory: 16 << 30, SetRAMLimit: func(p int) { f.ram = p }})
	mux := http.NewServeMux()
	web.Register(mux)
	app.Register(mux)
	f.srv = httptest.NewServer(new(http.CrossOriginProtection).Handler(mux))
	t.Cleanup(f.srv.Close)

	cookie, _, err := admin.NewSession(ctx, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(f.srv.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieName, Value: cookie, Path: "/"}})
	f.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

func (f *fixture) profile(name string) {
	f.t.Helper()
	if _, err := f.m.Profiles().Create(context.Background(), profiles.New(name)); err != nil {
		f.t.Fatal(err)
	}
}

type reply struct {
	code     int
	location string
	body     string
	header   http.Header
}

func (f *fixture) get(path string, header ...string) reply {
	f.t.Helper()
	return f.do(http.MethodGet, path, nil, header...)
}

func (f *fixture) post(path string, form url.Values, header ...string) reply {
	f.t.Helper()
	return f.do(http.MethodPost, path, form, header...)
}

func (f *fixture) do(method, path string, form url.Values, header ...string) reply {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header.Get("Location"), string(b), resp.Header}
}

func want(t *testing.T, r reply, code int, parts ...string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("got %d, want %d: %.400s", r.code, code, r.body)
	}
	for _, p := range parts {
		if !strings.Contains(r.body, p) {
			i := strings.Index(r.body, "class=\"errors\"")
			ctx := ""
			if i >= 0 {
				ctx = r.body[i:min(len(r.body), i+200)]
			}
			t.Fatalf("missing %q; error shown: %s", p, ctx)
		}
	}
}

func TestEveryPageRenders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, name := range []string{"google", "github", "notion"} {
		f.profile(name)
	}
	s, _, err := f.m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.handoffs.Create(ctx, s.ID, "research-bot", handoff.SensitiveInput, "Enter the 2FA code", ""); err != nil {
		t.Fatal(err)
	}
	gh, _, _ := f.m.Start(ctx, "github", "")
	if _, err := f.m.Stop(ctx, gh.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.tokens.Create(ctx, "research-bot", true, nil); err != nil {
		t.Fatal(err)
	}

	want(t, f.get("/"), http.StatusOK, "1 needs you", "Take control", "Enter the 2FA code", "research-bot")
	want(t, f.get("/profiles"), http.StatusOK, `id="card-google"`, "Needs you", "Last used", "Never started", "/profiles/google/preview.png")
	want(t, f.get("/profiles?view=table"), http.StatusOK, "<table", "Auto-stop 15 min idle")
	want(t, f.get("/profiles?show=stopped"), http.StatusOK, `id="card-github"`)
	if r := f.get("/profiles?show=stopped"); strings.Contains(r.body, `id="card-google"`) {
		t.Fatal("the stopped filter shows a running profile")
	}
	want(t, f.get("/profiles", "HX-Request", "true"), http.StatusOK, `id="summary" hx-swap-oob="true"`)
	want(t, f.get("/profiles/new"), http.StatusOK, "Create profile")
	want(t, f.get("/profiles/new", "HX-Request", "true"), http.StatusOK, "<dialog data-open")
	want(t, f.get("/profiles/google"), http.StatusOK, `id="stage"`, "/static/live.js", "Enter the 2FA code", "Done, hand back", "Paused, waiting for you")
	want(t, f.get("/profiles/github"), http.StatusOK, "Start browser", "This browser is stopped")
	want(t, f.get("/profiles/google/side"), http.StatusOK, "Agent log")
	want(t, f.get("/profiles/github/edit"), http.StatusOK, "Profile settings", "Time zone")
	want(t, f.get("/agents"), http.StatusOK, "research-bot", "bp_live_…", "Quick start")
	want(t, f.get("/settings"), http.StatusOK, "Handoff PIN", "Not set", "Memory limit", "50%")
	want(t, f.get("/chip"), http.StatusOK, "1 needs you")
	if r := f.get("/profiles/nope"); r.code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", r.code)
	}

	// Pages carry a CSP that allows no inline script.
	if csp := f.get("/").header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP %q", csp)
	}
	r := f.get("/static/app.css")
	want(t, r, http.StatusOK, "--blue:#2F6BFF")
	want(t, f.get("/static/htmx.min.js"), http.StatusOK)
}

func TestSignedOutIsSentToLogin(t *testing.T) {
	f := newFixture(t)
	f.client.Jar, _ = cookiejar.New(nil)
	for _, p := range []string{"/", "/profiles", "/agents", "/settings", "/profiles/x/preview.png"} {
		if r := f.get(p); r.code != http.StatusSeeOther || !strings.HasPrefix(r.location, "/login") {
			t.Fatalf("%s signed out: %d %s", p, r.code, r.location)
		}
	}
}

func TestCreateProfile(t *testing.T) {
	f := newFixture(t)
	// Settings' defaults apply.
	if err := f.settings.Save(context.Background(), settings.Settings{DefaultPolicy: profiles.PolicySignIn, DefaultAutoStop: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	want(t, f.get("/profiles/new"), http.StatusOK, `value="signin" checked`)

	r := f.post("/profiles", url.Values{"name": {"Bad Name"}, "policy": {"pin"}}, "HX-Request", "true")
	want(t, r, http.StatusUnprocessableEntity, "<dialog", `class="errors"`)

	r = f.post("/profiles", url.Values{"name": {"amazon"}, "policy": {"pin"}, "mode": {"api"}, "auto_stop": {"30"}, "screen": {"1920x1080"},
		"proxy": {"socks5://u:secret@proxy.example:1080"}}, "HX-Request", "true")
	if r.code != http.StatusNoContent || r.header.Get("HX-Redirect") != "/profiles" {
		t.Fatalf("create: %d %v", r.code, r.header)
	}
	p, err := f.m.Profiles().Get(context.Background(), "amazon")
	if err != nil || p.HandoffPolicy != profiles.PolicyPIN || p.DefaultMode != profiles.ModeAPI || p.AutoStop != 30*time.Minute || p.Width != 1920 || p.Proxy != "socks5://u:secret@proxy.example:1080" {
		t.Fatalf("created %+v, %v", p, err)
	}
	// The proxy password never reaches the page, and saving the masked
	// value keeps it.
	edit := f.get("/profiles/amazon/edit")
	want(t, edit, http.StatusOK, "socks5://u:••••@proxy.example:1080")
	if strings.Contains(edit.body, "secret") {
		t.Fatal("the edit page shows the proxy password")
	}
	r = f.post("/profiles/amazon/edit", url.Values{"policy": {"signin"}, "proxy": {"socks5://u:••••@proxy.example:1080"}, "time_zone": {"Europe/Berlin"}, "locale": {"de-DE"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.code, r.body)
	}
	p, _ = f.m.Profiles().Get(context.Background(), "amazon")
	if p.Proxy != "socks5://u:secret@proxy.example:1080" || p.HandoffPolicy != profiles.PolicySignIn || p.TimeZone != "Europe/Berlin" {
		t.Fatalf("saved %+v", p)
	}

	// "Start it now" sends you to the live view, which starts the browser.
	r = f.post("/profiles", url.Values{"name": {"bank"}, "start": {"1"}})
	if r.code != http.StatusSeeOther || r.location != "/profiles/bank?start=1" {
		t.Fatalf("create and start: %d %s", r.code, r.location)
	}
	want(t, f.get(r.location), http.StatusOK, "data-autosubmit")
}

func TestStartStopDelete(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	r := f.post("/profiles/google/start", url.Values{"back": {"/profiles/google"}})
	if r.code != http.StatusSeeOther || r.location != "/profiles/google" {
		t.Fatalf("start: %d %s", r.code, r.location)
	}
	if s, ok := f.m.Live("google"); !ok || s.State != sessions.Running {
		t.Fatalf("not running: %+v", s)
	}
	// The flash shows once.
	want(t, f.get("/profiles"), http.StatusOK, "google started")
	if strings.Contains(f.get("/profiles").body, "google started") {
		t.Fatal("the flash showed twice")
	}
	// Somewhere off-site isn't a place to go back to.
	if r := f.post("/profiles/google/stop", url.Values{"back": {"//evil.example/"}}); r.location != "/profiles" {
		t.Fatalf("stop went back to %q", r.location)
	}
	if _, ok := f.m.Live("google"); ok {
		t.Fatal("still running")
	}
	f.post("/profiles/google/start", nil)
	if r := f.post("/profiles/google/delete", nil); r.code != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.code)
	}
	if _, err := f.m.Profiles().Get(context.Background(), "google"); err == nil {
		t.Fatal("not deleted")
	}
}

func TestPreviewDoesNotCountAsUse(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	if r := f.get("/profiles/google/preview.png"); r.code != http.StatusNotFound {
		t.Fatalf("preview of a stopped browser: %d", r.code)
	}
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	r := f.get("/profiles/google/preview.png")
	if r.code != http.StatusOK || r.header.Get("Content-Type") != "image/png" || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("preview: %d %v", r.code, r.header)
	}
	if got := f.m.Activity(s.ID); len(got) != 0 {
		t.Fatalf("a preview shows in the agent log: %+v", got)
	}
}

func TestHandBackFinishesTheHandoff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.profile("google")
	s, _, _ := f.m.Start(ctx, "google", "")
	h, _, err := f.handoffs.Create(ctx, s.ID, "research-bot", handoff.SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}
	r := f.post("/profiles/google/handback", url.Values{"status": {"cant"}, "note": {"no phone here"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("hand back: %d", r.code)
	}
	got, _ := f.handoffs.Get(ctx, h.ID)
	if got.Status != handoff.Cant || got.Note != "no phone here" {
		t.Fatalf("handoff %+v", got)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running {
		t.Fatalf("browser not given back: %s", s.State)
	}
}

func TestTokens(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	r := f.post("/agents/tokens", url.Values{"name": {""}}, "HX-Request", "true")
	want(t, r, http.StatusUnprocessableEntity, "Give the token a name")
	r = f.post("/agents/tokens", url.Values{"name": {"claude"}, "scope": {"some"}, "profile": {"google", "nope"}}, "HX-Request", "true")
	want(t, r, http.StatusOK, "Copy your token now", auth.TokenPrefix)
	toks, _ := f.tokens.List(context.Background())
	if len(toks) != 1 || toks[0].AllProfiles || len(toks[0].Profiles) != 1 || toks[0].Profiles[0] != "google" {
		t.Fatalf("tokens %+v", toks)
	}
	// The secret is never shown again.
	secret := regexp.MustCompile(`<code>(bp_live_[^<]+)</code>`).FindStringSubmatch(r.body)
	if secret == nil || strings.Contains(f.get("/agents").body, secret[1]) {
		t.Fatalf("the token isn't shown once: %v", secret)
	}
	if r := f.post("/agents/tokens/"+toks[0].ID+"/revoke", nil); r.code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", r.code)
	}
	if toks, _ := f.tokens.List(context.Background()); len(toks) != 0 {
		t.Fatal("not revoked")
	}
}

func TestSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.post("/settings", url.Values{"policy": {"signin"}, "auto_stop": {"30"}, "ram": {"60"}, "proxy": {"http://proxy.example:3128"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.code, r.body)
	}
	st, _ := f.settings.Get(ctx)
	if st.DefaultPolicy != profiles.PolicySignIn || st.DefaultAutoStop != 30*time.Minute || st.RAMLimitPercent != 60 || st.DefaultProxy != "http://proxy.example:3128" || f.ram != 60 {
		t.Fatalf("settings %+v, ram applied %d", st, f.ram)
	}
	want(t, f.post("/settings", url.Values{"ram": {"5"}}), http.StatusUnprocessableEntity, "between 10% and 100%")

	// Setting the PIN the first time needs no current PIN; changing it does.
	if r := f.post("/settings/pin", url.Values{"pin": {"4821"}}); r.code != http.StatusSeeOther {
		t.Fatalf("set PIN: %d %s", r.code, r.body)
	}
	want(t, f.post("/settings/pin", url.Values{"current": {"0000"}, "pin": {"1234"}}), http.StatusUnprocessableEntity, "isn&#39;t right")
	if r := f.post("/settings/pin", url.Values{"current": {"4821"}, "pin": {"1234"}}); r.code != http.StatusSeeOther {
		t.Fatalf("change PIN: %d", r.code)
	}
	want(t, f.post("/settings/password", url.Values{"current": {"wrong"}, "password": {"a-new-password"}, "confirm": {"a-new-password"}}),
		http.StatusUnprocessableEntity, "set by BROWSERPIER_ADMIN_PASSWORD") // the fixture sets it from the environment
}

func TestFormatting(t *testing.T) {
	for in, out := range map[string]string{"research-bot": "RB", "claude": "C", "linkedin_poster": "LP", "": ""} {
		if got := initials(in); got != out {
			t.Errorf("initials(%q) = %q, want %q", in, got, out)
		}
	}
	for n, s := range map[int64]string{0: "0 MB", 512 << 20: "512 MB", 3 << 29: "1.5 GB"} {
		if got := formatBytes(n); got != s {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, s)
		}
	}
	if got := maskProxy("socks5://u:p@h:1"); got != "socks5://u:••••@h:1" {
		t.Errorf("maskProxy = %q", got)
	}
}
