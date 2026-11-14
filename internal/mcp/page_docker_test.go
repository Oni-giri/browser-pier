//go:build docker

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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// TestAgentLogsInWithPageTools is scenario 15: a scripted MCP client
// signs in to the test site in API mode with only the page tools
// (navigate, snapshot, act, wait_for, find), then stops the browser,
// starts it again and is still signed in.
func TestAgentLogsInWithPageTools(t *testing.T) {
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
	name := fmt.Sprintf("p%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.DefaultMode = profiles.ModeAPI
	p.LocalNetwork = true // the test site is on the Docker host
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(d)
	_, secret, err := tokens.Create(ctx, "mcp-page-tools", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	a := api.New(m, web, files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), log, "")
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
	cs := f.connect(secret, versions[0], nil)
	tool := func(name string, args map[string]any) string {
		t.Helper()
		res, out := call(t, cs, name, args)
		if res.IsError {
			t.Fatalf("%s %v: %s", name, args, out)
		}
		return out
	}
	_, out := call(t, cs, "start_browser", map[string]any{"profile": name})
	live, _ := m.Live(name)
	if !strings.Contains(out, live.ID) || !strings.Contains(out, "api mode") {
		t.Fatalf("start_browser: %s", out)
	}
	id := live.ID
	tool("navigate", map[string]any{"session_id": id, "url": site.URL + "/login"})
	snap := tool("snapshot", map[string]any{"session_id": id})
	// The site's fields have no labels: the snapshot lists them in order.
	fields := regexp.MustCompile(`(?m)^([a-z][a-z0-9]e[0-9]+) textbox `).FindAllStringSubmatch(snap, -1)
	button := regexp.MustCompile(`(?m)^([a-z][a-z0-9]e[0-9]+) button "Sign in"`).FindStringSubmatch(snap)
	if len(fields) != 2 || button == nil {
		t.Fatalf("snapshot of the login page:\n%s", snap)
	}
	out = tool("act", map[string]any{"session_id": id, "steps": []any{
		map[string]any{"action": "fill", "ref": fields[0][1], "text": "pier"},
		map[string]any{"action": "fill", "ref": fields[1][1], "text": "hunter2"},
		map[string]any{"action": "click", "ref": button[1]},
	}})
	if strings.Contains(out, "hunter2") {
		t.Errorf("the password is in act's result:\n%s", out)
	}
	if out := tool("wait_for", map[string]any{"session_id": id, "text": "Signed in"}); out != "ok" {
		t.Fatalf("wait_for: %s (rejected: %s)", out, site.Rejected())
	}
	if site.Issued() == "" {
		t.Fatalf("not signed in; the form sent %s", site.Rejected())
	}

	// Stop, start again, and the login is still there.
	tool("stop_browser", map[string]any{"session_id": id})
	_, out = call(t, cs, "start_browser", map[string]any{"profile": name})
	live, _ = m.Live(name)
	if !strings.Contains(out, live.ID) {
		t.Fatalf("restart: %s", out)
	}
	id = live.ID
	tool("navigate", map[string]any{"session_id": id, "url": site.URL + "/account"})
	if out := tool("find", map[string]any{"session_id": id, "text": "signed in as pier"}); !strings.Contains(out, "1 match.") {
		t.Fatalf("not signed in after a restart: %s", out)
	}
	if site.Seen() != site.Issued() {
		t.Fatalf("the site saw %q, issued %q", site.Seen(), site.Issued())
	}
}
