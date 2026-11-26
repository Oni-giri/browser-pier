//go:build docker

package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// pageEnv is a real browser for an API-mode profile, an MCP endpoint in
// front of it and a client session with a token for every profile.
type pageEnv struct {
	m    *sessions.Manager
	cs   *sdk.ClientSession
	name string // the profile
	host string // how the browser reaches this machine
}

func newPageEnv(t *testing.T, prefix string) pageEnv {
	t.Helper()
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
	name := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1e9)
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
	f := &fixture{t: t, srv: srv}
	return pageEnv{m: m, cs: f.connect(secret, versions[0], nil), name: name, host: host}
}

// TestAgentLogsInWithPageTools is scenario 15: a scripted MCP client
// signs in to the test site in API mode with only the page tools
// (navigate, snapshot, act, wait_for, find), then stops the browser,
// starts it again and is still signed in.
func TestAgentLogsInWithPageTools(t *testing.T) {
	env := newPageEnv(t, "p")
	m, cs, name := env.m, env.cs, env.name
	site := browsertest.NewSite(t, env.host)
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
	if out := tool("wait_for", map[string]any{"session_id": id, "text": "Signed in"}); !strings.HasPrefix(out, "waited ") && !strings.HasPrefix(out, "already true when wait started: ") {
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

	// page_content (PROJ-20): the signed-in page as Markdown and as its
	// main text, fenced; a PDF as an embedded resource; a screenshot.
	md := tool("page_content", map[string]any{"session_id": id, "format": "markdown"})
	if !strings.HasPrefix(md, "<untrusted-content-") || !strings.Contains(md, "# Signed in as pier\n\nBrowserPier persistence test") {
		t.Errorf("markdown:\n%s", md)
	}
	if out := tool("page_content", map[string]any{"session_id": id, "format": "readability"}); !strings.Contains(out, "Signed in as pier\n\nBrowserPier persistence test") {
		t.Errorf("readability:\n%s", out)
	}
	res, out := call(t, cs, "page_content", map[string]any{"session_id": id, "format": "pdf"})
	if res.IsError || len(res.Content) != 2 || !strings.Contains(out, "PDF of the active tab") {
		t.Fatalf("pdf: %s", out)
	}
	if r, ok := res.Content[1].(*sdk.EmbeddedResource); !ok || r.Resource.MIMEType != "application/pdf" || !strings.HasPrefix(string(r.Resource.Blob), "%PDF-") {
		t.Errorf("pdf: %T", res.Content[1])
	}
	res, out = call(t, cs, "page_content", map[string]any{"session_id": id, "format": "screenshot"})
	if _, ok := res.Content[len(res.Content)-1].(*sdk.ImageContent); res.IsError || !ok {
		t.Errorf("screenshot: %s", out)
	}
}

// TestAgentUploadsWithPageTools is PROJ-117 end to end over MCP: an agent
// with no CDP client of its own puts a file in the session with
// upload_file, sets it on a form's file input with act's upload step and
// sends the form; the site gets the same bytes.
func TestAgentUploadsWithPageTools(t *testing.T) {
	env := newPageEnv(t, "up")
	site := browsertest.NewPageSite(t, env.host)
	tool := func(name string, args map[string]any) string {
		t.Helper()
		res, out := call(t, env.cs, name, args)
		if res.IsError {
			t.Fatalf("%s %v: %s", name, args, out)
		}
		return out
	}
	tool("start_browser", map[string]any{"profile": env.name})
	live, _ := env.m.Live(env.name)
	id := live.ID
	content := make([]byte, 200<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	out := tool("upload_file", map[string]any{"session_id": id, "name": "invoice..v2.pdf", "content_base64": base64.StdEncoding.EncodeToString(content)})
	if !strings.Contains(out, "name: invoice..v2.pdf") {
		t.Fatalf("upload_file: %s", out)
	}
	tool("navigate", map[string]any{"session_id": id, "url": site.URL + "/upload"})
	snap := tool("snapshot", map[string]any{"session_id": id})
	ref := func(what string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^([a-z][a-z0-9]e[0-9]+) ` + regexp.QuoteMeta(what)).FindStringSubmatch(snap)
		if m == nil {
			t.Fatalf("no %s in\n%s", what, snap)
		}
		return m[1]
	}
	out = tool("act", map[string]any{"session_id": id, "steps": []any{
		map[string]any{"action": "upload", "ref": ref(`button "Document"`), "files": []string{"invoice..v2.pdf"}},
		map[string]any{"action": "click", "ref": ref(`button "Send"`)},
	}})
	if !strings.Contains(out, "uploaded 1 file to") {
		t.Errorf("act: %s", out)
	}
	if out := tool("wait_for", map[string]any{"session_id": id, "text": "Uploaded"}); !strings.HasPrefix(out, "waited ") && !strings.HasPrefix(out, "already true when wait started: ") {
		t.Fatalf("wait_for: %s", out)
	}
	got := site.Uploads()
	if len(got) != 1 || got[0].Field != "doc" || got[0].Name != "invoice..v2.pdf" || got[0].Size != int64(len(content)) || got[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("the site got %+v, want invoice..v2.pdf, %d bytes, sha256 %x", got, len(content), sum)
	}
	// A button that opens a chooser itself is refused, saying what to do.
	tool("navigate", map[string]any{"session_id": id, "url": site.URL + "/upload"})
	snap = tool("snapshot", map[string]any{"session_id": id})
	res, msg := call(t, env.cs, "act", map[string]any{"session_id": id, "steps": []any{
		map[string]any{"action": "upload", "ref": ref(`button "Choose a file"`), "files": []string{"invoice..v2.pdf"}},
	}})
	if !res.IsError || !strings.Contains(msg, "choose_files") {
		t.Errorf("a chooser button: %s", msg)
	}
}
