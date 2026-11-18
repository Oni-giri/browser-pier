package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/theme"
)

type webFixture struct {
	t      *testing.T
	admin  *Admin
	tokens *Tokens
	web    *Web
	srv    *httptest.Server
	logs   *bytes.Buffer
}

func newWebFixture(t *testing.T, env string, opts WebOptions) *webFixture {
	t.Helper()
	d := openDB(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	admin, err := NewAdmin(context.Background(), d, env, log)
	if err != nil {
		t.Fatal(err)
	}
	if opts.CookieSecure == "" {
		opts.CookieSecure = "auto"
	}
	f := &webFixture{t: t, admin: admin, tokens: NewTokens(d), logs: &logs}
	f.web = NewWeb(admin, f.tokens, log, opts)
	mux := http.NewServeMux()
	f.web.Register(mux)
	mux.Handle("GET /{$}", f.web.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "dashboard")
	})))
	mux.Handle("GET /api/me", f.web.RequireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := TokenFromContext(r.Context())
		_, _ = io.WriteString(w, tok.Name)
	})))
	f.srv = httptest.NewServer(new(http.CrossOriginProtection).Handler(mux))
	t.Cleanup(f.srv.Close)
	return f
}

// browser is an HTTP client with cookies that doesn't follow redirects.
func (f *webFixture) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// reply is a finished response, body read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
	cookies    []*http.Cookie
}

func (r reply) Cookies() []*http.Cookie { return r.cookies }

func (f *webFixture) do(c *http.Client, req *http.Request) (reply, string) {
	f.t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, cookies: resp.Cookies()}, string(b)
}

func (f *webFixture) post(c *http.Client, path string, form url.Values) (reply, string) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.do(c, req)
}

func (f *webFixture) get(c *http.Client, path string) (reply, string) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
	return f.do(c, req)
}

func TestSignInFlow(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	b := f.browser()

	resp, _ := f.get(b, "/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2F" {
		t.Fatalf("signed-out dashboard: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := f.get(b, "/login")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sign in to BrowserPier") || !strings.Contains(body, "Umbrel") {
		t.Fatalf("login page %d: %s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP %q", csp)
	}

	resp, body = f.post(b, "/login", url.Values{"password": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Wrong password. 4 tries left before a 5‑minute lockout.") {
		t.Fatalf("wrong password: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(f.logs.String(), "wrong") && strings.Contains(f.logs.String(), "password=wrong") {
		t.Fatal("the attempted password was logged")
	}

	resp, _ = f.post(b, "/login", url.Values{"password": {"the-admin-password"}, "next": {"/profiles?x=1"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/profiles?x=1" {
		t.Fatalf("sign-in: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure || !cookie.Expires.IsZero() {
		t.Fatalf("cookie %+v: want HttpOnly, Lax, not Secure over plain HTTP, browser-session", cookie)
	}
	if strings.Contains(f.logs.String(), cookie.Value) {
		t.Fatal("the session cookie was logged")
	}
	if resp, body := f.get(b, "/"); resp.StatusCode != http.StatusOK || body != "dashboard" {
		t.Fatalf("signed-in dashboard: %d %s", resp.StatusCode, body)
	}

	// The cookie never authenticates the API.
	if resp, _ := f.get(b, "/api/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API with only a cookie: %d", resp.StatusCode)
	}

	resp, _ = f.post(b, "/logout", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout %d", resp.StatusCode)
	}
	if _, err := f.admin.Session(context.Background(), cookie.Value); err == nil {
		t.Fatal("session still valid after logout")
	}
}

func TestRememberMeCookie(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	resp, _ := f.post(f.browser(), "/login", url.Values{"password": {"the-admin-password"}, "remember": {"1"}})
	for _, c := range resp.Cookies() {
		if c.Name == CookieName && !c.Expires.IsZero() {
			return
		}
	}
	t.Fatal("remember me didn't make a persistent cookie")
}

func TestLoginLockout(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	b := f.browser()
	for i := 0; i < 4; i++ {
		f.post(b, "/login", url.Values{"password": {"nope"}})
	}
	resp, body := f.post(b, "/login", url.Values{"password": {"nope"}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Try again in 5 minutes") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("fifth failure: %d %s", resp.StatusCode, body)
	}
	// Locked out even with the right password.
	if resp, _ := f.post(b, "/login", url.Values{"password": {"the-admin-password"}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("right password while locked: %d", resp.StatusCode)
	}
	if !strings.Contains(f.logs.String(), "admin sign-in failed") {
		t.Fatal("failed attempts aren't logged")
	}
}

func TestOpenRedirectRefused(t *testing.T) {
	for in, want := range map[string]string{
		"/profiles":         "/profiles",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"https://evil":      "/",
		"":                  "/",
		"/x\r\nSet-Cookie:": "/",
		"/\t/evil.example":  "/", // browsers drop the tab and see //evil.example
		"/\n/evil.example":  "/",
		"/a\\b":             "/",
		"/ok?next=//x":      "/ok?next=//x",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCrossSiteLoginRefused(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ := f.do(http.DefaultClient, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d, want 403", resp.StatusCode)
	}
}

func TestSetupPage(t *testing.T) {
	f := newWebFixture(t, "", WebOptions{})
	b := f.browser()
	code := f.admin.setupCode

	for _, path := range []string{"/", "/login"} {
		if resp, _ := f.get(b, path); resp.Header.Get("Location") != "/setup" {
			t.Fatalf("%s before setup goes to %q", path, resp.Header.Get("Location"))
		}
	}
	if resp, body := f.get(b, "/setup"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "setup_code=") {
		t.Fatalf("setup page %d", resp.StatusCode)
	}
	resp, body := f.post(b, "/setup", url.Values{"code": {"wrong"}, "password": {"a-long-password"}, "confirm": {"a-long-password"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "setup code is wrong") {
		t.Fatalf("wrong code: %d %s", resp.StatusCode, body)
	}
	resp, body = f.post(b, "/setup", url.Values{"code": {code}, "password": {"a-long-password"}, "confirm": {"different"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "passwords don&#39;t match") {
		t.Fatalf("mismatch: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.post(b, "/setup", url.Values{"code": {code}, "password": {"a-long-password"}, "confirm": {"a-long-password"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := f.get(b, "/"); body != "dashboard" {
		t.Fatalf("after setup: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.get(b, "/setup"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("setup page after setup: %d", resp.StatusCode)
	}
}

func TestRequireToken(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	_, secret, err := f.tokens.Create(context.Background(), "claude", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/me", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, body := f.do(http.DefaultClient, req)
		return resp.StatusCode, body
	}
	if code, body := call("Bearer " + secret); code != http.StatusOK || body != "claude" {
		t.Fatalf("good token: %d %s", code, body)
	}
	if code, _ := call("bearer " + secret); code != http.StatusOK {
		t.Fatalf("lowercase scheme: %d", code)
	}
	for _, bad := range []string{"", "Bearer", "Basic " + secret, "Bearer bp_live_nope"} {
		if code, _ := call(bad); code != http.StatusUnauthorized {
			t.Errorf("%q: %d", bad, code)
		}
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Fatal("a token was logged")
	}
	for range 20 {
		call("Bearer bp_live_nope")
	}
	if code, _ := call("Bearer " + secret); code != http.StatusTooManyRequests {
		t.Fatalf("after many bad tokens from one address: %d, want 429", code)
	}
}

func TestClientIPAndHTTPS(t *testing.T) {
	_, proxies, _ := net.ParseCIDR("172.18.0.0/16")
	w := &Web{opts: WebOptions{TrustedProxies: []*net.IPNet{proxies}, CookieSecure: "auto"}}
	req := func(remote, xff, xfp string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		return r
	}
	cases := []struct {
		remote, xff, want string
	}{
		{"203.0.113.9:4000", "", "203.0.113.9"},
		{"203.0.113.9:4000", "1.2.3.4", "203.0.113.9"},               // untrusted peer: header ignored
		{"172.18.0.2:4000", "198.51.100.7", "198.51.100.7"},          // through the proxy
		{"172.18.0.2:4000", "6.6.6.6, 198.51.100.7", "198.51.100.7"}, // a spoofed hop on the left is skipped
		{"172.18.0.2:4000", "198.51.100.7, 172.18.0.3", "198.51.100.7"},
		{"172.18.0.2:4000", "garbage", "172.18.0.2"},
	}
	for _, c := range cases {
		if got := w.ClientIP(req(c.remote, c.xff, "")); got != c.want {
			t.Errorf("ClientIP(%s, %q) = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if !w.IsHTTPS(req("172.18.0.2:1", "", "https")) {
		t.Error("trusted proxy saying https not believed")
	}
	if w.IsHTTPS(req("203.0.113.9:1", "", "https")) {
		t.Error("untrusted peer's X-Forwarded-Proto believed")
	}
	tlsReq := req("203.0.113.9:1", "", "")
	tlsReq.TLS = &tls.ConnectionState{}
	if !w.IsHTTPS(tlsReq) || !w.SecureCookie(tlsReq) {
		t.Error("direct TLS not seen as HTTPS")
	}
	if w.SecureCookie(req("203.0.113.9:1", "", "")) {
		t.Error("Secure cookie over plain HTTP under auto")
	}
	w.opts.PublicURL = "https://pier.example.com"
	if !w.SecureCookie(req("203.0.113.9:1", "", "")) {
		t.Error("an https public URL should make cookies Secure")
	}
	w.opts.CookieSecure = "never"
	if w.SecureCookie(tlsReq) {
		t.Error("never means never")
	}
}

func TestHTMXGetsARedirectHeader(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/", nil)
	req.Header.Set("HX-Request", "true")
	resp, _ := f.do(f.browser(), req)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/login") {
		t.Fatalf("htmx request: %d, HX-Redirect %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
}

func TestGlobalBackstopThrottlesButNeverLocksTheOwnerOut(t *testing.T) {
	_, proxies, _ := net.ParseCIDR("127.0.0.0/8")
	f := newWebFixture(t, "the-admin-password", WebOptions{TrustedProxies: []*net.IPNet{proxies}})
	f.web.backstop.base, f.web.backstop.max = 50*time.Millisecond, 200*time.Millisecond
	from := func(ip, password string) (reply, string) {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/login", strings.NewReader(url.Values{"password": {password}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", ip)
		return f.do(f.browser(), req)
	}
	// An attacker spread over many addresses trips the backstop.
	for i := range 25 {
		from(fmt.Sprintf("198.51.100.%d", i), "guess")
	}
	start := time.Now()
	resp, _ := from("203.0.113.1", "the-admin-password")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the owner's sign-in during an attack: %d, want it to work", resp.StatusCode)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("attempts past the backstop weren't slowed")
	}
}

func TestBackstopDelayGrows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := &backstop{window: 15 * time.Minute, free: 20, base: 2 * time.Second, max: 30 * time.Second, now: func() time.Time { return now }}
	at := func(n int) time.Duration {
		b.fails, b.pending = nil, 0
		for range n {
			a, _ := b.begin()
			a.fail()
		}
		a, d := b.begin()
		a.end()
		return d
	}
	for n, want := range map[int]time.Duration{0: 0, 19: 0, 20: 2 * time.Second, 39: 2 * time.Second, 40: 4 * time.Second, 60: 8 * time.Second, 100: 30 * time.Second, 1000: 30 * time.Second} {
		if got := at(n); got != want {
			t.Errorf("after %d failures the delay is %s, want %s", n, got, want)
		}
	}
	at(100)
	now = now.Add(16 * time.Minute)
	if a, d := b.begin(); d != 0 {
		t.Fatalf("delay %s after the window passed", d)
	} else {
		a.end()
	}

	// Attempts in flight count, so a burst can't all see no delay.
	b.fails, b.pending = nil, 0
	var last time.Duration
	for range 25 {
		_, last = b.begin()
	}
	if last == 0 {
		t.Fatal("25 parallel attempts all got through with no delay")
	}
}

func TestKnownDeviceSkipsTheBackstop(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	f.web.backstop.base, f.web.backstop.max = time.Hour, time.Hour // anyone throttled would hang
	owner := f.browser()
	if resp, _ := f.post(owner, "/login", url.Values{"password": {"the-admin-password"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first sign-in %d", resp.StatusCode)
	}
	// An attack trips the backstop.
	for range 30 {
		a, _ := f.web.backstop.begin()
		a.fail()
	}
	f.post(owner, "/logout", nil)
	done := make(chan reply, 1)
	go func() {
		resp, _ := f.post(owner, "/login", url.Values{"password": {"the-admin-password"}})
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("known device sign-in %d", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a known device was throttled")
	}

	// The device cookie stops counting once the password changes.
	ctx := context.Background()
	dev := ""
	for _, c := range owner.Jar.Cookies(mustURL(f.srv.URL + "/login")) {
		if c.Name == DeviceCookie {
			dev = c.Value
		}
	}
	if !f.admin.KnownDevice(ctx, dev) {
		t.Fatal("device cookie not recognised")
	}
	if err := f.admin.storePassword(ctx, "a-new-password", ""); err != nil {
		t.Fatal(err)
	}
	if f.admin.KnownDevice(ctx, dev) {
		t.Fatal("device cookie still known after a password change")
	}
	for _, bad := range []string{"", "x", dev + "0", strings.Replace(dev, ".", "", 1)} {
		if f.admin.KnownDevice(ctx, bad) {
			t.Errorf("KnownDevice(%q)", bad)
		}
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestFailingDeviceCookieStopsBeingTrusted(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	f.web.loginIP = NewLockout(1000, time.Minute, time.Minute) // isolate the device limit
	b := f.browser()
	f.post(b, "/login", url.Values{"password": {"the-admin-password"}})
	dev := ""
	for _, c := range b.Jar.Cookies(mustURL(f.srv.URL + "/login")) {
		if c.Name == DeviceCookie {
			dev = c.Value
		}
	}
	for range 5 {
		f.post(b, "/login", url.Values{"password": {"guess"}})
	}
	if wait := f.web.deviceFails.Locked(dev); wait == 0 {
		t.Fatal("a device cookie that failed 5 times is still trusted")
	}
	// Its next attempt goes through the backstop like anyone else's.
	before := len(f.web.backstop.fails)
	f.post(b, "/login", url.Values{"password": {"guess"}})
	if len(f.web.backstop.fails) != before+1 {
		t.Fatal("an untrusted device's failure wasn't counted by the backstop")
	}
}

func TestThrottleQueueIsBounded(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	f.web.waiting.Store(maxWaiting)
	if _, ok := f.web.throttle(context.Background(), time.Millisecond); ok {
		t.Fatal("a sign-in queued past the limit")
	}
	f.web.waiting.Store(0)
	release, ok := f.web.throttle(context.Background(), time.Millisecond)
	if !ok {
		t.Fatal("an empty queue refused a sign-in")
	}
	release()
	if n := f.web.waiting.Load(); n != 0 {
		t.Fatalf("%d left waiting", n)
	}
}

func TestSignInPageHasTheDevicesTheme(t *testing.T) {
	f := newWebFixture(t, "the-admin-password", WebOptions{})
	b := f.browser()
	if _, body := f.get(b, "/login"); !strings.Contains(body, `<html lang="en">`) || !strings.Contains(body, `content="light dark"`) {
		t.Fatalf("system: %.400s", body)
	}
	u, _ := url.Parse(f.srv.URL)
	b.Jar.SetCookies(u, []*http.Cookie{{Name: theme.Cookie, Value: "dark", Path: "/"}})
	if _, body := f.get(b, "/login"); !strings.Contains(body, `<html lang="en" data-theme="dark">`) || !strings.Contains(body, `<meta name="color-scheme" content="dark">`) {
		t.Fatalf("dark: %.400s", body)
	}
}

// Two-step sign-in (PROJ-162).

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// twoStepFixture is a sign-in server with a fake clock and two-step on.
// Whatever secret, code or step cookie a test uses is checked to be
// absent from the logs when it ends.
type twoStepFixture struct {
	*webFixture
	clock   *fakeClock
	secret  []byte
	codes   []string
	secrets []string
}

func newTwoStepFixture(t *testing.T, opts WebOptions) *twoStepFixture {
	t.Helper()
	f := &twoStepFixture{webFixture: newWebFixture(t, "the-admin-password", opts), clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}}
	f.admin.now = f.clock.now
	w := f.web
	for _, l := range []*Lockout{w.loginIP, w.deviceFails, w.codeIP, w.codeAccount, w.setup, w.badToken} {
		l.now = f.clock.now
	}
	w.backstop.now = f.clock.now
	w.steps.now = f.clock.now
	f.enable()
	t.Cleanup(func() {
		logs := f.logs.String()
		for _, s := range f.secrets {
			if s != "" && strings.Contains(logs, s) {
				t.Errorf("the logs hold a secret, code or step cookie (%.4s…):\n%s", s, logs)
			}
		}
	})
	return f
}

func (f *twoStepFixture) enable() {
	f.t.Helper()
	f.secret = NewTOTPSecret()
	codes, err := f.admin.EnableTwoStep(context.Background(), f.secret, TOTPCode(f.secret, f.clock.now()), "")
	if err != nil {
		f.t.Fatal(err)
	}
	f.codes = codes
	enc := EncodeTOTPSecret(f.secret)
	f.secrets = append(f.secrets, string(f.secret), enc, strings.ToLower(enc), TOTPURL(f.secret, "127.0.0.1"), "otpauth://")
	for _, c := range codes {
		n, _ := classifyCode(c)
		f.secrets = append(f.secrets, c, n)
	}
	f.clock.add(30 * time.Second) // the confirming code's step is used
}

// code is the app's code now.
func (f *twoStepFixture) code() string {
	c := TOTPCode(f.secret, f.clock.now())
	f.secrets = append(f.secrets, c)
	return c
}

// send posts a form from ip ("" for the connection's own address).
func (f *twoStepFixture) send(c *http.Client, ip, path string, form url.Values) (reply, string) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	resp, body := f.do(c, req)
	if ck := cookieNamed(resp.Cookies(), StepCookie); ck != nil {
		f.secrets = append(f.secrets, ck.Value)
	}
	return resp, body
}

// password does the password step and expects the code page next.
func (f *twoStepFixture) password(c *http.Client, ip string, form url.Values) reply {
	f.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("password", "the-admin-password")
	resp, body := f.send(c, ip, "/login", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login/code" {
		f.t.Fatalf("right password with two-step on: %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	return resp
}

func (f *twoStepFixture) enter(c *http.Client, ip, code string) (reply, string) {
	f.t.Helper()
	return f.send(c, ip, "/login/code", url.Values{"code": {code}})
}

func (f *twoStepFixture) sessions() int {
	f.t.Helper()
	var n int
	if err := f.admin.db.QueryRow(`SELECT count(*) FROM admin_sessions`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func cookieNamed(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

func trustLoopback() WebOptions {
	_, lo, _ := net.ParseCIDR("127.0.0.0/8")
	return WebOptions{TrustedProxies: []*net.IPNet{lo}}
}

func TestTwoStepPasswordAloneSignsNothingIn(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	b := f.browser()
	resp := f.password(b, "", url.Values{"next": {"/profiles?x=1"}, "remember": {"1"}})
	if cookieNamed(resp.Cookies(), CookieName) != nil || cookieNamed(resp.Cookies(), DeviceCookie) != nil {
		t.Fatalf("the password alone set %v", resp.Cookies())
	}
	step := cookieNamed(resp.Cookies(), StepCookie)
	if step == nil || !step.HttpOnly || step.SameSite != http.SameSiteStrictMode || step.Path != "/login" || step.Secure {
		t.Fatalf("step cookie %+v: want HttpOnly, Strict, /login, not Secure over plain HTTP", step)
	}
	if f.sessions() != 0 {
		t.Fatal("the password alone made a session")
	}
	if resp, _ := f.get(b, "/"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("dashboard after the password alone: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The code page: a plain form under the sign-in page's CSP.
	_, login := f.get(f.browser(), "/login")
	loginResp, _ := f.get(f.browser(), "/login")
	resp, page := f.get(b, "/login/code")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Enter your code") || !strings.Contains(page, `autocomplete="one-time-code"`) ||
		!strings.Contains(page, `inputmode="numeric"`) || !strings.Contains(page, "Lost your phone? Use a recovery code") {
		t.Fatalf("code page %d: %s", resp.StatusCode, page)
	}
	if got, want := resp.Header.Get("Content-Security-Policy"), loginResp.Header.Get("Content-Security-Policy"); got != want || strings.Contains(got, "script-src") || strings.Contains(got, "unsafe-inline") {
		t.Fatalf("code page CSP %q, sign-in page's %q", got, want)
	}
	for _, p := range []string{page, login} {
		if strings.Contains(p, "<script") || strings.Contains(p, "style=") {
			t.Fatalf("script or inline style on a sign-in page: %s", p)
		}
	}

	// A right code signs in, remembered as asked on the password form.
	resp, _ = f.enter(b, "", f.code())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/profiles?x=1" {
		t.Fatalf("right code: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	sess, dev := cookieNamed(resp.Cookies(), CookieName), cookieNamed(resp.Cookies(), DeviceCookie)
	if sess == nil || dev == nil || sess.Expires.Before(f.clock.now().Add(29*24*time.Hour)) && sess.Expires.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("after the code: session %+v, device %+v; want both, the session for 30 days", sess, dev)
	}
	f.secrets = append(f.secrets, sess.Value, dev.Value)
	if s, err := f.admin.Session(context.Background(), sess.Value); err != nil || !s.Remember || f.sessions() != 1 {
		t.Fatalf("session %+v, %v; %d sessions", s, err, f.sessions())
	}
	if resp, body := f.get(b, "/"); body != "dashboard" {
		t.Fatalf("dashboard after the code: %d", resp.StatusCode)
	}

	// The step was used up.
	again := f.browser()
	again.Jar.SetCookies(mustURL(f.srv.URL+"/login"), []*http.Cookie{{Name: StepCookie, Value: step.Value, Path: "/login"}})
	if resp, _ := f.enter(again, "", f.code()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" || f.sessions() != 1 {
		t.Fatalf("a used step: %d to %q, %d sessions", resp.StatusCode, resp.Header.Get("Location"), f.sessions())
	}
	// The same code, with a new password step in the same 30 seconds, is refused.
	other := f.browser()
	f.password(other, "", nil)
	if resp, body := f.enter(other, "", f.code()); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "isn&#39;t right") || f.sessions() != 1 {
		t.Fatalf("a replayed code: %d, %d sessions: %s", resp.StatusCode, f.sessions(), body)
	}
	if !strings.Contains(f.logs.String(), "wrong sign-in code after a right password") || !strings.Contains(f.logs.String(), "tries_left=") {
		t.Fatalf("a wrong code after a right password isn't logged: %s", f.logs)
	}
	// The next step's code works.
	f.clock.add(30 * time.Second)
	if resp, _ := f.enter(other, "", f.code()); resp.StatusCode != http.StatusSeeOther || f.sessions() != 2 {
		t.Fatalf("the next code: %d, %d sessions", resp.StatusCode, f.sessions())
	}
}

func TestTwoStepParallelCodeMakesOneSession(t *testing.T) {
	f := newTwoStepFixture(t, trustLoopback())
	f.web.codeAccount = NewLockout(100, time.Hour, time.Hour) // let all 20 reach the database
	// Line all 20 up after they read the state, before any writes: the
	// worst case, where only the guarded update of the last step can
	// tell them apart.
	var arrived sync.WaitGroup
	arrived.Add(20)
	lined := make(chan struct{})
	go func() { arrived.Wait(); close(lined) }()
	hook := func() {
		arrived.Done()
		select {
		case <-lined:
		case <-time.After(5 * time.Second):
		}
	}
	testHookBeforeCodeTx.Store(&hook)
	t.Cleanup(func() { testHookBeforeCodeTx.Store(nil) })
	browsers := make([]*http.Client, 20)
	for i := range browsers {
		browsers[i] = f.browser()
		f.password(browsers[i], fmt.Sprintf("198.51.100.%d", i+1), nil)
	}
	code := f.code()
	results := make(chan int, len(browsers))
	var wg sync.WaitGroup
	for i, b := range browsers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/login/code", strings.NewReader(url.Values{"code": {code}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
			resp, err := b.Do(req)
			if err != nil {
				results <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusSeeOther && resp.Header.Get("Location") == "/" {
				results <- 1
				return
			}
			results <- 0
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for r := range results {
		won += r
	}
	if won != 1 || f.sessions() != 1 {
		t.Fatalf("20 parallel posts of one code: %d signed in, %d sessions; want 1", won, f.sessions())
	}
}

func TestTwoStepPendingStepLimits(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	f.web.codeIP = NewLockout(1000, time.Minute, time.Minute) // isolate the step's own limit

	// Refused after 5 minutes.
	b := f.browser()
	f.password(b, "", nil)
	f.clock.add(5 * time.Minute)
	if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" || f.sessions() != 0 {
		t.Fatalf("a step after 5 minutes: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Ended by 5 wrong codes; the 6th post, right, goes back to the password.
	b = f.browser()
	f.password(b, "", nil)
	for i := 1; i <= 4; i++ {
		resp, body := f.enter(b, "", "000000")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, fmt.Sprintf("%d %s left", 5-i, plural(5-i, "try", "tries"))) {
			t.Fatalf("wrong code %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, body := f.enter(b, "", "000000"); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Enter your password again") {
		t.Fatalf("fifth wrong code: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" || f.sessions() != 0 {
		t.Fatalf("right code after the step ended: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// No step cookie, or one that names no step, makes nothing; and
	// doesn't touch a step another browser has.
	owner := f.browser()
	f.password(owner, "", nil)
	forged := f.browser()
	forged.Jar.SetCookies(mustURL(f.srv.URL+"/login"), []*http.Cookie{{Name: StepCookie, Value: randomString(32), Path: "/login"}})
	for _, c := range []*http.Client{f.browser(), forged} {
		if resp, _ := f.enter(c, "", f.code()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" || f.sessions() != 0 {
			t.Fatalf("code without a step: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, _ := f.enter(owner, "", f.code()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("the owner's own step: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestPendingStepsAreCapped(t *testing.T) {
	ps := newPendingSteps()
	first := ps.add(&pendingStep{next: "/first"})
	for range maxSteps {
		ps.add(&pendingStep{next: "/"})
	}
	if len(ps.m) != maxSteps {
		t.Fatalf("%d steps in memory", len(ps.m))
	}
	if _, ok := ps.peek(first); ok {
		t.Fatal("the oldest step wasn't dropped")
	}
}

func TestRightPasswordDoesNotClearCodeFailures(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	b := f.browser()
	f.password(b, "", nil)
	for range 4 {
		f.enter(b, "", "000000")
	}
	f.password(b, "", nil)
	resp, body := f.enter(b, "", "000000")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Try again in 5 minutes") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("4 wrong codes, a right password, 1 more: %d %s", resp.StatusCode, body)
	}
	f.password(b, "", nil)
	if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusTooManyRequests || f.sessions() != 0 {
		t.Fatalf("right code while code entry is locked: %d", resp.StatusCode)
	}
	f.clock.add(5 * time.Minute)
	f.password(b, "", nil)
	if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("right code after the lockout: %d", resp.StatusCode)
	}
}

func TestTwoStepAccountCap(t *testing.T) {
	f := newTwoStepFixture(t, trustLoopback())
	// The owner's usual browser signs in once and so is known.
	owner := f.browser()
	f.password(owner, "203.0.113.1", nil)
	if resp, _ := f.enter(owner, "203.0.113.1", f.code()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner's sign-in: %d", resp.StatusCode)
	}
	f.post(owner, "/logout", nil)

	// Someone who knows the password tries codes from 10 addresses.
	for i := range 10 {
		ip := fmt.Sprintf("198.51.100.%d", i+1)
		b := f.browser()
		f.password(b, ip, nil)
		if resp, _ := f.enter(b, ip, "000000"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: %d", i, resp.StatusCode)
		}
	}
	// The 11th try, even right, from a new address is refused.
	f.clock.add(30 * time.Second)
	b := f.browser()
	f.password(b, "198.51.100.11", nil)
	if resp, body := f.enter(b, "198.51.100.11", f.code()); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "for this account today") || f.sessions() != 0 {
		t.Fatalf("right code past the account cap: %d %s", resp.StatusCode, body)
	}
	// The owner's known browser still signs in with a right code.
	f.password(owner, "203.0.113.1", nil)
	if resp, _ := f.enter(owner, "203.0.113.1", f.code()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a known device past the account cap: %d", resp.StatusCode)
	}
	// So does a recovery code from a new address.
	r := f.browser()
	f.password(r, "198.51.100.12", nil)
	if resp, _ := f.enter(r, "198.51.100.12", f.codes[0]); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a recovery code past the account cap: %d", resp.StatusCode)
	}
	// A day later, an app code from a new address works again.
	f.clock.add(24 * time.Hour)
	n := f.browser()
	f.password(n, "198.51.100.13", nil)
	if resp, _ := f.enter(n, "198.51.100.13", f.code()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a right code a day later: %d", resp.StatusCode)
	}
}

func TestDeviceCookieWithWrongCodesStopsBeingTrusted(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	f.web.codeIP = NewLockout(1000, time.Minute, time.Minute) // isolate the device limit
	b := f.browser()
	f.password(b, "", nil)
	if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.StatusCode)
	}
	dev := ""
	for _, c := range b.Jar.Cookies(mustURL(f.srv.URL + "/login")) {
		if c.Name == DeviceCookie {
			dev = c.Value
		}
	}
	accountFails := func() int {
		f.web.codeAccount.mu.Lock()
		defer f.web.codeAccount.mu.Unlock()
		if a := f.web.codeAccount.keys["account"]; a != nil {
			return len(a.failures)
		}
		return 0
	}
	f.password(b, "", nil)
	for range 5 {
		f.enter(b, "", "000000")
	}
	if f.web.deviceFails.Locked(dev) == 0 {
		t.Fatal("a device cookie with 5 wrong codes is still trusted")
	}
	if accountFails() != 0 {
		t.Fatal("a trusted device's wrong codes counted on the account")
	}
	f.password(b, "", nil)
	f.enter(b, "", "000000")
	if accountFails() != 1 {
		t.Fatal("an untrusted device's wrong code isn't under the account cap")
	}
}

func TestRecoveryCodeSignsIn(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	ctx := context.Background()
	b := f.browser()
	f.password(b, "", nil)
	if resp, _ := f.enter(b, "", strings.ToLower(f.codes[3])); resp.StatusCode != http.StatusSeeOther || f.sessions() != 1 {
		t.Fatalf("recovery code: %d", resp.StatusCode)
	}
	if st, _ := f.admin.TwoStep(ctx); st.CodesLeft != RecoveryCodes-1 {
		t.Fatalf("%d left", st.CodesLeft)
	}
	c := f.browser()
	f.password(c, "", nil)
	if resp, _ := f.enter(c, "", f.codes[3]); resp.StatusCode != http.StatusUnauthorized || f.sessions() != 1 {
		t.Fatalf("a used recovery code: %d", resp.StatusCode)
	}
}

func TestFailedCodeWriteMakesNoSession(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	// The database refuses to record a code's use.
	for _, q := range []string{
		`CREATE TRIGGER fail_step BEFORE UPDATE ON settings WHEN NEW.key = 'totp_last_step' BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
		`CREATE TRIGGER fail_recovery BEFORE DELETE ON admin_recovery_codes BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
	} {
		if _, err := f.admin.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range []string{f.code(), f.codes[0]} {
		b := f.browser()
		f.password(b, "", nil)
		resp, body := f.enter(b, "", code)
		if resp.StatusCode != http.StatusInternalServerError || cookieNamed(resp.Cookies(), CookieName) != nil || f.sessions() != 0 {
			t.Fatalf("a code whose use couldn't be written: %d, %d sessions: %s", resp.StatusCode, f.sessions(), body)
		}
	}
}

func TestUnreadableTwoStepRefusesSignIn(t *testing.T) {
	for key, value := range map[string]string{keyTOTPSecret: "not base32!", keyTOTPLast: "soon"} {
		f := newTwoStepFixture(t, WebOptions{})
		if _, err := f.admin.db.Exec(`UPDATE settings SET value = ? WHERE key = ?`, value, key); err != nil {
			t.Fatal(err)
		}
		b := f.browser()
		resp, body := f.send(b, "", "/login", url.Values{"password": {"the-admin-password"}})
		if resp.StatusCode != http.StatusInternalServerError || cookieNamed(resp.Cookies(), CookieName) != nil || cookieNamed(resp.Cookies(), StepCookie) != nil || f.sessions() != 0 {
			t.Fatalf("broken %s: %d, %d sessions: %s", key, resp.StatusCode, f.sessions(), body)
		}
	}
	// Also a step begun before the state broke.
	f := newTwoStepFixture(t, WebOptions{})
	b := f.browser()
	f.password(b, "", nil)
	if _, err := f.admin.db.Exec(`UPDATE settings SET value = 'x' WHERE key = ?`, keyTOTPSecret); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusInternalServerError || f.sessions() != 0 {
		t.Fatalf("code with a broken state: %d", resp.StatusCode)
	}
}

func TestSignInPageSaysNothingAboutTwoStep(t *testing.T) {
	off := newWebFixture(t, "the-admin-password", WebOptions{})
	on := newTwoStepFixture(t, WebOptions{})
	look := func(f *webFixture) (string, string, int, string) {
		_, page := f.get(f.browser(), "/login?next=/profiles")
		resp, wrong := f.post(f.browser(), "/login", url.Values{"password": {"nope"}, "next": {"/profiles"}})
		var names []string
		for _, c := range resp.Cookies() {
			names = append(names, c.Name)
		}
		return page, wrong, resp.StatusCode, strings.Join(names, ",")
	}
	p1, w1, s1, c1 := look(off)
	p2, w2, s2, c2 := look(on.webFixture)
	if p1 != p2 || w1 != w2 || s1 != s2 || c1 != c2 {
		t.Fatalf("the sign-in page differs with two-step on:\n%s\n---\n%s\n%d %d %q %q", p1, p2, s1, s2, c1, c2)
	}
}

func TestOpenRedirectRefusedAfterTheCode(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	for _, next := range []string{"//evil.example", "https://evil.example"} {
		b := f.browser()
		f.password(b, "", url.Values{"next": {next}})
		f.clock.add(30 * time.Second)
		if resp, _ := f.enter(b, "", f.code()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("next=%s after the code: %d to %q", next, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestCrossSiteCodeRefused(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	b := f.browser()
	f.password(b, "", nil)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/login/code", strings.NewReader(url.Values{"code": {f.code()}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, _ := f.do(b, req); resp.StatusCode != http.StatusForbidden || f.sessions() != 0 {
		t.Fatalf("cross-site code post: %d", resp.StatusCode)
	}
}

// Over plain HTTP from a private address, the code form posts its origin
// like the password form (TestSignInOverPlainHTTP).
func TestCodeFormOverPlainHTTP(t *testing.T) {
	f := newTwoStepFixture(t, WebOptions{})
	b := f.browser()
	f.password(b, "", nil)
	post := func(origin string) int {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/login/code", strings.NewReader(url.Values{"code": {f.code()}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		resp, _ := f.do(b, req)
		return resp.StatusCode
	}
	if code := post("null"); code != http.StatusForbidden {
		t.Fatalf("Origin null: %d", code)
	}
	resp, _ := f.get(b, "/login/code")
	if p := resp.Header.Get("Referrer-Policy"); p != "same-origin" {
		t.Fatalf("Referrer-Policy %q", p)
	}
	if code := post(f.srv.URL); code != http.StatusSeeOther {
		t.Fatalf("own origin over plain HTTP: %d", code)
	}
}
