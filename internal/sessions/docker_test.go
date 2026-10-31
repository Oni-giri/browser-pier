//go:build docker

// These tests start real browser containers through the session manager.
// They need Docker (or Podman's Docker API) and BROWSERPIER_TEST_IMAGE; see
// `make test-docker`. BROWSERPIER_TEST_SITE_HOST is how a browser container
// reaches this machine (default: the Docker bridge's gateway), and
// BROWSERPIER_USERNS_MODE is passed through ("keep-id" under rootless Podman).
package sessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/archive"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

type realFixture struct {
	t      *testing.T
	dir    string
	layout Layout
	rt     *docker.Client
}

func newRealFixture(t *testing.T) *realFixture {
	t.Helper()
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	// Each test is its own BrowserPier: tests running in parallel must not
	// take each other's containers for strays.
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	dir, err := os.MkdirTemp("", "bp") // short, for socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := rt.UseDataDir(context.Background(), dir, dir); err != nil {
		t.Fatal(err)
	}
	return &realFixture{t: t, dir: dir, rt: rt,
		layout: Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}}
}

// manager opens a Manager as a freshly started orchestrator would.
func (f *realFixture) manager(layout Layout) *Manager {
	f.t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(f.dir, "browserpier.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := NewManager(d, f.rt, layout, slog.New(slog.NewTextHandler(testWriter{f.t}, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		// Leave nothing running, even if the test failed half way.
		for _, s := range m.List() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, _ = m.Stop(ctx, s.ID)
			cancel()
		}
		m.Close()
		_ = d.Close()
	})
	return m
}

func (f *realFixture) profile(m *Manager) string {
	f.t.Helper()
	p := profiles.New(fmt.Sprintf("t%d", time.Now().UnixNano()%1e9))
	p.Width, p.Height = 1280, 800
	// The test sites are on the Docker host: the local network.
	p.LocalNetwork = true
	if _, err := m.Profiles().Create(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerName(p.Name)).Run() })
	return p.Name
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

func siteHost(t *testing.T) string {
	if h := os.Getenv("BROWSERPIER_TEST_SITE_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("can't find the bridge gateway (%v); set BROWSERPIER_TEST_SITE_HOST", err)
	}
	return strings.TrimSpace(string(out))
}

func startRunning(t *testing.T, m *Manager, profile string) Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, attached, err := m.Start(ctx, profile, "")
	if err != nil || attached || s.State != Running {
		t.Fatalf("Start = %+v, attached %t, %v", s, attached, err)
	}
	return s
}

func stopCleanly(t *testing.T, m *Manager, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := m.Stop(ctx, id)
	if err != nil || s.State != Stopped {
		t.Fatalf("Stop = %+v, %v", s, err)
	}
}

// TestLoginSurvivesStopAndStart is PROJ-5's done-when, end to end through
// the orchestrator: log in, stop, start again, still logged in.
func TestLoginSurvivesStopAndStart(t *testing.T) {
	f := newRealFixture(t)
	m := f.manager(f.layout)
	name := f.profile(m)
	site := browsertest.NewSite(t, siteHost(t))

	s := startRunning(t, m, name)
	control := browsertest.ControlClient(f.layout.ControlSocket(name))
	p := browsertest.Page(t, control)
	token := site.Login(t, p)
	p.Close()

	again, attached, err := m.Start(context.Background(), name, profiles.ModeAPI)
	if err != nil || !attached || again.ID != s.ID {
		t.Fatalf("second Start = %+v, attached %t, %v; want the running session", again, attached, err)
	}
	stopCleanly(t, m, s.ID)

	// The profile's files belong to the orchestrator's user, so it can
	// export them and read downloads.
	st, err := os.Stat(filepath.Join(f.dir, "profiles", name, "chrome", "Default", "Cookies"))
	if err != nil {
		t.Fatal(err)
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
		t.Fatalf("Cookies is owned by uid %d, want %d", uid, os.Getuid())
	}

	next := startRunning(t, m, name)
	if next.ID == s.ID {
		t.Fatal("a new start reused the old session")
	}
	if got := site.Account(t, browsertest.Page(t, control)); got != token {
		t.Fatalf("after stop and start the site saw session %q, want %q", got, token)
	}
	stopCleanly(t, m, next.ID)
}

// TestLoginSurvivesExportAndImport: a profile exported and imported under
// another name is still logged in.
func TestLoginSurvivesExportAndImport(t *testing.T) {
	f := newRealFixture(t)
	m := f.manager(f.layout)
	name := f.profile(m)
	site := browsertest.NewSite(t, siteHost(t))

	s := startRunning(t, m, name)
	p := browsertest.Page(t, browsertest.ControlClient(f.layout.ControlSocket(name)))
	token := site.Login(t, p)
	p.Close()
	stopCleanly(t, m, s.ID)

	release, err := m.Hold(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Start(context.Background(), name, ""); !errors.Is(err, ErrProfileHeld) {
		t.Fatalf("start during an export: %v", err)
	}
	var buf bytes.Buffer
	const pass = "correct horse battery"
	if err := archive.Export(&buf, f.layout.ProfileDir(name), archive.Manifest{}, pass); err != nil {
		t.Fatal(err)
	}
	release()

	copyName := name + "c"
	if _, err := archive.Import(&buf, pass, f.layout.ProfileDir(copyName), archive.DefaultLimits); err != nil {
		t.Fatal(err)
	}
	cp := profiles.New(copyName)
	cp.Width, cp.Height, cp.LocalNetwork = 1280, 800, true
	if _, err := m.Profiles().Create(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerName(copyName)).Run() })

	next := startRunning(t, m, copyName)
	if got := site.Account(t, browsertest.Page(t, browsertest.ControlClient(f.layout.ControlSocket(copyName)))); got != token {
		t.Fatalf("the imported copy is signed in as %q, want %q", got, token)
	}
	stopCleanly(t, m, next.ID)
}

// TestBrowserSurvivesOrchestratorRestart: restarting the orchestrator must
// not stop browsers; the new one picks them up.
func TestBrowserSurvivesOrchestratorRestart(t *testing.T) {
	f := newRealFixture(t)
	m := f.manager(f.layout)
	name := f.profile(m)
	s := startRunning(t, m, name)
	m.Close()

	m2 := f.manager(f.layout)
	got, ok := m2.Live(name)
	if !ok || got.ID != s.ID || got.State != Running {
		t.Fatalf("after restart: %+v, %t; want the same running session", got, ok)
	}
	// The adopted browser still answers, with its sandbox on under the
	// orchestrator's container settings.
	p := browsertest.Page(t, browsertest.ControlClient(f.layout.ControlSocket(name)))
	p.Navigate("chrome://sandbox/")
	deadline := time.Now().Add(10 * time.Second)
	for {
		text, _ := p.Eval("document.body.innerText").(string)
		if strings.Contains(text, "You are adequately sandboxed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chrome://sandbox says:\n%s", text)
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.Close()
	stopCleanly(t, m2, s.ID)
}

// TestWrongHostPathFailsLoudly: if the orchestrator's idea of the data
// dir's host path is wrong, the browser must refuse to start rather than
// open an empty profile.
func TestWrongHostPathFailsLoudly(t *testing.T) {
	f := newRealFixture(t)
	elsewhere, err := os.MkdirTemp("", "bpx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(elsewhere) })
	wrong := f.layout
	wrong.HostDataDir = elsewhere
	m := f.manager(wrong)
	name := f.profile(m)
	for _, sub := range []string{"chrome", "run", "files"} { // Docker refuses bind sources that don't exist
		if err := os.MkdirAll(filepath.Join(elsewhere, "profiles", name, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, _, err := m.Start(ctx, name, "")
	if !errors.Is(err, ErrStartFailed) || s.State != Failed {
		t.Fatalf("Start = %+v, %v; want a failure", s, err)
	}
	if !strings.Contains(s.LastLog, "not the folder the orchestrator prepared") {
		t.Fatalf("last log line %q doesn't explain the mount mismatch", s.LastLog)
	}
	if entries, _ := os.ReadDir(filepath.Join(elsewhere, "profiles", name, "chrome")); len(entries) != 0 {
		t.Fatalf("Chromium wrote into the wrong folder: %v", entries)
	}
}
