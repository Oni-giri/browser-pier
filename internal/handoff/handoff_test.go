package handoff

import (
	"context"
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

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

type fixture struct {
	t     *testing.T
	m     *sessions.Manager
	svc   *Service
	admin *auth.Admin
	srv   *httptest.Server
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
	mux.HandleFunc("GET /cdp/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"type":"page","url":"https://accounts.google.com/signin/v2?continue=https%3A%2F%2Fevil.example%2Fpay+now"}]`)
	})
	rt.Control = mux
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
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
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{"127.0.0.1"}}, m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	hmux := http.NewServeMux()
	web.Register(hmux)
	NewWeb(svc, web, webrtc.NewHandler(relay, m, web, log), m, log).Register(hmux)
	srv := httptest.NewServer(hmux)
	t.Cleanup(srv.Close)
	return &fixture{t: t, m: m, svc: svc, admin: admin, srv: srv}
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
