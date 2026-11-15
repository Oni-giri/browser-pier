package ui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
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
	web      *auth.Web
	tokens   *auth.Tokens
	settings *settings.Store
	srv      *httptest.Server
	client   *http.Client
	ram      int
	free     int64 // what the host has available, for HostFree
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
	ctl.HandleFunc("GET /proxy/stats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"blocked": 42}`)
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
	// A list downloaded, so sessions of blocking profiles block.
	m.SetBlocklist(func() []byte { return []byte("# browserpier domain set 1\nads.example.com\n") })
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(d)
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	f := &fixture{t: t, m: m, handoffs: handoff.New(d, m, log), web: web, tokens: tokens, settings: settings.New(d)}
	app := New(m, web, f.handoffs, f.settings, log, Options{HostMemory: 16 << 30, HostFree: func() int64 { return f.free }, SetRAMLimit: func(p int) { f.ram = p }})
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
	want(t, f.get("/agents"), http.StatusOK, "No agent tokens yet", "It works with the REST API, CDP and MCP.")
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
	want(t, f.get("/profiles/google"), http.StatusOK, "<dt>Ads and trackers</dt><dd class=\"num\">42 blocked</dd>")
	want(t, f.get("/profiles/github"), http.StatusOK, "<dt>Ads and trackers</dt><dd>On</dd>")
	want(t, f.get("/profiles/github"), http.StatusOK, "Start browser", "This browser is stopped",
		`data-dialog="export-dialog" data-name="github"`, `data-dialog="delete-dialog" data-name="github"`)
	if r := f.get("/profiles/google"); !strings.Contains(r.body, `href="/profiles/google/edit"`) || !strings.Contains(r.body, `data-dialog="delete-dialog"`) {
		t.Fatal("a running profile's page has no menu with Edit settings and Delete")
	} else if strings.Contains(r.body, "Copy CDP URL") {
		t.Fatal("the profile page's menu repeats its Connection card")
	}
	want(t, f.get("/profiles/google/side"), http.StatusOK, "Agent log")
	want(t, f.get("/profiles/github/edit"), http.StatusOK, "Profile settings", "Time zone")
	want(t, f.get("/agents"), http.StatusOK, "research-bot", "bp_live_…", "Quick start")
	// The Agent tab's message, shown and copied, points to this BrowserPier's
	// setup guide and keeps the token in PIER_TOKEN.
	msg := "read " + f.srv.URL + "/mcp/setup (with curl if you can) and follow it. My agent token is in the PIER_TOKEN environment variable."
	if body := f.get("/agents").body; strings.Count(body, msg) != 2 || !strings.Contains(body, `data-tab="Agent"`) || !strings.Contains(body, "Copy message") {
		t.Fatal("the Agent tab's message isn't shown and copyable")
	}
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

	// Pages link their CSS and scripts by version, which browsers keep.
	link := regexp.MustCompile(`href="(/static/app\.css\?v=[0-9a-f]{12})"`).FindStringSubmatch(f.get("/").body)
	if link == nil {
		t.Fatal("the page doesn't link app.css by version")
	}
	if r := f.get(link[1]); r.code != http.StatusOK || r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned app.css: %d %q", r.code, r.header.Get("Cache-Control"))
	}
	if r := f.get("/static/app.css", "If-None-Match", f.get("/static/app.css").header.Get("ETag")); r.code != http.StatusNotModified {
		t.Fatalf("revalidating app.css: %d", r.code)
	}
	if r := f.get("/static/not-yet.js"); r.code != http.StatusNotFound || strings.Contains(r.header.Get("Cache-Control"), "immutable") {
		t.Fatalf("a missing file: %d %q", r.code, r.header.Get("Cache-Control"))
	}
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
	if err != nil || p.HandoffPolicy != profiles.PolicyPIN || p.DefaultMode != profiles.ModeAPI || p.AutoStop != 30*time.Minute || p.Width != 1920 || p.Proxy != "socks5://u:secret@proxy.example:1080" || p.LocalNetwork {
		t.Fatalf("created %+v, %v", p, err)
	}
	// The proxy password never reaches the page, and saving the masked
	// value keeps it.
	edit := f.get("/profiles/amazon/edit")
	want(t, edit, http.StatusOK, "socks5://u:••••@proxy.example:1080")
	if strings.Contains(edit.body, "secret") {
		t.Fatal("the edit page shows the proxy password")
	}
	r = f.post("/profiles/amazon/edit", url.Values{"policy": {"signin"}, "proxy": {"socks5://u:••••@proxy.example:1080"}, "time_zone": {"Europe/Berlin"}, "locale": {"de-DE"},
		"local_network": {"1"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.code, r.body)
	}
	p, _ = f.m.Profiles().Get(context.Background(), "amazon")
	if p.Proxy != "socks5://u:secret@proxy.example:1080" || p.HandoffPolicy != profiles.PolicySignIn || p.TimeZone != "Europe/Berlin" || !p.LocalNetwork {
		t.Fatalf("saved %+v", p)
	}
	want(t, f.get("/profiles/amazon/edit"), http.StatusOK, `name="local_network" value="1" checked`)
	// Unticked, the box sends nothing, which turns it off.
	f.post("/profiles/amazon/edit", url.Values{"policy": {"signin"}, "proxy": {"socks5://u:••••@proxy.example:1080"}})
	if p, _ = f.m.Profiles().Get(context.Background(), "amazon"); p.LocalNetwork {
		t.Fatal("unticking the local network kept it on")
	}

	// Ad and tracker blocking: off here, from the saved defaults; ticked on.
	if p.BlockAds {
		t.Fatal("the settings' default (off) didn't apply")
	}
	f.post("/profiles/amazon/edit", url.Values{"policy": {"signin"}, "proxy": {"socks5://u:••••@proxy.example:1080"}, "block_ads_field": {"1"}, "block_ads": {"1"}})
	if p, _ = f.m.Profiles().Get(context.Background(), "amazon"); !p.BlockAds {
		t.Fatal("ticking blocking left it off")
	}
	want(t, f.get("/profiles/amazon/edit"), http.StatusOK, `name="block_ads" value="1" checked`)

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

// A start refused because the host is short says so, and why.
func TestStartRefusedWhenTheHostIsShort(t *testing.T) {
	f := newFixture(t)
	f.m.SetHostFree(func() (int64, error) { return 1000 << 20, nil }) // 0.98 GB, shown as 0.9
	f.profile("google")
	f.post("/profiles/google/start", nil)
	if _, ok := f.m.Live("google"); ok {
		t.Fatal("started with the host short of memory")
	}
	want(t, f.get("/profiles"), http.StatusOK, "Not enough memory to start google: the host has 0.9 GB free, other apps included, and a browser needs about 1.0 GB, with no browser running to stop.")
}

// With no memory limit, pages count room in what the host has free and
// show no limit.
func TestNoLimitShowsTheHostsRoom(t *testing.T) {
	f := newFixture(t)
	f.m.SetMemoryLimit(0, 1<<30)
	f.free = 3 << 30
	f.profile("google")
	for _, path := range []string{"/", "/profiles", "/settings"} {
		r := f.get(path)
		want(t, r, http.StatusOK)
		if strings.Contains(r.body, "limit 0.0 GB") || strings.Contains(r.body, "of the 0.0 GB") {
			t.Errorf("%s shows a 0 GB limit", path)
		}
	}
	f.post("/profiles/google/start", nil)
	want(t, f.get("/profiles"), http.StatusOK, "1 running · ", "room for about 3 more")
}

// The setup guide needs no sign-in: agents read it before they're set up,
// and it holds no secret.
func TestSetupGuide(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/mcp/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("GET /mcp/setup: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, s := range []string{
		"claude mcp add --transport http browserpier " + f.srv.URL + "/mcp --header 'Authorization: Bearer ${PIER_TOKEN}'",
		"codex mcp add browserpier --url " + f.srv.URL + "/mcp --bearer-token-env-var PIER_TOKEN",
		"curl -s -X POST " + f.srv.URL + "/api/sessions",
		`"screenshot": true}' -o screen.png`, // a PNG, not JSON, for the terminal
	} {
		if !strings.Contains(body, s) {
			t.Errorf("the guide is missing %q", s)
		}
	}
	if strings.Contains(body, "{{") || strings.Contains(body, "bp_live_") {
		t.Error("the guide has a template left in it, or a token")
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
	// Ad and tracker blocking for new profiles: on until unticked; a form
	// without the box leaves it.
	want(t, f.get("/settings"), http.StatusOK, `name="block_ads" value="1" form="defaults" checked`)
	if st, _ = f.settings.Get(ctx); !st.DefaultBlockAds {
		t.Fatal("a form without the box turned blocking off")
	}
	f.post("/settings", url.Values{"block_ads_field": {"1"}})
	if st, _ = f.settings.Get(ctx); st.DefaultBlockAds {
		t.Fatal("unticking the box kept blocking on")
	}
	f.post("/settings", url.Values{"block_ads_field": {"1"}, "block_ads": {"1"}})
	if st, _ = f.settings.Get(ctx); !st.DefaultBlockAds {
		t.Fatal("ticking the box left blocking off")
	}

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

func TestExportAndImport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := profiles.New("google")
	p.DefaultMode, p.Width, p.Height = profiles.ModeAPI, 1280, 800
	p.Site, p.IconGlyph, p.IconColor = "mail.google.com", "envelope", "sky"
	if _, err := f.m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	cookies := filepath.Join(f.m.Layout().ProfileDir("google"), "chrome", "Default", "Cookies")
	if err := os.MkdirAll(filepath.Dir(cookies), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cookies, []byte("session=abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	const pass = "correct horse battery"

	if r := f.post("/profiles/google/export", url.Values{"passphrase": {pass}, "confirm": {"different one"}}); r.code != http.StatusSeeOther {
		t.Fatalf("mismatched passphrases: %d", r.code)
	}
	if _, _, err := f.m.Start(ctx, "google", ""); err != nil {
		t.Fatal(err)
	}
	if r := f.post("/profiles/google/export", url.Values{"passphrase": {pass}, "confirm": {pass}}); r.code != http.StatusSeeOther || r.header.Get("Content-Disposition") != "" {
		t.Fatalf("export while running: %d", r.code)
	}
	s, _ := f.m.Live("google")
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	f.writeIcon("google")
	exp := f.post("/profiles/google/export", url.Values{"passphrase": {pass}, "confirm": {pass}})
	if exp.code != http.StatusOK || !strings.Contains(exp.header.Get("Content-Disposition"), "attachment") || strings.Contains(exp.body, "session=abc") {
		t.Fatalf("export: %d %v", exp.code, exp.header)
	}

	imp := func(name, passphrase string) reply {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("name", name)
		_ = mw.WriteField("passphrase", passphrase)
		fw, _ := mw.CreateFormFile("archive", "google.browserpier.age")
		_, _ = fw.Write([]byte(exp.body))
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/profiles/import", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return reply{code: resp.StatusCode, location: resp.Header.Get("Location")}
	}
	if r := imp("google-copy", "not the passphrase"); r.location != "/profiles" {
		t.Fatalf("wrong passphrase: %d %s", r.code, r.location)
	}
	if r := imp("google", pass); r.location != "/profiles" {
		t.Fatalf("a name that's taken: %s", r.location)
	}
	if r := imp("google-copy", pass); r.code != http.StatusSeeOther || r.location != "/profiles/google-copy" {
		t.Fatalf("import: %d %s", r.code, r.location)
	}
	copied, err := f.m.Profiles().Get(ctx, "google-copy")
	if err != nil || copied.DefaultMode != profiles.ModeAPI || copied.Width != 1280 ||
		copied.Site != "mail.google.com" || copied.IconGlyph != "envelope" || copied.IconColor != "sky" {
		t.Fatalf("imported profile %+v, %v", copied, err)
	}
	if f.get("/profiles/google-copy/icon").code != http.StatusOK {
		t.Fatal("the icon didn't come with the import")
	}
	b, err := os.ReadFile(filepath.Join(f.m.Layout().ProfileDir("google-copy"), "chrome", "Default", "Cookies"))
	if err != nil || string(b) != "session=abc" {
		t.Fatalf("imported cookies %q, %v", b, err)
	}
	left, _ := filepath.Glob(filepath.Join(f.m.Layout().DataDir, "profiles", sessions.ImportPrefix+"*"))
	if len(left) != 0 {
		t.Fatalf("failed imports left folders: %v", left)
	}
}

func TestMemoryCountsOtherApps(t *testing.T) {
	f := newFixture(t)
	// Nothing known about other apps: the limit alone, 50% of 16 GB.
	want(t, f.get("/settings"), http.StatusOK, `<b id="ram-fits">8</b>`)
	if strings.Contains(f.get("/").body, "Other apps") {
		t.Fatal("other apps shown without knowing the host's free memory")
	}
	// Other apps leave 3 GB: three browsers fit, whatever the limit says.
	f.free = 3 << 30
	want(t, f.get("/settings"), http.StatusOK, `<b id="ram-fits">3</b>`, "Other apps <b>13.0 GB</b>", `data-room="3221225472"`)
	want(t, f.get("/"), http.StatusOK, "Other apps", `class="others"`)
	// The profiles list says the same, once a browser runs.
	f.profile("google")
	if _, _, err := f.m.Start(context.Background(), "google", ""); err != nil {
		t.Fatal(err)
	}
	want(t, f.get("/profiles"), http.StatusOK, "room for about 3 more")
	// Plenty free: the limit is what counts again.
	f.free = 15 << 30
	want(t, f.get("/settings"), http.StatusOK, `<b id="ram-fits">8</b>`)
}

func TestMemoryRoom(t *testing.T) {
	gb := int64(1 << 30)
	for _, c := range []struct {
		limit, used, free int64
		room              int
		freeBound         bool
	}{
		{limit: 10 * gb, used: 2 * gb, room: 8},                                // free unknown
		{limit: 10 * gb, used: 2 * gb, free: 20 * gb, room: 8},                 // the limit holds it
		{limit: 10 * gb, used: 2 * gb, free: 3 * gb, room: 3, freeBound: true}, // other apps do
		{free: 5 * gb, room: 5, freeBound: true},                               // no limit
		{room: -1},
	} {
		mv := memoryView{MemoryReport: sessions.MemoryReport{Limit: c.limit, Used: c.used, Each: gb}, Free: c.free}
		if mv.Room() != c.room || mv.FreeBound() != c.freeBound {
			t.Errorf("%+v: room %d, free-bound %v", c, mv.Room(), mv.FreeBound())
		}
	}
}

func TestOrangeOnlyWhenSomeoneIsNeeded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, name := range []string{"google", "notion", "github"} {
		f.profile(name)
	}
	for _, path := range []string{"/", "/profiles"} {
		if r := f.get(path); strings.Contains(r.body, "has-help") || strings.Contains(r.body, "emph") {
			t.Fatalf("%s is warm with nobody needed", path)
		}
	}
	var open []handoff.Handoff
	for _, name := range []string{"google", "notion"} {
		s, _, err := f.m.Start(ctx, name, "")
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := f.handoffs.Create(ctx, s.ID, "research-bot", handoff.SensitiveInput, "Enter the 2FA code", "")
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, h)
	}
	// Two need you: the page is warm, and only one card is emphasised.
	r := f.get("/")
	want(t, r, http.StatusOK, `<body class="has-help">`, `class="cell emph`, `<li class="help">`)
	if n := strings.Count(r.body, " emph"); n != 1 {
		t.Fatalf("overview has %d emphasis cards", n)
	}
	r = f.get("/profiles")
	want(t, r, http.StatusOK, `class="card help emph"`, `class="card help"`)
	if n := strings.Count(r.body, " emph"); n != 1 {
		t.Fatalf("profiles has %d emphasis cards", n)
	}
	want(t, f.get("/chip"), http.StatusOK, `class="chip-help"`, "2 need you")
	if _, err := f.handoffs.Withdraw(ctx, open[0], ""); err != nil {
		t.Fatal(err)
	}
	want(t, f.get("/chip"), http.StatusOK, "1 needs you")
	// Back to nobody: the chip, which app.js reads to turn the light off, goes.
	if _, err := f.handoffs.Withdraw(ctx, open[1], ""); err != nil {
		t.Fatal(err)
	}
	if r := f.get("/chip"); r.code != http.StatusOK || strings.Contains(r.body, "chip-help") {
		t.Fatalf("the chip stays with nobody needed: %d %q", r.code, r.body)
	}
	if r := f.get("/"); strings.Contains(r.body, "has-help") || strings.Contains(r.body, " emph") {
		t.Fatal("the overview stays warm with nobody needed")
	}
	for _, grain := range []string{"grain.svg", "grain-dark.svg"} {
		want(t, f.get("/static/"+grain), http.StatusOK, "feTurbulence")
		if r := f.get("/static/" + grain); r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%s: %q", grain, r.header.Get("Cache-Control"))
		}
	}
}
