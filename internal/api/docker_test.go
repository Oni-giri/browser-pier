//go:build docker

// End to end through the HTTP API with real browser containers; see
// internal/sessions/docker_test.go for the environment it needs.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

type agent struct {
	t     *testing.T
	base  string
	token string
}

func (a agent) do(method, path, body string, out any) int {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			a.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

func (a agent) header() http.Header { return http.Header{"Authorization": {"Bearer " + a.token}} }

func newRealAPI(t *testing.T) agent {
	t.Helper()
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := rt.UseDataDir(ctx, dir, dir); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range m.List() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, _ = m.Stop(ctx, s.ID)
			cancel()
		}
		m.Close()
	})
	tokens := auth.NewTokens(d)
	_, secret, err := tokens.Create(ctx, "e2e", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	mux := http.NewServeMux()
	New(m, web, log, "").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return agent{t: t, base: srv.URL, token: secret}
}

func siteHost(t *testing.T) string {
	if h := os.Getenv("BROWSERPIER_TEST_SITE_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		t.Fatalf("can't find the bridge gateway (%v); set BROWSERPIER_TEST_SITE_HOST", err)
	}
	return strings.TrimSpace(string(out))
}

// TestAgentKeepsItsLoginThroughTheAPI is the done-when through the API an
// agent uses: create a profile, start it, log in over the CDP proxy, stop
// it, start it again and find the login still there.
func TestAgentKeepsItsLoginThroughTheAPI(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewSite(t, siteHost(t))
	name := fmt.Sprintf("e%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })

	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "api", "screen": {"width": 1280, "height": 800}}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.State != "running" {
		t.Fatalf("start: %d %+v", code, s)
	}
	cdpHTTP := strings.Replace(s.CDPURL, "ws://", "http://", 1)
	p := browsertest.PageVia(t, cdpHTTP+"/json/list", a.header())
	token := site.Login(t, p)
	p.Close()

	// The screenshot comes from the X display, at the size asked for.
	resp, err := http.DefaultClient.Do(func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, a.base+"/api/sessions/"+s.ID+"/screenshot?width=640", nil)
		req.Header = a.header()
		return req
	}())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(resp.Body)
	_ = resp.Body.Close()
	if err != nil || img.Bounds().Dx() != 640 || img.Bounds().Dy() != 400 {
		t.Fatalf("screenshot: %v, %v", err, img.Bounds())
	}

	var stopped sessionJSON
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", &stopped); code != http.StatusOK || stopped.State != "stopped" {
		t.Fatalf("stop: %d %+v", code, stopped)
	}
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	if got := site.Account(t, browsertest.PageVia(t, cdpHTTP+"/json/list", a.header())); got != token {
		t.Fatalf("after stop and start the site saw session %q, want %q", got, token)
	}
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("final stop: %d", code)
	}

	// In Visual mode nothing may attach over CDP, but the screen and input work.
	var vis sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": "visual"}`, name), &vis); code != http.StatusCreated || vis.CDPURL != "" {
		t.Fatalf("visual start: %d %+v", code, vis)
	}
	s = vis
	if code := a.do(http.MethodGet, "/cdp/"+name+"/json/version", "", nil); code != http.StatusConflict {
		t.Fatalf("CDP in Visual mode: %d", code)
	}
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 640, "y": 400}`, nil); code != http.StatusNoContent {
		t.Fatalf("click: %d", code)
	}
}
