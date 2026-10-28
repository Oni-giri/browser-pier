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
	"testing"
	"time"
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
	if !w.IsHTTPS(tlsReq) || !w.secureCookie(tlsReq) {
		t.Error("direct TLS not seen as HTTPS")
	}
	if w.secureCookie(req("203.0.113.9:1", "", "")) {
		t.Error("Secure cookie over plain HTTP under auto")
	}
	w.opts.PublicURL = "https://pier.example.com"
	if !w.secureCookie(req("203.0.113.9:1", "", "")) {
		t.Error("an https public URL should make cookies Secure")
	}
	w.opts.CookieSecure = "never"
	if w.secureCookie(tlsReq) {
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
		b.fails = nil
		for range n {
			b.fail()
		}
		return b.delay()
	}
	for n, want := range map[int]time.Duration{0: 0, 19: 0, 20: 2 * time.Second, 39: 2 * time.Second, 40: 4 * time.Second, 60: 8 * time.Second, 100: 30 * time.Second, 1000: 30 * time.Second} {
		if got := at(n); got != want {
			t.Errorf("after %d failures the delay is %s, want %s", n, got, want)
		}
	}
	at(100)
	now = now.Add(16 * time.Minute)
	if d := b.delay(); d != 0 {
		t.Fatalf("delay %s after the window passed", d)
	}
}
