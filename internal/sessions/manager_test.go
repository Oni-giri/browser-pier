package sessions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

type fixture struct {
	t      *testing.T
	dir    string
	rt     *fakeRuntime
	m      *Manager
	layout Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// Not t.TempDir: its long paths would overflow a Unix socket path.
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fixture{t: t, dir: dir, rt: newFakeRuntime(dir),
		layout: Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}}
	f.m = f.manager()
	if _, err := f.m.Profiles().Create(context.Background(), profiles.New("google")); err != nil {
		t.Fatal(err)
	}
	return f
}

// manager opens a new Manager on the fixture's database and runtime, as a
// restarted orchestrator would.
func (f *fixture) manager() *Manager {
	f.t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(f.dir, "browserpier.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := NewManager(d, f.rt, f.layout, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		m.Close()
		_ = d.Close()
	})
	return m
}

func (f *fixture) start(m *Manager) Session {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, attached, err := m.Start(ctx, "google", "")
	if err != nil || attached {
		f.t.Fatalf("Start = %+v, attached %t, %v", s, attached, err)
	}
	return s
}

func (f *fixture) stored(id string) Session {
	f.t.Helper()
	s, err := getSession(context.Background(), f.m.db, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartAttachStop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	s := f.start(f.m)
	if s.State != Running || s.Mode != profiles.ModeVisual || s.ContainerID == "" {
		t.Fatalf("started session %+v", s)
	}
	if got := f.stored(s.ID); got.State != Running {
		t.Fatalf("stored state %s", got.State)
	}

	again, attached, err := f.m.Start(ctx, "google", profiles.ModeAPI)
	if err != nil || !attached || again.ID != s.ID {
		t.Fatalf("second Start = %+v, attached %t, %v; want the same session", again, attached, err)
	}
	if live := f.m.List(); len(live) != 1 || live[0].ID != s.ID {
		t.Fatalf("List = %+v", live)
	}

	stopped, err := f.m.Stop(ctx, s.ID)
	if err != nil || stopped.State != Stopped {
		t.Fatalf("Stop = %+v, %v", stopped, err)
	}
	if got := f.stored(s.ID); got.State != Stopped {
		t.Fatalf("stored state %s", got.State)
	}
	if f.rt.count() != 0 {
		t.Fatal("the stopped container wasn't removed")
	}
	if _, ok := f.m.Live("google"); ok {
		t.Fatal("profile still has a live session")
	}
	// Stopping again is harmless and returns the final session.
	if again, err := f.m.Stop(ctx, s.ID); err != nil || again.State != Stopped {
		t.Fatalf("second Stop = %+v, %v", again, err)
	}

	next := f.start(f.m)
	if next.ID == s.ID {
		t.Fatal("a new start reused the old session ID")
	}
}

func TestContainerSpec(t *testing.T) {
	f := newFixture(t)
	p, _ := f.m.Profiles().Get(context.Background(), "google")
	p.Width, p.Height = 1280, 800
	if _, err := f.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	s := f.start(f.m)
	c, _ := f.rt.container(s.ContainerID)
	spec := c.spec
	if spec.Name != "browserpier-google" || spec.Profile != "google" || spec.Session != s.ID || spec.Screen != "1280x800" {
		t.Fatalf("spec %+v", spec)
	}
	if spec.ProfileDir != filepath.Join(f.dir, "profiles", "google", "chrome") || spec.ControlDir != filepath.Join(f.dir, "profiles", "google", "run") {
		t.Fatalf("spec dirs %s, %s", spec.ProfileDir, spec.ControlDir)
	}
	if want := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()); spec.User != want {
		t.Fatalf("user %s, want %s", spec.User, want)
	}
	if spec.MountCheck != s.ID {
		t.Fatalf("mount check %q, want the session ID", spec.MountCheck)
	}
	for _, dir := range []string{spec.ProfileDir, spec.ControlDir} {
		b, err := os.ReadFile(filepath.Join(dir, mountMarker))
		if err != nil || strings.TrimSpace(string(b)) != s.ID {
			t.Fatalf("marker in %s = %q, %v", dir, b, err)
		}
	}
}

func TestConcurrentStartsShareOneBrowser(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	fresh := make(chan bool, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, attached, err := f.m.Start(context.Background(), "google", "")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- s.ID
			fresh <- !attached
		}()
	}
	wg.Wait()
	close(ids)
	close(fresh)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("got sessions %s and %s for one profile", first, id)
		}
	}
	n := 0
	for f := range fresh {
		if f {
			n++
		}
	}
	if n != 1 || f.rt.creates != 1 {
		t.Fatalf("%d callers started a browser and %d containers were created; want 1 and 1", n, f.rt.creates)
	}
}

func TestStartFailsWhenTheBrowserExits(t *testing.T) {
	f := newFixture(t)
	status := 1
	f.rt.exitOnStart = &status
	f.rt.lastLog = `level=ERROR msg=exiting err="profile /profile is not writable"`

	s, _, err := f.m.Start(context.Background(), "google", "")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("Start err = %v, want ErrStartFailed", err)
	}
	if s.State != Failed || s.LastLog != f.rt.lastLog || !strings.Contains(s.Error, "exit status 1") {
		t.Fatalf("failed session %+v", s)
	}
	if got := f.stored(s.ID); got.State != Failed || got.LastLog != f.rt.lastLog {
		t.Fatalf("stored %+v", got)
	}
	if f.rt.count() != 0 {
		t.Fatal("the failed container wasn't removed")
	}

	// Retry works once the cause is gone.
	f.rt.exitOnStart = nil
	if s := f.start(f.m); s.State != Running {
		t.Fatalf("retry: %+v", s)
	}
}

func TestBrowserCrashFailsTheSession(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.rt.lastLog = "level=ERROR msg=exiting err=\"chromium exited by itself\""
	f.rt.exit(s.ContainerID, 1)
	eventually(t, "the session to fail", func() bool { return f.stored(s.ID).State == Failed })
	got := f.stored(s.ID)
	if !strings.Contains(got.Error, "unexpectedly") || got.LastLog != f.rt.lastLog {
		t.Fatalf("stored %+v", got)
	}
	if _, ok := f.m.Live("google"); ok {
		t.Fatal("crashed session still live")
	}
}

func TestUncleanStopFailsTheSession(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.rt.stopStatus = 1 // the entrypoint's SIGTERM fallback
	got, err := f.m.Stop(context.Background(), s.ID)
	if err != nil || got.State != Failed || !strings.Contains(got.Error, "logins may be lost") {
		t.Fatalf("Stop = %+v, %v", got, err)
	}
}

func TestReadyTimeout(t *testing.T) {
	f := newFixture(t)
	f.rt.unhealthy = true
	f.m.readyTimeout = 300 * time.Millisecond
	s, _, err := f.m.Start(context.Background(), "google", "")
	if !errors.Is(err, ErrStartFailed) || s.State != Failed || !strings.Contains(s.Error, "wasn't ready") {
		t.Fatalf("Start = %+v, %v", s, err)
	}
	if f.rt.stops != 1 || f.rt.count() != 0 {
		t.Fatalf("stops %d, containers left %d; want the container stopped and removed", f.rt.stops, f.rt.count())
	}
}

func TestStartModeAndErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.m.Start(ctx, "missing", ""); !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("unknown profile: %v", err)
	}
	if _, _, err := f.m.Start(ctx, "google", "cdp"); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, err := f.m.Stop(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stop unknown = %v", err)
	}
	s, _, err := f.m.Start(ctx, "google", profiles.ModeAPI)
	if err != nil || s.Mode != profiles.ModeAPI {
		t.Fatalf("Start api = %+v, %v", s, err)
	}
}

func TestNeedsHelp(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	got, err := f.m.SetNeedsHelp(s.ID, true)
	if err != nil || got.State != NeedsHelp || f.stored(s.ID).State != NeedsHelp {
		t.Fatalf("SetNeedsHelp(true) = %+v, %v", got, err)
	}
	// An agent asking for the browser while a human has it still gets it.
	if again, attached, err := f.m.Start(context.Background(), "google", ""); err != nil || !attached || again.State != NeedsHelp {
		t.Fatalf("Start during handoff = %+v, %t, %v", again, attached, err)
	}
	if got, err := f.m.SetNeedsHelp(s.ID, false); err != nil || got.State != Running {
		t.Fatalf("SetNeedsHelp(false) = %+v, %v", got, err)
	}
	// Stopping from Needs help works too.
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, err := f.m.Stop(context.Background(), s.ID); err != nil || got.State != Stopped {
		t.Fatalf("Stop = %+v, %v", got, err)
	}
	if _, err := f.m.SetNeedsHelp(s.ID, true); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("SetNeedsHelp on a stopped session = %v", err)
	}
}

func TestReconcileAfterRestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.Profiles().Create(ctx, profiles.New("github")); err != nil {
		t.Fatal(err)
	}
	running := f.start(f.m)
	gone, _, err := f.m.Start(ctx, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	f.m.Close() // the orchestrator goes down; browsers keep running

	// While it's down, one container disappears and a stray one appears.
	f.rt.exit(gone.ContainerID, 0)
	if err := f.rt.Remove(ctx, gone.ContainerID); err != nil {
		t.Fatal(err)
	}
	f.rt.mu.Lock()
	f.rt.containers["stray"] = &fakeContainer{id: "stray", state: "running", exited: make(chan struct{})}
	f.rt.containers["stray"].spec.Name = "browserpier-stray"
	f.rt.mu.Unlock()

	m := f.manager()
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := m.Live("google"); !ok || s.ID != running.ID || s.State != Running {
		t.Fatalf("google after restart: %+v, %t; want the same running session", s, ok)
	}
	if again, attached, err := m.Start(ctx, "google", ""); err != nil || !attached || again.ID != running.ID {
		t.Fatalf("Start after restart = %+v, %t, %v", again, attached, err)
	}
	if got := f.stored(gone.ID); got.State != Failed || !strings.Contains(got.Error, "gone") {
		t.Fatalf("session with a vanished container: %+v", got)
	}
	eventually(t, "the stray container to go", func() bool {
		_, ok := f.rt.container("stray")
		return !ok
	})
	if got, err := m.Stop(ctx, running.ID); err != nil || got.State != Stopped {
		t.Fatalf("Stop adopted session = %+v, %v", got, err)
	}
}

func TestDeleteProfile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	if err := f.m.DeleteProfile(ctx, "google"); !errors.Is(err, ErrProfileBusy) {
		t.Fatalf("delete while running = %v", err)
	}
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "profiles", "google")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := f.m.DeleteProfile(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("profile folder still there: %v", err)
	}
	if _, err := f.m.Get(ctx, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session history kept after delete: %v", err)
	}
}

func TestLayoutRejectsLongDataDir(t *testing.T) {
	long := "/" + strings.Repeat("d", 60)
	if err := (Layout{DataDir: long, HostDataDir: long}).check(); err == nil {
		t.Fatal("want an error for a data dir too long for socket paths")
	}
	if err := (Layout{DataDir: "data", HostDataDir: "/data"}).check(); err == nil {
		t.Fatal("want an error for a relative data dir")
	}
	if err := (Layout{DataDir: "/data", HostDataDir: "/srv/bp/data"}).check(); err != nil {
		t.Fatal(err)
	}
}
