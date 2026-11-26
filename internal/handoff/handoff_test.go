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
	"strconv"
	"strings"
	"sync"
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
	return &fixture{t: t, m: m, svc: svc, admin: admin, srv: srv, cdp: b, tabs: tabs, emptied: emptied, choosers: choosers, files: fs}
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
	got, _ := f.svc.Get(context.Background(), h.ID)
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
	got, err := f.svc.Get(context.Background(), h.ID)
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
	got, _ := f.svc.Get(context.Background(), h.ID)
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
	f.svc.sweep(ctx)
	var status Status
	if err := f.svc.db.QueryRow(`SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&status); err != nil || status != Cancelled {
		t.Fatalf("after a sweep: %s, %v", status, err)
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
			if got, err := f.svc.Get(context.Background(), h.ID); err != nil || got.Status != Expired {
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
			if got := f.emptied.took(); len(got) != 1 || !got[0] {
				t.Fatalf("emptied %d times, with control %v; want once, while the person had it", len(got), got)
			}
			if s, _ := f.m.Live("google"); s.HumanControl || s.State != sessions.Running {
				t.Errorf("after the handoff: %+v", s)
			}
		})
	}
}
