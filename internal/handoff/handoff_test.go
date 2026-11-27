package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pionwebrtc "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/picker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
	"github.com/Oni-giri/browser-pier/internal/theme"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

type fixture struct {
	t     *testing.T
	m     *sessions.Manager
	svc   *Service
	admin *auth.Admin
	srv   *httptest.Server
	// cdp is the browser's CDP, which an until_url watch asks for its
	// tabs; tabs is what its Target.getTargets answers (watch_test.go).
	cdp  *cdptest.Browser
	tabs *fakeTabs
	// emptied counts the browser's clipboard emptyings, with whether a
	// person still had control as each happened (PROJ-194).
	emptied *clipRecord
	// onLift, if set, runs as the browser is asked to let go of held
	// keys and buttons (a hand-back).
	onLift *atomic.Pointer[func()]
	// choosers is the browsers' file choosers (chooser_test.go), files
	// the profiles' files, with a 1 MiB cap.
	choosers *fakeChoosers
	files    *files.Store
}

type clipRecord struct {
	mu       sync.Mutex
	controls []bool
}

func (c *clipRecord) took() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.controls
	c.controls = nil
	return out
}

func newFixture(t *testing.T, policy profiles.HandoffPolicy, opts auth.WebOptions) *fixture {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "ho")
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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	tabs := &fakeTabs{}
	tabs.set(tab{ID: "T1", Typ: "page", URL: "https://accounts.google.com/signin/v2?continue=https%3A%2F%2Fevil.example%2Fpay+now"})
	mux.HandleFunc("GET /cdp/json/list", tabs.serveList)
	emptied := &clipRecord{}
	var m *sessions.Manager
	mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, _ *http.Request) {
		s, _ := m.Live("google")
		emptied.mu.Lock()
		emptied.controls = append(emptied.controls, s.HumanControl)
		emptied.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	onLift := &atomic.Pointer[func()]{}
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		if f := onLift.Load(); f != nil {
			(*f)()
		}
		sessionstest.AnswerLift(w, r)
	})
	choosers := &fakeChoosers{}
	choosers.register(mux)
	// No CDP: what the handoff reads of the browser, it reads over HTTP.
	b := cdptest.New(func(m cdptest.Message) (any, error) {
		return nil, cdptest.Error{Message: "not in the fake: " + m.Method}
	})
	rt.Control = b.Handler(mux)
	m, err = sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	p := profiles.New("google")
	p.HandoffPolicy = policy
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.SetPIN(ctx, "4821"); err != nil {
		t.Fatal(err)
	}
	if opts.CookieSecure == "" {
		opts.CookieSecure = "auto"
	}
	web := auth.NewWeb(admin, auth.NewTokens(d), log, opts)
	svc := New(d, m, log)
	turn := webrtc.TURN{URLs: []string{"turn:pier.example.com:3478"}, Secret: testTURNSecret}
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{"127.0.0.1"}, TURN: turn}, m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	hmux := http.NewServeMux()
	web.Register(hmux)
	fs := files.New(m.Layout().FilesDir, 1<<20, os.Getuid(), os.Getgid())
	hw := NewWeb(svc, web, webrtc.NewHandler(relay, m, web, log), m, log)
	hw.SetPicker(picker.New(m, fs, log))
	hw.Register(hmux)
	// As in production, form posts from other origins are refused.
	srv := httptest.NewServer(new(http.CrossOriginProtection).Handler(hmux))
	t.Cleanup(srv.Close)
	t.Cleanup(svc.Close)
	return &fixture{t: t, m: m, svc: svc, admin: admin, srv: srv, cdp: b, tabs: tabs, emptied: emptied, onLift: onLift, choosers: choosers, files: fs}
}

func (f *fixture) running() sessions.Session {
	f.t.Helper()
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) open(require profiles.HandoffPolicy) (Handoff, string) {
	f.t.Helper()
	s := f.running()
	h, token, err := f.svc.Create(context.Background(), s.ID, "research-bot", SensitiveInput, "Enter the 2FA code", require)
	if err != nil {
		f.t.Fatal(err)
	}
	return h, token
}

func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type reply struct {
	code     int
	location string
	body     string
}

func (f *fixture) do(c *http.Client, method, path string, form url.Values, header http.Header) reply {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header.Get("Location"), string(b)}
}

func TestCreatePausesTheAgentAndShowsOnlyTheOrigin(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	if h.Origin != "https://accounts.google.com" {
		t.Fatalf("origin %q; the path and query must never reach the prompt", h.Origin)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.NeedsHelp {
		t.Fatalf("session %s, want needs_help", s.State)
	}
	if _, _, err := f.svc.Create(context.Background(), h.SessionID, "x", Review, "", ""); !errors.Is(err, ErrOpen) {
		t.Fatalf("second handoff = %v", err)
	}
	if len(token) < 40 {
		t.Fatalf("token %q is short", token)
	}
	p := f.do(browser(), "GET", "/h/"+token, nil, nil)
	if p.code != http.StatusOK || !strings.Contains(p.body, "research-bot needs you") || strings.Contains(p.body, "evil.example") || strings.Contains(p.body, "signin/v2") {
		t.Fatalf("PIN page %d: %s", p.code, p.body)
	}
}

func TestConcurrentCreatesOpenOneHandoff(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	s := f.running()
	errs := make(chan error, 8)
	for range cap(errs) {
		go func() {
			_, _, err := f.svc.Create(context.Background(), s.ID, "research-bot", Captcha, "", "")
			errs <- err
		}()
	}
	won := 0
	for range cap(errs) {
		switch err := <-errs; {
		case err == nil:
			won++
		case !errors.Is(err, ErrOpen):
			t.Fatalf("a losing Create: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent Creates won, want 1", won)
	}
}

func TestAgentsCanOnlyRaiseThePolicy(t *testing.T) {
	for _, c := range []struct{ profile, require, want profiles.HandoffPolicy }{
		{profiles.PolicyPIN, profiles.PolicyLink, profiles.PolicyPIN},
		{profiles.PolicyPIN, profiles.PolicySignIn, profiles.PolicySignIn},
		{profiles.PolicyLink, "", profiles.PolicyLink},
		{profiles.PolicySignIn, profiles.PolicyPIN, profiles.PolicySignIn},
	} {
		if got := Stricter(c.profile, c.require); got != c.want {
			t.Errorf("Stricter(%s, %s) = %s, want %s", c.profile, c.require, got, c.want)
		}
	}
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	if h, _ := f.open(profiles.PolicyLink); h.Policy != profiles.PolicyPIN {
		t.Fatalf("an agent lowered the policy to %s", h.Policy)
	}
}

func TestPINFlowHandsBack(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	h, token := f.open("")
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"1111"}}, nil); p.code != http.StatusUnauthorized || !strings.Contains(p.body, "4 tries left") {
		t.Fatalf("wrong PIN: %d %s", p.code, p.body)
	}
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusOK || !strings.Contains(p.body, "Done, hand back") {
		t.Fatalf("control page: %d", p.code)
	} else if tools := between(p.body, `class="ho-tools"`, "</div>"); !strings.Contains(tools, `id="who"`) || !strings.Contains(tools, `id="full"`) ||
		!strings.Contains(between(p.body, "data-viewer", `class="ho"`), `id="stage"`) {
		// The pill and the full-screen button in the tools, and the stream
		// in the viewer that goes full screen (PROJ-193).
		t.Errorf("the control page's tools: %s", tools)
	} else if !strings.Contains(tools, `id="who" class="pill you"`) {
		// Blue from the start, not grey until the script runs.
		t.Errorf("the pill isn't blue: %s", tools)
	} else {
		// Done and Can't do this are in the tools too, for full screen,
		// where the page's own form is hidden: they submit that form, note
		// and all (PROJ-193).
		for _, status := range []string{"done", "cant"} {
			if !strings.Contains(tools, `type="submit" form="finish" name="status" value="`+status+`"`) {
				t.Errorf("no %s button for full screen in the tools: %s", status, tools)
			}
		}
		if !strings.Contains(between(p.body, `<form method="post" action="/h/`+token+`/finish" id="finish">`, "</form>"), `name="note"`) {
			t.Error("the finish form lost its note")
		}
		// The clipboard box, in the tools and so in full screen too
		// (PROJ-194).
		if !strings.Contains(tools, `<button id="clip" class="btn small" type="button" aria-expanded="false" aria-controls="clipbox" hidden>Clipboard</button>`) ||
			!strings.Contains(between(p.body, "data-viewer", `id="stage"`), `<textarea id="cliptext"`) {
			t.Errorf("no clipboard box in the tools: %s", between(p.body, "data-viewer", `id="stage"`))
		}
	}
	// The link works once: another browser is refused, even with the PIN.
	other := browser()
	if p := f.do(other, "GET", "/h/"+token, nil, nil); p.code != http.StatusForbidden || !strings.Contains(p.body, "another device") {
		t.Fatalf("second browser: %d", p.code)
	}
	if p := f.do(other, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusForbidden {
		t.Fatalf("second browser with the PIN: %d", p.code)
	}
	if p := f.do(other, "POST", "/h/"+token+"/finish", url.Values{"status": {"done"}}, nil); p.code != http.StatusForbidden {
		t.Fatalf("second browser finishing: %d", p.code)
	}

	waited := make(chan Handoff, 1)
	go func() {
		got, _ := f.svc.Wait(ctx, h.ID, time.Minute)
		waited <- got
	}()
	if p := f.do(phone, "POST", "/h/"+token+"/finish", url.Values{"status": {"done"}, "note": {"used a backup code"}}, nil); p.code != http.StatusOK || !strings.Contains(p.body, "Handed back") {
		t.Fatalf("finish: %d %s", p.code, p.body)
	}
	select {
	case got := <-waited:
		if got.Status != Done || got.Note != "used a backup code" {
			t.Fatalf("waiter got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poll didn't wake")
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running || s.HumanControl {
		t.Fatalf("after hand-back: %s, human control %t", s.State, s.HumanControl)
	}
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusGone {
		t.Fatalf("link after hand-back: %d", p.code)
	}
}

func TestFiveWrongPINsCancel(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	var last reply
	for range MaxPINFailures {
		last = f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"0000"}}, nil)
	}
	if last.code != http.StatusGone || !strings.Contains(last.body, "cancelled") {
		t.Fatalf("fifth wrong PIN: %d %s", last.code, last.body)
	}
	got, _ := f.settled(h.ID)
	if got.Status != Cancelled || !strings.Contains(got.Note, "wrong PIN") {
		t.Fatalf("handoff %+v", got)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running {
		t.Fatalf("browser not given back: %s", s.State)
	}
	// Even the right PIN does nothing now.
	if p := f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusGone {
		t.Fatalf("right PIN after cancel: %d", p.code)
	}
}

func TestLinkOnlyAndSignIn(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	_, token := f.open("")
	phone := browser()
	// From a private address (here, loopback) one tap opens it.
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther {
		t.Fatalf("link only: %d", p.code)
	}
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusOK || !strings.Contains(p.body, "Done, hand back") {
		t.Fatalf("control page: %d", p.code)
	} else if !strings.Contains(p.body, `style="--ratio:1440/900"`) {
		t.Fatalf("the live view doesn't take the browser's shape:\n%s", p.body)
	}

	g := newFixture(t, profiles.PolicySignIn, auth.WebOptions{})
	_, token = g.open("")
	if p := g.do(browser(), "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther || p.location != "/login?next=%2Fh%2F"+token {
		t.Fatalf("full sign-in without a session: %d to %q", p.code, p.location)
	}

	// With two-step sign-in on (PROJ-162), the password alone doesn't
	// open a Full sign-in link; the code does, and the browser lands on it.
	secret := auth.NewTOTPSecret()
	if _, _, err := g.admin.EnableTwoStep(context.Background(), secret, auth.TOTPCode(secret, time.Now()), ""); err != nil {
		t.Fatal(err)
	}
	phone = browser()
	if p := g.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther || p.location != "/login?next=%2Fh%2F"+token {
		t.Fatalf("full sign-in, two-step on, fresh browser: %d to %q", p.code, p.location)
	}
	if p := g.do(phone, "POST", "/login", url.Values{"password": {"the-admin-password"}, "next": {"/h/" + token}}, nil); p.code != http.StatusSeeOther || p.location != "/login/code" {
		t.Fatalf("password step: %d to %q", p.code, p.location)
	}
	if p := g.do(phone, "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther || p.location != "/login?next=%2Fh%2F"+token {
		t.Fatalf("the password alone opened a Full sign-in link: %d to %q", p.code, p.location)
	}
	code := auth.TOTPCode(secret, time.Now().Add(30*time.Second)) // the confirming code's step is used
	if p := g.do(phone, "POST", "/login/code", url.Values{"code": {code}}, nil); p.code != http.StatusSeeOther || p.location != "/h/"+token {
		t.Fatalf("code step: %d to %q", p.code, p.location)
	}
	p := g.do(phone, "GET", "/h/"+token, nil, nil)
	for i := 0; p.code == http.StatusSeeOther && p.location == "/h/"+token && i < 3; i++ {
		p = g.do(phone, "GET", "/h/"+token, nil, nil)
	}
	if p.code != http.StatusOK || !strings.Contains(p.body, "Done, hand back") {
		t.Fatalf("after the code, the link: %d to %q", p.code, p.location)
	}
}

func TestPlainHTTPFromOutsideAsksForNothing(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("127.0.0.0/8")
	outside := http.Header{"X-Forwarded-For": {"203.0.113.5"}}
	for _, policy := range []profiles.HandoffPolicy{profiles.PolicyLink, profiles.PolicyPIN, profiles.PolicySignIn} {
		f := newFixture(t, policy, auth.WebOptions{TrustedProxies: []*net.IPNet{trusted}})
		h, token := f.open("")
		b := browser()
		p := f.do(b, "GET", "/h/"+token, nil, outside)
		if p.code != http.StatusForbidden || !strings.Contains(p.body, "needs HTTPS") || strings.Contains(p.body, "<form") {
			t.Fatalf("%s from outside over HTTP: %d %s", policy, p.code, p.body)
		}
		// Not even the right PIN, sent anyway, unlocks it or counts.
		if p := f.do(b, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, outside); p.code != http.StatusForbidden {
			t.Fatalf("%s: PIN from outside over HTTP: %d", policy, p.code)
		}
		if got, _ := f.svc.Get(context.Background(), h.ID); got.unlocked || got.PINFailures != 0 {
			t.Fatalf("%s: plain HTTP from outside unlocked the link or counted a try: %+v", policy, got)
		}
	}
	// Through an HTTPS proxy, the same address is fine.
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{TrustedProxies: []*net.IPNet{trusted}})
	_, token := f.open("")
	if p := f.do(browser(), "GET", "/h/"+token, nil, http.Header{"X-Forwarded-For": {"203.0.113.5"}, "X-Forwarded-Proto": {"https"}}); p.code != http.StatusSeeOther {
		t.Fatalf("link only from outside over HTTPS: %d", p.code)
	}
	g := newFixture(t, profiles.PolicyPIN, auth.WebOptions{TrustedProxies: []*net.IPNet{trusted}})
	_, token = g.open("")
	if p := g.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, http.Header{"X-Forwarded-For": {"203.0.113.5"}, "X-Forwarded-Proto": {"https"}}); p.code != http.StatusSeeOther {
		t.Fatalf("PIN from outside over HTTPS: %d", p.code)
	}
}

// Over plain HTTP on a LAN or Tailscale address, browsers send no
// Sec-Fetch-Site, so the cross-origin check compares Origin with Host. The
// page's referrer policy must let its PIN form post carry its origin.
func TestPINFormKeepsItsOriginOverPlainHTTP(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	phone := browser()
	resp, err := phone.Get(f.srv.URL + "/h/" + token)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
	if !strings.Contains(string(page), `<meta name="referrer" content="same-origin">`) || strings.Contains(string(page), "no-referrer") {
		t.Error("the handoff page's meta referrer isn't same-origin; under no-referrer its PIN form posts Origin: null")
	}
	for _, origin := range []string{"null", "http://elsewhere.example"} {
		if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, http.Header{"Origin": {origin}}); p.code != http.StatusForbidden {
			t.Fatalf("PIN with Origin %s: %d", origin, p.code)
		}
	}
	if got, _ := f.svc.Get(context.Background(), h.ID); got.unlocked || got.PINFailures != 0 {
		t.Fatalf("a refused cross-origin post unlocked the link or counted a try: %+v", got)
	}
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, http.Header{"Origin": {f.srv.URL}}); p.code != http.StatusSeeOther {
		t.Fatalf("PIN with the page's own origin: %d %s", p.code, p.body)
	}
}

func TestExpiry(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	later := time.Now().Add(Lifetime + time.Second)
	f.svc.now = func() time.Time { return later }
	got, err := f.settled(h.ID)
	if err != nil || got.Status != Expired {
		t.Fatalf("after 15 minutes: %+v, %v", got, err)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running {
		t.Fatalf("browser not given back after expiry: %s", s.State)
	}
	p := f.do(browser(), "GET", "/h/"+token, nil, nil)
	if p.code != http.StatusGone || !strings.Contains(p.body, "expired") || !strings.Contains(p.body, `href="/login"`) {
		t.Fatalf("expired link: %d %s", p.code, p.body)
	}
	if p := f.do(browser(), "GET", "/h/nonsense", nil, nil); p.code != http.StatusNotFound {
		t.Fatalf("unknown link: %d", p.code)
	}
}

func TestPINCantOpenAFullSignInLink(t *testing.T) {
	f := newFixture(t, profiles.PolicySignIn, auth.WebOptions{})
	h, token := f.open("")
	b := browser()
	if p := f.do(b, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("PIN on a sign-in link: %d", p.code)
	}
	if p := f.do(b, "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther || !strings.HasPrefix(p.location, "/login") {
		t.Fatalf("after the PIN, the link should still want a sign-in: %d to %q", p.code, p.location)
	}
	if got, _ := f.svc.Get(context.Background(), h.ID); got.unlocked {
		t.Fatal("the PIN unlocked a full sign-in link")
	}
}

func TestParallelWrongPINsStopAtFive(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"0000"}}, nil)
		}()
	}
	wg.Wait()
	got, _ := f.settled(h.ID)
	if got.PINFailures != MaxPINFailures || got.Status != Cancelled {
		t.Fatalf("after 25 parallel guesses: %d failures, %s", got.PINFailures, got.Status)
	}
}

func TestWaitSeesAFinishJustBeforeItListens(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	h, _ := f.open("")
	f.svc.beforeListen = func() { _, _ = f.svc.Finish(ctx, h, Done, "") }
	start := time.Now()
	got, err := f.svc.Wait(ctx, h.ID, 10*time.Second)
	if err != nil || got.Status != Done || time.Since(start) > 2*time.Second {
		t.Fatalf("Wait: %s, %v after %s", got.Status, err, time.Since(start))
	}
}

func TestSweepCancelsAHandoffWhoseBrowserStopped(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	h, _ := f.open("")
	waited := make(chan Handoff, 1)
	go func() {
		got, _ := f.svc.Wait(ctx, h.ID, time.Minute)
		waited <- got
	}()
	for listening := false; !listening; time.Sleep(10 * time.Millisecond) {
		f.svc.mu.Lock()
		listening = len(f.svc.waiters[h.ID]) > 0
		f.svc.mu.Unlock()
	}
	if _, err := f.m.Stop(ctx, h.SessionID); err != nil {
		t.Fatal(err)
	}
	f.svc.sweep(ctx) // each handoff's end on its own
	var status Status
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		err := f.svc.db.QueryRow(`SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&status)
		if err == nil && status == Cancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a sweep: %s, %v", status, err)
		}
	}
	select {
	case got := <-waited:
		if got.Status != Cancelled {
			t.Fatalf("waiter got %s", got.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the long-poll didn't wake when the browser stopped")
	}
}

func TestWrongPINsCountEvenWhenRecordingThemFails(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	h, _ := f.open("")
	reserve := func() bool {
		ok, err := f.svc.ReservePINTry(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// The client went away mid-check: the guess still counts.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if !reserve() {
		t.Fatal("first reservation refused")
	}
	if _, err := f.svc.PINWrong(gone, h); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(ctx, h.ID); got.PINFailures != 1 {
		t.Fatalf("a wrong PIN from a client that left: %d failures, want 1", got.PINFailures)
	}

	// The count can't be written: the try stays used up.
	if _, err := f.svc.db.Exec(`CREATE TRIGGER no_count BEFORE UPDATE OF pin_failures ON handoffs BEGIN SELECT RAISE(FAIL, 'disk on fire'); END`); err != nil {
		t.Fatal(err)
	}
	if !reserve() {
		t.Fatal("second reservation refused")
	}
	if _, err := f.svc.PINWrong(ctx, h); err == nil {
		t.Fatal("PINWrong didn't report the failed write")
	}
	if _, err := f.svc.db.Exec(`DROP TRIGGER no_count`); err != nil {
		t.Fatal(err)
	}
	for i := range MaxPINFailures - 2 {
		if !reserve() {
			t.Fatalf("reservation %d of the %d tries left refused", i+1, MaxPINFailures-2)
		}
	}
	if reserve() {
		t.Fatal("a guess whose count failed to write was given back")
	}
}

func TestParallelRightPINsDontCancel(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	codes := make(chan int, MaxPINFailures+1)
	var start sync.WaitGroup
	start.Add(1)
	for range cap(codes) {
		go func() {
			start.Wait()
			codes <- f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil).code
		}()
	}
	start.Done()
	unlocked := 0
	for range cap(codes) {
		switch c := <-codes; c {
		case http.StatusSeeOther:
			unlocked++
		case http.StatusForbidden, http.StatusTooManyRequests: // another browser got it, or wait
		default:
			t.Fatalf("a right PIN answered %d", c)
		}
	}
	got, _ := f.svc.Get(context.Background(), h.ID)
	if unlocked != 1 || got.Status != Pending || !got.unlocked || got.PINFailures != 0 {
		t.Fatalf("after %d parallel right PINs: %d unlocked, %+v", cap(codes), unlocked, got)
	}
}

func TestWrongPINsAcrossLinksAndProfilesAreCapped(t *testing.T) {
	_, local, _ := net.ParseCIDR("127.0.0.0/8")
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{TrustedProxies: []*net.IPNet{local}})
	ctx := context.Background()
	for _, name := range []string{"github", "bank", "mail"} {
		p := profiles.New(name)
		p.HandoffPolicy = profiles.PolicyPIN
		if _, err := f.m.Profiles().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	ip := 0
	guess := func(token, pin string) reply {
		ip++ // a new address every time, so only the link, profile and global caps apply
		return f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {pin}},
			http.Header{"X-Forwarded-For": {"203.0.113." + strconv.Itoa(ip)}, "X-Forwarded-Proto": {"https"}})
	}
	link := func(profile string) string {
		s, _, err := f.m.Start(ctx, profile, "")
		if err != nil {
			t.Fatal(err)
		}
		_, token, err := f.svc.Create(ctx, s.ID, "research-bot", SensitiveInput, "Enter the 2FA code", "")
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	burn := func(profile string) { // five wrong PINs, which cancel the link
		token := link(profile)
		for range MaxPINFailures {
			if r := guess(token, "0000"); r.code != http.StatusUnauthorized && r.code != http.StatusGone {
				t.Fatalf("%s: wrong PIN answered %d", profile, r.code)
			}
		}
	}

	// Three links for one profile use up its 15 tries; a fourth link
	// refuses even the right PIN.
	for range 3 {
		burn("google")
	}
	if r := guess(link("google"), "4821"); r.code != http.StatusTooManyRequests {
		t.Fatalf("16th try for one profile: %d", r.code)
	}

	// The PIN is shared, so 30 wrong tries across profiles lock every
	// profile's links, even one well under its own cap.
	for range 2 {
		burn("github")
	}
	if r := guess(link("bank"), "4821"); r.code != http.StatusSeeOther {
		t.Fatalf("right PIN on another profile at 25 wrong overall: %d", r.code)
	}
	burn("mail")
	if r := guess(link("github"), "4821"); r.code != http.StatusTooManyRequests {
		t.Fatalf("a try after 30 wrong overall: %d", r.code)
	}
}

func TestPendingAndStats(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	start := time.Now()
	h, _ := f.open("")
	open, err := f.svc.Pending(ctx)
	if err != nil || len(open) != 1 || open[0].ID != h.ID {
		t.Fatalf("pending: %v %+v", err, open)
	}
	f.svc.now = func() time.Time { return start.Add(48 * time.Second) }
	if _, err := f.svc.Finish(ctx, h, Done, ""); err != nil {
		t.Fatal(err)
	}
	if open, _ := f.svc.Pending(ctx); len(open) != 0 {
		t.Fatalf("still pending after done: %+v", open)
	}
	// A second one, left to expire, counts but has no time to hand back.
	f.svc.now = time.Now
	h2, _ := f.open("")
	f.svc.now = func() time.Time { return h2.ExpiresAt.Add(time.Second) }
	// Pending starts its end, listing it as it stands meanwhile; once
	// the end is over it is gone.
	_, _ = f.svc.Pending(ctx)
	if got, _ := f.settled(h2.ID); got.Status != Expired {
		t.Fatalf("an expired link: %s", got.Status)
	}
	if open, _ := f.svc.Pending(ctx); len(open) != 0 {
		t.Fatalf("an expired link is still pending: %+v", open)
	}
	n, median, err := f.svc.Stats(ctx, start.Add(-time.Minute))
	if err != nil || n != 2 || median < 47*time.Second || median > 49*time.Second {
		t.Fatalf("stats: %d, %s, %v", n, median, err)
	}
	// "Can't do this" is an answer too; with two answers, the median is
	// their mean.
	f.svc.now = time.Now
	h3, _ := f.open("")
	f.svc.now = func() time.Time { return h3.CreatedAt.Add(60 * time.Second) }
	if _, err := f.svc.Finish(ctx, h3, Cant, ""); err != nil {
		t.Fatal(err)
	}
	n, median, _ = f.svc.Stats(ctx, start.Add(-time.Minute))
	if n != 3 || median < 53*time.Second || median > 55*time.Second {
		t.Fatalf("stats with a can't: %d, %s", n, median)
	}
}

func TestHandoffCookieFollowsCookieSecure(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{CookieSecure: "always"})
	_, token := f.open("")
	resp, err := browser().Get(f.srv.URL + "/h/" + token) // plain HTTP, but the admin says always Secure
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var c *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == browserCookie {
			c = x
		}
	}
	if resp.StatusCode != http.StatusSeeOther || c == nil || !c.Secure {
		t.Fatalf("link only with CookieSecure=always: %d, cookie %+v", resp.StatusCode, c)
	}
}

// pionOffer is a viewer's offer, as the handoff page's script makes it.
func pionOffer(t *testing.T) string {
	t.Helper()
	pc, err := pionwebrtc.NewPeerConnection(pionwebrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(pionwebrtc.RTPCodecTypeVideo, pionwebrtc.RTPTransceiverInit{Direction: pionwebrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.CreateDataChannel("input", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := pionwebrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	b, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP})
	return string(b)
}

func TestReloadedHandoffPageGetsControlBack(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	_, token := f.open("")
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	offer := func() reply {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/h/"+token+"/offer", strings.NewReader(pionOffer(t)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := phone.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return reply{code: resp.StatusCode, body: string(b)}
	}
	for i := range 2 { // the page, then the same page reloaded
		r := offer()
		if r.code != http.StatusOK || !strings.Contains(r.body, `"control":true`) {
			t.Fatalf("offer %d: %d %s", i+1, r.code, r.body)
		}
	}
	if s, _ := f.m.Live("google"); !s.HumanControl {
		t.Fatal("the reloaded page doesn't hold control")
	}
}

func TestHandoffPageHasThePhonesTheme(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	_, token := f.open("")
	phone := browser()
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); !strings.Contains(p.body, `<html lang="en">`) || !strings.Contains(p.body, `content="light dark"`) {
		t.Fatalf("system: %.400s", p.body)
	}
	u, _ := url.Parse(f.srv.URL)
	phone.Jar.SetCookies(u, []*http.Cookie{{Name: theme.Cookie, Value: "dark", Path: "/"}})
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); !strings.Contains(p.body, `<html lang="en" data-theme="dark">`) || strings.Count(p.body, `name="theme-color"`) != 1 {
		t.Fatalf("dark: %.400s", p.body)
	}
	// The page offers no choice of its own.
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); strings.Contains(p.body, "appearance") {
		t.Fatal("the handoff page has a theme control")
	}
}

const testTURNSecret = "north-wind-secret-123"

// TURN credentials go only to the browser holding the link, while it's
// open, and run out shortly after the link does.
func TestTURNCredentialsOnlyForTheHolder(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	phone, other := browser(), browser()
	refused := func(c *http.Client, when string) {
		t.Helper()
		if p := f.do(c, "GET", "/h/"+token+"/ice", nil, nil); p.code != http.StatusForbidden || strings.Contains(p.body, "credential") {
			t.Fatalf("%s: %d %s", when, p.code, p.body)
		}
	}
	refused(phone, "before the PIN")
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d", p.code)
	}
	if p := f.do(phone, "GET", "/h/"+token, nil, nil); !strings.Contains(p.body, `data-ice="/h/`+token+`/ice"`) {
		t.Fatalf("the control page doesn't point the live view at its ICE servers: %.600s", p.body)
	}
	refused(other, "another browser")
	refused(browser(), "no cookie")

	p := f.do(phone, "GET", "/h/"+token+"/ice", nil, nil)
	var out struct {
		ICEServers []webrtc.ICEServer `json:"iceServers"`
	}
	if err := json.Unmarshal([]byte(p.body), &out); p.code != http.StatusOK || err != nil || len(out.ICEServers) != 1 || out.ICEServers[0].Credential == "" {
		t.Fatalf("the holder: %d %s", p.code, p.body)
	}
	if strings.Contains(p.body, testTURNSecret) {
		t.Fatal("the shared secret reached the phone")
	}
	exp, _, _ := strings.Cut(out.ICEServers[0].Username, ":")
	n, _ := strconv.ParseInt(exp, 10, 64)
	if until := time.Unix(n, 0); until.Before(h.ExpiresAt) || until.After(h.ExpiresAt.Add(2*time.Minute)) {
		t.Fatalf("credentials until %v for a link that expires at %v", until, h.ExpiresAt)
	}
	// Asking again gets the same TURN user, so the TURN server's per-user
	// quota holds the link however often its page reloads, and the user
	// says nothing of the link or the handoff.
	if again := f.do(phone, "GET", "/h/"+token+"/ice", nil, nil); !strings.Contains(again.body, `"username":"`+out.ICEServers[0].Username+`"`) {
		t.Fatalf("a second request got another TURN user: %s, then %s", out.ICEServers[0].Username, again.body)
	}
	if u := out.ICEServers[0].Username; strings.Contains(u, h.ID) || strings.Contains(u, token) {
		t.Fatalf("the TURN user %q names the handoff", u)
	}

	if p := f.do(phone, "POST", "/h/"+token+"/finish", url.Values{"status": {"done"}}, nil); p.code != http.StatusOK {
		t.Fatalf("finish: %d", p.code)
	}
	refused(phone, "after hand-back")
}

// With an until_url, the open handoff page tells the person it hands
// back by itself, quoting the agent's pattern escaped and shortened.
func TestHandoffPageSaysWhenItHandsBackByItself(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	_, token := f.openUntil(`/a<b>"c&d/` + strings.Repeat("x", 120))
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	p := f.do(phone, "GET", "/h/"+token, nil, nil)
	want := "This hands back by itself when the page reaches an address containing “/a&lt;b&gt;&#34;c&amp;d/" + strings.Repeat("x", 69) + "…”."
	if !strings.Contains(p.body, want) {
		t.Fatalf("the page doesn't say it hands back by itself, escaped and shortened:\n%s", p.body)
	}
	if strings.Contains(p.body, "<b>") {
		t.Fatal("the pattern isn't escaped")
	}

	// Without one, it says nothing of the kind.
	g := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	_, token = g.open("")
	phone = browser()
	if p := g.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	if p := g.do(phone, "GET", "/h/"+token, nil, nil); !strings.Contains(p.body, `id="finish"`) || strings.Contains(p.body, "hands back by itself") {
		t.Fatalf("without until_url: %s", p.body)
	}
}

// between is s from the first from to the next to after it, "" if either
// is missing.
func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		return ""
	}
	return s[i : i+j]
}

// However a handoff ends while the person has control (Done here, or it
// expires), the browser's clipboard is emptied while they still have it,
// before the agent gets the browser back (PROJ-194).
func TestHandoffEndEmptiesTheClipboard(t *testing.T) {
	for name, end := range map[string]func(f *fixture, h Handoff){
		"done": func(f *fixture, h Handoff) {
			if _, err := f.svc.Finish(context.Background(), h, Done, ""); err != nil {
				t.Fatal(err)
			}
		},
		"expiry": func(f *fixture, h Handoff) {
			later := time.Now().Add(Lifetime + time.Second)
			f.svc.now = func() time.Time { return later }
			if got, err := f.settled(h.ID); err != nil || got.Status != Expired {
				t.Fatalf("after 15 minutes: %+v, %v", got, err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
			h, _ := f.open("")
			if err := f.m.TakeControl(h.SessionID, sessions.HandoffHolder(h.ID)); err != nil {
				t.Fatal(err)
			}
			end(f, h)
			// The hand-back goes on by itself: wait for it.
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if s, _ := f.m.Live("google"); !s.HumanControl {
					break
				}
			}
			if got := f.emptied.took(); len(got) != 1 || !got[0] {
				t.Fatalf("emptied %d times, with control %v; want once, while the person had it", len(got), got)
			}
			if s, _ := f.m.Live("google"); s.HumanControl || s.State != sessions.Running {
				t.Errorf("after the handoff: %+v", s)
			}
		})
	}
}

// A handoff's offer that a stop and restart overtake (after the link was
// checked, before the live view connects) never takes control of the
// profile's new session: the link is for the session it was opened in,
// and the new one's agent keeps its browser (PROJ-196).
func TestHandoffOfferNeverTakesTheNextSession(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	offer := func() reply {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/h/"+token+"/offer", strings.NewReader(pionOffer(t)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := phone.Do(req)
		if err != nil {
			t.Error(err)
			return reply{}
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return reply{code: resp.StatusCode, body: string(b)}
	}
	if r := offer(); r.code != http.StatusOK || !strings.Contains(r.body, `"control":true`) {
		t.Fatalf("offer: %d %s", r.code, r.body)
	}
	// The page reloads: its new offer passes the link's checks, then
	// waits for the old view's hand-back, held up here.
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hold := func() { once.Do(func() { close(entered); <-gate }) }
	f.onLift.Store(&hold)
	defer close(gate)
	second := make(chan reply, 1)
	go func() { second <- offer() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reloaded page's offer never got to the old view's hand-back")
	}
	// Meanwhile the browser stops, and the profile starts again.
	if _, err := f.m.Stop(context.Background(), h.SessionID); err != nil {
		t.Fatal(err)
	}
	next := f.running()
	r := <-second
	if r.code == http.StatusOK {
		t.Errorf("the offer for the stopped session connected (%d)", r.code)
	}
	if s, _ := f.m.Live("google"); s.ID != next.ID || s.HumanControl {
		t.Fatalf("the new session: %s, person in control %v", s.ID, s.HumanControl)
	}
	if _, done, err := f.m.AgentInput(context.Background(), next.ID, "tok"); err != nil {
		t.Fatalf("the new session's agent = %v", err)
	} else {
		done()
	}
}

// A handoff that ends while its live view's offer is under way (after
// the link was checked, before control is taken) takes nothing: the
// browser is the agent's again, not the stale page's (PROJ-196).
func TestHandoffOfferAfterItsHandoffEndedTakesNothing(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	offer := func() reply {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/h/"+token+"/offer", strings.NewReader(pionOffer(t)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := phone.Do(req)
		if err != nil {
			t.Error(err)
			return reply{}
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return reply{code: resp.StatusCode, body: string(b)}
	}
	if r := offer(); r.code != http.StatusOK || !strings.Contains(r.body, `"control":true`) {
		t.Fatalf("offer: %d %s", r.code, r.body)
	}
	// The page reloads: its new offer passes the link's checks, then
	// waits for the old view's hand-back, held up here.
	entered, gate := make(chan struct{}), make(chan struct{})
	var once, opened sync.Once
	letGo := func() { opened.Do(func() { close(gate) }) }
	defer letGo()
	hold := func() { once.Do(func() { close(entered); <-gate }) }
	f.onLift.Store(&hold)
	second := make(chan reply, 1)
	go func() { second <- offer() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reloaded page's offer never got to the old view's hand-back")
	}
	// Meanwhile the handoff ends (Done, on another device).
	finished := make(chan error, 1)
	go func() {
		_, err := f.svc.Finish(context.Background(), h, Done, "")
		finished <- err
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s, _ := f.m.Live("google"); s.State == sessions.Running {
			break
		}
	}
	letGo()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if r := <-second; r.code == http.StatusOK {
		t.Errorf("the offer for the ended handoff connected (%d)", r.code)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running || s.HumanControl {
		t.Fatalf("after the handoff: %s, person in control %v", s.State, s.HumanControl)
	}
	if _, done, err := f.m.AgentInput(context.Background(), h.SessionID, "tok"); err != nil {
		t.Fatalf("the agent = %v", err)
	} else {
		done()
	}
}

// Ending a handoff no one opened is one step in the manager: an admin
// who takes control the moment the browser stops waiting for help keeps
// it; the handoff's end never reaches their takeover (PROJ-196).
func TestHandoffEndLeavesALaterTakeoverAlone(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	s := f.running()
	ctx := context.Background()
	for i := range 20 {
		h, _, err := f.svc.Create(ctx, s.ID, "research-bot", SensitiveInput, "Enter the 2FA code", "")
		if err != nil {
			t.Fatal(err)
		}
		// An admin on the profile page takes control as soon as the
		// browser runs again (Running, so this take is theirs, not the
		// handoff's helper's).
		took := make(chan bool, 1)
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					took <- false
					return
				default:
				}
				if sess, _ := f.m.Live("google"); sess.State == sessions.Running {
					if f.m.TakeControl(s.ID, "person") == nil {
						took <- true
						return
					}
				}
			}
		}()
		if _, err := f.svc.Finish(ctx, h, Done, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		close(stop)
		if <-took {
			if sess, _ := f.m.Live("google"); !sess.HumanControl {
				t.Fatalf("round %d: the admin's takeover, taken after the handoff's Needs help, was ended by the handoff", i)
			}
			f.m.ReleaseControl(s.ID)
		}
	}
}

// A browser that never answers its hand-back holds up no one else: the
// sweep that expires its handoff goes on to the next profile's at once,
// and that one's agent has its browser back (PROJ-196).
func TestWedgedBrowserDoesntHoldTheSweep(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	wedged, _ := f.open("")
	if err := f.m.TakeControl(wedged.SessionID, "person"); err != nil { // a person helps
		t.Fatal(err)
	}
	if _, err := f.m.Profiles().Create(ctx, profiles.New("github")); err != nil {
		t.Fatal(err)
	}
	other, _, err := f.m.Start(ctx, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	waiting, _, err := f.svc.Create(ctx, other.ID, "research-bot", SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}
	// The first browser never answers its lift.
	stuck := make(chan struct{})
	defer close(stuck)
	hold := func() { <-stuck }
	f.onLift.Store(&hold)

	later := time.Now().Add(Lifetime + time.Second)
	f.svc.now = func() time.Time { return later }
	swept := make(chan struct{})
	go func() { f.svc.sweep(ctx); close(swept) }()
	select {
	case <-swept:
	case <-time.After(3 * time.Second):
		t.Fatal("the sweep is held by a browser that doesn't answer")
	}
	// Listing the open handoffs neither waits for the wedged browser nor
	// fails for it: the wedged one is listed as it stands.
	start := time.Now()
	open, err := f.svc.Pending(ctx)
	if err != nil || !slices.ContainsFunc(open, func(h Handoff) bool { return h.ID == wedged.ID }) || time.Since(start) > 2*time.Second {
		t.Fatalf("Pending: %+v, %v after %s", open, err, time.Since(start))
	}
	// The other profile's handoff ends, on its own.
	if got, err := f.settled(waiting.ID); err != nil || got.Status != Expired {
		t.Fatalf("the other profile's handoff after the sweep: %s, %v", got.Status, err)
	}
	// The wedged one isn't told as ended while its browser isn't handed
	// back: a read answers at once that it is still pending, and a wait
	// answers that within its time.
	start = time.Now()
	got, err := f.svc.Get(ctx, wedged.ID)
	if got.Status != Pending || err != nil {
		t.Errorf("the wedged handoff: %s, %v; want it still pending", got.Status, err)
	}
	got, err = f.svc.Wait(ctx, wedged.ID, time.Second)
	if got.Status != Pending || err != nil {
		t.Errorf("waiting on the wedged handoff: %s, %v; want it still pending", got.Status, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("reading the wedged handoff took %s", d)
	}
	if s, _ := f.m.Live("github"); s.State != sessions.Running {
		t.Errorf("the other profile's browser: %s", s.State)
	}
	if _, done, err := f.m.AgentInput(ctx, other.ID, "tok"); err != nil {
		t.Errorf("the other profile's agent = %v", err)
	} else {
		done()
	}
}

// holdLifts makes the browser's lift wait until the returned function is
// called (or the test ends): the hand-back is under way meanwhile.
func holdLifts(t *testing.T, f *fixture) (letGo func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	letGo = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	hold := func() { <-gate }
	f.onLift.Store(&hold)
	return letGo
}

// Nothing tells anyone a handoff has ended before its browser is handed
// back: the end that asked waits for it (or, its request over, says it
// is still handing back), and anyone else looking meanwhile sees it
// pending, or, if their look would end it, also hears it is still
// handing back; once handed back, it is final and agents can act
// (PROJ-196).
func TestHandoffIsFinalOnlyOnceHandedBack(t *testing.T) {
	for _, how := range []string{"done", "expiry"} {
		t.Run(how, func(t *testing.T) {
			f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
			ctx := context.Background()
			h, _ := f.open("")
			if err := f.m.TakeControl(h.SessionID, "person"); err != nil { // a person helps
				t.Fatal(err)
			}
			letGo := holdLifts(t, f)
			want := Done
			ended := make(chan error, 1)
			if how == "done" {
				go func() {
					_, err := f.svc.Finish(ctx, h, Done, "")
					ended <- err
				}()
			} else {
				want = Expired
				later := time.Now().Add(Lifetime + time.Second)
				f.svc.now = func() time.Time { return later }
				go func() {
					// A read starts its end; the wait sees it over.
					got, err := f.svc.Wait(ctx, h.ID, time.Minute)
					if err == nil && !got.Status.Final() {
						err = errors.New("still " + string(got.Status))
					}
					ended <- err
				}()
			}
			for !f.svc.isEnding(h.ID) {
				time.Sleep(time.Millisecond)
			}
			// A request that gives up: told it is still handing back.
			short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			got, err := f.svc.Wait(short, h.ID, 0)
			cancel()
			if got.Status.Final() {
				t.Errorf("told %s mid-hand-back", got.Status)
			}
			if err != nil {
				t.Errorf("a look mid-hand-back = %v, want the handoff as it stands", err)
			}
			// An agent withdrawing it meanwhile joins the end under way, and
			// its request, given up, hears that it is still handing back.
			short, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
			got, err = f.svc.Withdraw(short, h, "")
			cancel()
			if got.Status.Final() || !errors.Is(err, ErrHandingBack) {
				t.Errorf("a withdrawal mid-hand-back: %s, %v; want ErrHandingBack", got.Status, err)
			}
			if _, done, err := f.m.AgentInput(ctx, h.SessionID, "tok"); !errors.Is(err, sessions.ErrHumanDriving) {
				if err == nil {
					done()
				}
				t.Fatalf("agent input mid-hand-back = %v", err)
			}
			select {
			case err := <-ended:
				t.Fatalf("the end returned (%v) before the hand-back", err)
			default:
			}
			letGo()
			if err := <-ended; err != nil {
				t.Fatal(err)
			}
			got, err = f.svc.Wait(ctx, h.ID, 0)
			if err != nil || got.Status != want {
				t.Fatalf("after the hand-back: %s, %v; want %s", got.Status, err, want)
			}
			if _, done, err := f.m.AgentInput(ctx, h.SessionID, "tok"); err != nil {
				t.Fatalf("agent input once told %s = %v", got.Status, err)
			} else {
				done()
			}
		})
	}
}

// A handoff page closed, then reloaded, gets control back, even while the
// closed page's takeover is still being handed back (PROJ-196).
func TestClosedThenReloadedHandoffPageGetsControlBack(t *testing.T) {
	reloadDuringHandBack(t, 500*time.Millisecond)
}

// A hand-back that answers can take a few seconds (the lift's drain, the
// settle, emptying both selections): a reload during a 4 s one still gets
// control back, once it is over.
func TestReloadDuringASlowHandBackGetsControlBack(t *testing.T) {
	reloadDuringHandBack(t, 4*time.Second)
}

// reloadDuringHandBack closes a handoff's connected page, whose
// hand-back's lift takes slowFor, and reloads it meanwhile.
func reloadDuringHandBack(t *testing.T, slowFor time.Duration) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	_, token := f.open("")
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	offer := func(sdp string) reply {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/h/"+token+"/offer", strings.NewReader(sdp))
		req.Header.Set("Content-Type", "application/json")
		resp, err := phone.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return reply{code: resp.StatusCode, body: string(b)}
	}
	// The first page connects for real.
	pc, err := pionwebrtc.NewPeerConnection(pionwebrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if _, err := pc.AddTransceiverFromKind(pionwebrtc.RTPCodecTypeVideo, pionwebrtc.RTPTransceiverInit{Direction: pionwebrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.CreateDataChannel("input", nil); err != nil {
		t.Fatal(err)
	}
	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s pionwebrtc.PeerConnectionState) {
		if s == pionwebrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	o, _ := pc.CreateOffer(nil)
	gathered := pionwebrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(o)
	<-gathered
	body, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP})
	r := offer(string(body))
	var answer struct{ SDP string }
	if r.code != http.StatusOK || json.Unmarshal([]byte(r.body), &answer) != nil {
		t.Fatalf("first offer: %d %s", r.code, r.body)
	}
	if err := pc.SetRemoteDescription(pionwebrtc.SessionDescription{Type: pionwebrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the first page never connected")
	}
	// Its hand-back will be slow; the page closes, and the browser sees
	// it go.
	release := make(chan struct{})
	var lifted sync.Once
	slow := func() {
		lifted.Do(func() { close(release) })
		time.Sleep(slowFor)
	}
	f.onLift.Store(&slow)
	_ = pc.Close()
	select {
	case <-release:
	case <-time.After(10 * time.Second):
		t.Fatal("the closed page's takeover was never handed back")
	}
	// The reloaded page, while that hand-back is under way.
	if r := offer(pionOffer(t)); r.code != http.StatusOK || !strings.Contains(r.body, `"control":true`) {
		t.Fatalf("the reloaded page's offer: %d %s", r.code, r.body)
	}
}

// A handoff is never stored open without the help epoch it opened: not
// even while the browser is still going to Needs help (waiting for the
// agent's call under way), so a restart then always finds it (PROJ-196).
func TestOpenHandoffAlwaysHasItsEpoch(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	s := f.running()
	lease, err := f.m.PageLease(ctx, s.ID, "tok") // an agent's call under way
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan Handoff, 1)
	go func() {
		h, _, err := f.svc.Create(ctx, s.ID, "research-bot", SensitiveInput, "Enter the 2FA code", "")
		if err != nil {
			t.Error(err)
		}
		opened <- h
	}()
	time.Sleep(100 * time.Millisecond) // it waits for the call to end
	var zero int
	if err := f.svc.db.QueryRow(`SELECT count(*) FROM handoffs WHERE status = 'pending' AND help_epoch = 0`).Scan(&zero); err != nil {
		t.Fatal(err)
	}
	if zero != 0 {
		t.Errorf("%d open handoff stored without its help epoch", zero)
	}
	lease.Release()
	h := <-opened
	epoch, _ := f.m.HelpEpoch(s.ID)
	var stored uint64
	if err := f.svc.db.QueryRow(`SELECT help_epoch FROM handoffs WHERE id = ?`, h.ID).Scan(&stored); err != nil || stored == 0 || stored != epoch {
		t.Fatalf("stored help epoch %d, %v; the browser's %d", stored, err, epoch)
	}
}

// A handoff whose stored help epoch isn't its browser's (stored by an
// older BrowserPier) still ends the help its browser waits for, logged,
// rather than leave its agent paused for good (PROJ-196).
func TestHandoffEndRepairsAMismatchedEpoch(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	h, _ := f.open("")
	if _, err := f.svc.db.Exec(`UPDATE handoffs SET help_epoch = 99 WHERE id = ?`, h.ID); err != nil {
		t.Fatal(err)
	}
	h, err := f.svc.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Finish(ctx, h, Done, ""); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running {
		t.Fatalf("after the handoff: %s", s.State)
	}
}

// Two ends of one handoff at once (the person's Done, the agent's
// withdrawal) run one hand-back: the second joins the first and gets its
// outcome, neither returns before the browser is handed back, and the
// loser never undoes the winner's end (PROJ-196).
func TestConcurrentEndsOfOneHandoff(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	h, _ := f.open("")
	if err := f.m.TakeControl(h.SessionID, "person"); err != nil {
		t.Fatal(err)
	}
	var lifts atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	hold := func() { lifts.Add(1); <-gate }
	f.onLift.Store(&hold)
	type outcome struct {
		h   Handoff
		err error
	}
	results := make(chan outcome, 2)
	go func() {
		got, err := f.svc.Finish(ctx, h, Done, "")
		results <- outcome{got, err}
	}()
	go func() {
		got, err := f.svc.Withdraw(ctx, h, "")
		results <- outcome{got, err}
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-results:
		t.Fatalf("an end returned (%s, %v) before the hand-back", r.h.Status, r.err)
	default:
	}
	if n := lifts.Load(); n != 1 {
		t.Errorf("%d hand-backs began, want 1", n)
	}
	letGo()
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.h.Status != b.h.Status || !a.h.Status.Final() {
		t.Fatalf("the two ends: %s, %v and %s, %v; want one final status", a.h.Status, a.err, b.h.Status, b.err)
	}
	got, err := f.svc.Get(ctx, h.ID)
	if err != nil || got.Status != a.h.Status {
		t.Fatalf("stored: %s, %v; told %s", got.Status, err, a.h.Status)
	}
	if s, _ := f.m.Live("google"); s.State != sessions.Running || s.HumanControl {
		t.Errorf("after: %s, person in control %v", s.State, s.HumanControl)
	}
}

// A handoff's end whose help has already ended never ends a newer help:
// a later handoff's, opened meanwhile, before that one's row was stored
// (PROJ-196).
func TestOldHandoffEndLeavesANewerHelp(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	old, _ := f.open("")
	// Its help ends (as by an end cut short), and a later handoff opens
	// help, its row not stored yet.
	if _, err := f.m.EndHandoff(old.SessionID, old.helpEpoch); err != nil {
		t.Fatal(err)
	}
	newer, err := f.m.OpenHelp(old.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Finish(ctx, old, Done, ""); err != nil {
		t.Fatal(err)
	}
	if epoch, waiting := f.m.HelpEpoch(old.SessionID); !waiting || epoch != newer {
		t.Fatalf("the newer help: waiting %v, epoch %d; want %d", waiting, epoch, newer)
	}
}

// settled reads handoff id as an agent waiting on it would: once an end
// a read started, or one under way, is over (a few seconds at most with
// a browser that answers).
func (f *fixture) settled(id string) (Handoff, error) {
	return f.svc.Wait(context.Background(), id, 5*time.Second)
}

// Close waits for the ends under way, the sweep and the waits, even an
// end whose browser doesn't answer its hand-back: it stops waiting and
// leaves the handoff pending, a wait under way answers ErrClosing, and
// nothing of the service touches the database once Close has returned
// (the database closes right after, at shutdown).
func TestCloseWaitsForEndsAndTheSweep(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	var mu sync.Mutex
	var logged strings.Builder
	f.svc.log = slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(b)
	}), nil))
	h, _ := f.open("")
	if err := f.m.TakeControl(h.SessionID, sessions.AdminHolder); err != nil { // someone in the live view
		t.Fatal(err)
	}
	stuck := make(chan struct{})
	hold := func() { <-stuck }
	f.onLift.Store(&hold)
	later := time.Now().Add(Lifetime + time.Second)
	f.svc.now = func() time.Time { return later }
	swept := make(chan struct{})
	go func() { f.svc.Sweep(context.Background(), 10*time.Millisecond); close(swept) }()
	for deadline := time.Now().Add(5 * time.Second); !f.svc.isEnding(h.ID); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the sweep never started the end")
		}
	}
	// An agent long-polls it meanwhile.
	waited := make(chan error, 1)
	go func() {
		_, err := f.svc.Wait(context.Background(), h.ID, time.Minute)
		waited <- err
	}()
	time.Sleep(100 * time.Millisecond)
	closed := make(chan struct{})
	go func() { f.svc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close is held by a browser that doesn't answer")
	}
	select {
	case <-swept:
	default:
		t.Fatal("the sweep outlived Close")
	}
	select {
	case err := <-waited:
		if !errors.Is(err, ErrClosing) {
			t.Fatalf("the wait under way = %v, want ErrClosing", err)
		}
	default:
		t.Fatal("the wait outlived Close")
	}
	// Its browser wasn't handed back in this process: nothing is saved,
	// so no agent hears it is over; the next start ends it.
	var status Status
	if err := f.svc.db.QueryRow(`SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&status); err != nil || status != Pending {
		t.Fatalf("after Close: %s, %v; want it left pending", status, err)
	}
	if _, err := f.svc.Wait(context.Background(), h.ID, time.Second); !errors.Is(err, ErrClosing) {
		t.Fatalf("a wait after Close = %v, want ErrClosing", err)
	}
	// Shutdown goes on: the database closes, then the browser answers.
	_ = f.svc.db.Close()
	close(stuck)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logged.String(), "closed") {
		t.Fatalf("the service used the database after Close: %s", logged.String())
	}
}

// writerFunc is an io.Writer.
type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }

// While a handoff's browser is being handed back (its end under way, or
// its time up), its page says "Handing back…" and offers nothing: no live
// view, no Done, no PIN form, no timer that reloads it, and a PIN sent
// then isn't counted. It asks the status, which answers anyone that it is
// ending, until the end is over (handoff.js).
func TestHandoffPageWhileHandingBack(t *testing.T) {
	ending := func(t *testing.T, p reply, what string) {
		t.Helper()
		for _, want := range []string{"Handing back…", `data-ending="/h/`} {
			if !strings.Contains(p.body, want) {
				t.Fatalf("%s: no %q in %.300s", what, want, p.body)
			}
		}
		for _, not := range []string{"data-expires", "data-offer", `name="pin"`, "Done, hand back", `id="timer"`, "live.js"} {
			if strings.Contains(p.body, not) {
				t.Fatalf("%s offers %q while handing back", what, not)
			}
		}
	}
	status := func(t *testing.T, f *fixture, c *http.Client, token string) {
		t.Helper()
		p := f.do(c, "GET", "/h/"+token+"/status", nil, nil)
		if p.code != http.StatusOK || !strings.Contains(p.body, `"ending":true`) || !strings.Contains(p.body, `"handoff":"pending"`) {
			t.Fatalf("status while handing back: %d %s", p.code, p.body)
		}
	}
	wedge := func(f *fixture) {
		stuck := make(chan struct{})
		hold := func() { <-stuck }
		f.onLift.Store(&hold)
		f.t.Cleanup(func() { close(stuck) })
	}

	t.Run("done on a browser that doesn't answer", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
		f.svc.SetHandBackWait(300 * time.Millisecond)
		h, token := f.open("")
		phone := browser()
		f.openLink(phone, token)
		if err := f.m.TakeControl(h.SessionID, sessions.HandoffHolder(h.ID)); err != nil {
			t.Fatal(err)
		}
		wedge(f)
		p := f.do(phone, "POST", "/h/"+token+"/finish", url.Values{"status": {"done"}}, nil)
		if p.code != http.StatusServiceUnavailable {
			t.Fatalf("Done on a wedged browser: %d", p.code)
		}
		ending(t, p, "Done's answer")
		ending(t, f.do(phone, "GET", "/h/"+token, nil, nil), "the reloaded page")
		status(t, f, phone, token)
		status(t, f, browser(), token) // another browser, waiting for the end too
	})

	t.Run("expired, unopened, with a PIN", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, token := f.open("")
		if err := f.m.TakeControl(h.SessionID, sessions.AdminHolder); err != nil { // someone in the live view
			t.Fatal(err)
		}
		wedge(f)
		later := time.Now().Add(Lifetime + time.Second)
		f.svc.now = func() time.Time { return later }
		phone := browser()
		p := f.do(phone, "GET", "/h/"+token, nil, nil)
		if p.code != http.StatusOK {
			t.Fatalf("the page: %d", p.code)
		}
		ending(t, p, "the page")
		ending(t, f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"0000"}}, nil), "a PIN sent")
		var failures int
		if err := f.svc.db.QueryRow(`SELECT pin_failures FROM handoffs WHERE id = ?`, h.ID).Scan(&failures); err != nil || failures != 0 {
			t.Fatalf("a PIN sent while handing back counted: %d, %v", failures, err)
		}
		status(t, f, phone, token)
	})

	t.Run("a withdrawal under way, unopened", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
		f.svc.SetHandBackWait(200 * time.Millisecond)
		h, token := f.open("")
		if err := f.m.TakeControl(h.SessionID, sessions.AdminHolder); err != nil {
			t.Fatal(err)
		}
		wedge(f)
		if _, err := f.svc.Withdraw(context.Background(), h, ""); !errors.Is(err, ErrHandingBack) {
			t.Fatalf("withdrawing: %v", err)
		}
		// Opening the one-tap link now unlocks nothing: it is handing back.
		ending(t, f.do(browser(), "GET", "/h/"+token, nil, nil), "the link opened")
	})
}
