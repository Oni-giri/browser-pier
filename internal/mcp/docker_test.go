//go:build docker

// End to end through MCP with a real browser container; see
// internal/sessions/docker_test.go for the environment it needs.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// TestAgentLogsInThroughMCP is the done-when through MCP, in Visual mode:
// an agent signs in to a site with only the keyboard and the screen,
// asks a person for help and gets the browser back, stops the browser,
// starts it again and is still signed in.
func TestAgentLogsInThroughMCP(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
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
	name := fmt.Sprintf("m%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.Width, p.Height = 1280, 800
	// The test site is on the Docker host: the local network.
	p.LocalNetwork = true
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(d)
	_, secret, err := tokens.Create(ctx, "mcp-e2e", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.SetPIN(ctx, "4821"); err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	a := api.New(m, web, files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), log, "")
	handoffs := handoff.New(d, m, log)
	a.SetHandoffs(handoffs)
	routes, mux := http.NewServeMux(), http.NewServeMux()
	a.Register(routes)
	a.Register(mux)
	New(routes, web, log, "test").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	host := os.Getenv("BROWSERPIER_TEST_SITE_HOST")
	if host == "" {
		out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
		if err != nil {
			t.Fatalf("can't find the bridge gateway (%v); set BROWSERPIER_TEST_SITE_HOST", err)
		}
		host = strings.TrimSpace(string(out))
	}
	site := browsertest.NewSite(t, host)

	f := &fixture{t: t, srv: srv}
	cs := f.connect(secret, versions[0], func(r *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		go func() { // the person hands back
			time.Sleep(300 * time.Millisecond)
			if h, err := handoffs.Get(context.Background(), r.Params.ElicitationID); err == nil {
				_, _ = handoffs.Finish(context.Background(), h, handoff.Done, "")
			}
		}()
		return &sdk.ElicitResult{Action: "accept"}, nil
	})
	_, out := call(t, cs, "start_browser", map[string]any{"profile": name})
	live, _ := m.Live(name)
	if !strings.Contains(out, live.ID) {
		t.Fatalf("start_browser: %s", out)
	}
	id := live.ID
	visit := func(url string) {
		t.Helper()
		call(t, cs, "press_keys", map[string]any{"session_id": id, "keys": "ctrl+l"})
		call(t, cs, "type_text", map[string]any{"session_id": id, "text": url})
		call(t, cs, "press_keys", map[string]any{"session_id": id, "keys": "Return"})
		call(t, cs, "wait", map[string]any{"session_id": id, "seconds": 2})
	}

	res, out := call(t, cs, "screenshot", map[string]any{"session_id": id})
	if res.IsError || !strings.Contains(out, "1152×720") {
		t.Fatalf("screenshot: %s", out)
	}
	// Sign in with the keyboard: the page has the user field first.
	visit(site.URL + "/login")
	call(t, cs, "press_keys", map[string]any{"session_id": id, "keys": "Tab"})
	call(t, cs, "type_text", map[string]any{"session_id": id, "text": "pier"})
	call(t, cs, "press_keys", map[string]any{"session_id": id, "keys": "Tab"})
	call(t, cs, "type_text", map[string]any{"session_id": id, "text": "hunter2"})
	res, out = call(t, cs, "press_keys", map[string]any{"session_id": id, "keys": "Return", "screenshot": true})
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("press_keys with a screenshot: %s", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for site.Issued() == "" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if site.Issued() == "" {
		if r := site.Rejected(); r != "" {
			t.Fatalf("the login form was sent with %s", r)
		}
		t.Fatal("the login form was never submitted")
	}

	// A key named like an xdotool command would make xdotool run it.
	res, out = call(t, cs, "press_keys", map[string]any{"session_id": id, "keys": "Return exec touch pwned"})
	if !res.IsError {
		t.Fatalf("press_keys with a command in it: %s", out)
	}
	found, err := exec.Command("docker", "exec", "browserpier-"+name, "sh", "-c", "find / -xdev -name pwned 2>/dev/null; echo checked").Output()
	if err != nil || strings.TrimSpace(string(found)) != "checked" {
		t.Fatalf("xdotool ran a program, or the check didn't run: %q, %v", found, err)
	}

	// A person helps, and the agent gets the browser back.
	_, out = call(t, cs, "request_handoff", map[string]any{"session_id": id, "reason": "review", "detail": "Check the account page"})
	if !strings.Contains(out, "done") {
		t.Fatalf("request_handoff: %s", out)
	}

	call(t, cs, "stop_browser", map[string]any{"session_id": id})
	_, out = call(t, cs, "start_browser", map[string]any{"profile": name})
	live, _ = m.Live(name)
	if !strings.Contains(out, live.ID) {
		t.Fatalf("restart: %s", out)
	}
	id = live.ID
	visit(site.URL + "/account")
	deadline = time.Now().Add(10 * time.Second)
	for site.Seen() != site.Issued() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if site.Seen() != site.Issued() {
		t.Fatalf("not signed in after a restart: the site saw %q, issued %q", site.Seen(), site.Issued())
	}
	res, _ = call(t, cs, "zoom", map[string]any{"session_id": id, "x": 0, "y": 0, "width": 400, "height": 200})
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("zoom: %+v", res)
	}
}
