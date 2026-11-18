package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
	"github.com/Oni-giri/browser-pier/internal/settings"
	"github.com/Oni-giri/browser-pier/internal/ui"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	h, _, _ := testServer(t)
	return h
}

// testServer is the whole handler, with its admin account and tokens.
func testServer(t *testing.T) (http.Handler, *auth.Admin, *auth.Tokens) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	admin, err := auth.NewAdmin(context.Background(), d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(d)
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	mgr, err := sessions.NewManager(d, sessionstest.New(dir), sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	app := ui.New(mgr, web, handoff.New(d, mgr, log), settings.New(d), log, ui.Options{})
	return newHandler(web, api.New(mgr, web, files.New(mgr.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), log, ""), nil, nil, app, log), admin, tokens
}

func TestHealthz(t *testing.T) {
	h := testHandler(t)
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusSeeOther}, // to the sign-in page
		{http.MethodGet, "/login", http.StatusOK},
		{http.MethodGet, "/api/profiles", http.StatusUnauthorized}, // no token
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.code)
		}
	}
}

// Over plain HTTP on any address but localhost (a LAN or Tailscale IP),
// browsers send no Sec-Fetch-Site, so the cross-origin check compares Origin
// with Host. The sign-in page must let its form post carry its origin.
func TestSignInOverPlainHTTP(t *testing.T) {
	h := testHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://100.64.0.7:7436/login", nil))
	if p := rec.Header().Get("Referrer-Policy"); p != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", p)
	}
	if strings.Contains(rec.Body.String(), "no-referrer") {
		t.Error("the sign-in page sets no-referrer, under which its form posts Origin: null")
	}

	for _, tc := range []struct {
		origin string
		code   int
	}{
		{"http://100.64.0.7:7436", http.StatusSeeOther},
		{"null", http.StatusForbidden},
		{"http://elsewhere.example", http.StatusForbidden},
	} {
		form := url.Values{"password": {"the-admin-password"}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "http://100.64.0.7:7436/login", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", tc.origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Errorf("sign-in with Origin %s = %d, want %d", tc.origin, rec.Code, tc.code)
		}
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, testHandler(t), slog.New(slog.DiscardHandler)) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

func TestDefaultBrowserImage(t *testing.T) {
	for version, want := range map[string]string{
		"v1.2.3":              "ghcr.io/oni-giri/browser-pier-chromium:1.2.3",
		"v1.2.3-4-gabcdef0":   "ghcr.io/oni-giri/browser-pier-chromium:edge",
		"dev":                 "ghcr.io/oni-giri/browser-pier-chromium:edge",
		"abcdef0-dirty":       "ghcr.io/oni-giri/browser-pier-chromium:edge",
		"v1.2.3\nevil:latest": "ghcr.io/oni-giri/browser-pier-chromium:edge",
	} {
		if got := defaultBrowserImage(version); got != want {
			t.Errorf("defaultBrowserImage(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestBrowserUser(t *testing.T) {
	if u, g := browserUser(1001, 1002); u != 1001 || g != 1002 {
		t.Errorf("browserUser(1001, 1002) = %d, %d", u, g)
	}
	if u, g := browserUser(0, 0); u != 1000 || g != 1000 {
		t.Errorf("browserUser(0, 0) = %d, %d; Chromium can't run as root", u, g)
	}
}

func TestInstanceIDIsKept(t *testing.T) {
	dir := t.TempDir()
	a, err := instanceID(dir)
	if err != nil || len(a) != 16 {
		t.Fatalf("instanceID = %q, %v", a, err)
	}
	if b, _ := instanceID(dir); b != a {
		t.Fatalf("second call gave %q, want %q", b, a)
	}
	if c, _ := instanceID(t.TempDir()); c == a {
		t.Fatal("two data dirs share an instance ID")
	}
}

func TestParseMeminfo(t *testing.T) {
	n, err := parseMeminfo("MemTotal:       16303512 kB\nMemFree:  1 kB\n", "MemTotal")
	if err != nil || n != 16303512*1024 {
		t.Fatalf("parseMeminfo = %d, %v", n, err)
	}
	if n, err := parseMeminfo("MemTotal: 1 kB\nMemAvailable:  2048 kB\n", "MemAvailable"); n != 2<<20 || err != nil {
		t.Fatalf("MemAvailable = %d, %v", n, err)
	}
	if _, err := parseMeminfo("nothing", "MemTotal"); err == nil {
		t.Fatal("want an error")
	}
	if n, err := hostMemory(); err != nil || n <= 0 {
		t.Fatalf("hostMemory = %d, %v", n, err)
	}
}

func TestPrivateDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	privateDir(slog.New(slog.DiscardHandler), dir)
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestLocalFree(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	if localFree() == nil {
		t.Fatal("no free memory for a local daemon")
	}
	t.Setenv("DOCKER_HOST", "tcp://elsewhere:2375")
	if localFree() != nil {
		t.Fatal("this host's free memory used for a remote daemon")
	}
}

// Two-step sign-in changes nothing for agents: a token still works on the
// API, CDP and MCP, and the admin cookie still doesn't (PROJ-162).
func TestAgentsUnaffectedByTwoStep(t *testing.T) {
	h, admin, tokens := testServer(t)
	ctx := context.Background()
	secret := auth.NewTOTPSecret()
	if _, err := admin.EnableTwoStep(ctx, secret, auth.TOTPCode(secret, time.Now()), ""); err != nil {
		t.Fatal(err)
	}
	_, token, err := tokens.Create(ctx, "agent", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := admin.NewSession(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, header ...string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	mcpInit := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	mcpHeaders := []string{"Content-Type", "application/json", "Accept", "application/json, text/event-stream"}
	if code := call(http.MethodGet, "/api/profiles", "", "Authorization", "Bearer "+token); code != http.StatusOK {
		t.Fatalf("API with a token: %d", code)
	}
	if code := call(http.MethodPost, "/mcp", mcpInit, append(mcpHeaders, "Authorization", "Bearer "+token)...); code != http.StatusOK {
		t.Fatalf("MCP with a token: %d", code)
	}
	// CDP for a profile that doesn't exist: the token is accepted (404), not refused (401).
	if code := call(http.MethodGet, "/cdp/nope/json/version", "", "Authorization", "Bearer "+token); code == http.StatusUnauthorized {
		t.Fatalf("CDP with a token: %d", code)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/profiles", ""}, {http.MethodGet, "/cdp/nope/json/version", ""}, {http.MethodPost, "/mcp", mcpInit},
	} {
		if code := call(c.method, c.path, c.body, append(mcpHeaders, "Cookie", auth.CookieName+"="+cookie)...); code != http.StatusUnauthorized {
			t.Errorf("%s with the admin cookie: %d, want 401", c.path, code)
		}
	}
}
