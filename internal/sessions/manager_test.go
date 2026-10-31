package sessions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

type fixture struct {
	t      *testing.T
	dir    string
	rt     *sessionstest.FakeRuntime
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
	f := &fixture{t: t, dir: dir, rt: sessionstest.New(dir),
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
	if f.rt.Count() != 0 {
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
	c, _ := f.rt.Container(s.ContainerID)
	spec := c.Spec
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
	if n != 1 || f.rt.Creates != 1 {
		t.Fatalf("%d callers started a browser and %d containers were created; want 1 and 1", n, f.rt.Creates)
	}
}

func TestStartFailsWhenTheBrowserExits(t *testing.T) {
	f := newFixture(t)
	status := 1
	f.rt.ExitOnStart = &status
	f.rt.LastLog = `level=ERROR msg=exiting err="profile /profile is not writable"`

	s, _, err := f.m.Start(context.Background(), "google", "")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("Start err = %v, want ErrStartFailed", err)
	}
	if s.State != Failed || s.LastLog != f.rt.LastLog || !strings.Contains(s.Error, "exit status 1") {
		t.Fatalf("failed session %+v", s)
	}
	if got := f.stored(s.ID); got.State != Failed || got.LastLog != f.rt.LastLog {
		t.Fatalf("stored %+v", got)
	}
	if f.rt.Count() != 0 {
		t.Fatal("the failed container wasn't removed")
	}

	// Retry works once the cause is gone.
	f.rt.ExitOnStart = nil
	if s := f.start(f.m); s.State != Running {
		t.Fatalf("retry: %+v", s)
	}
}

func TestBrowserCrashFailsTheSession(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.rt.LastLog = "level=ERROR msg=exiting err=\"chromium exited by itself\""
	f.rt.Exit(s.ContainerID, 1)
	eventually(t, "the session to fail", func() bool { return f.stored(s.ID).State == Failed })
	got := f.stored(s.ID)
	if !strings.Contains(got.Error, "unexpectedly") || got.LastLog != f.rt.LastLog {
		t.Fatalf("stored %+v", got)
	}
	if _, ok := f.m.Live("google"); ok {
		t.Fatal("crashed session still live")
	}
}

func TestUncleanStopFailsTheSession(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.rt.StopStatus = 1 // the entrypoint's SIGTERM fallback
	got, err := f.m.Stop(context.Background(), s.ID)
	if err != nil || got.State != Failed || !strings.Contains(got.Error, "logins may be lost") {
		t.Fatalf("Stop = %+v, %v", got, err)
	}
}

func TestReadyTimeout(t *testing.T) {
	f := newFixture(t)
	f.rt.Unhealthy = true
	f.m.readyTimeout = 300 * time.Millisecond
	s, _, err := f.m.Start(context.Background(), "google", "")
	if !errors.Is(err, ErrStartFailed) || s.State != Failed || !strings.Contains(s.Error, "wasn't ready") {
		t.Fatalf("Start = %+v, %v", s, err)
	}
	if f.rt.Stops != 1 || f.rt.Count() != 0 {
		t.Fatalf("stops %d, containers left %d; want the container stopped and removed", f.rt.Stops, f.rt.Count())
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
	f.rt.Exit(gone.ContainerID, 0)
	if err := f.rt.Remove(ctx, gone.ContainerID); err != nil {
		t.Fatal(err)
	}
	f.rt.Mu.Lock()
	f.rt.Containers["stray"] = &sessionstest.FakeContainer{ID: "stray", State: "running", Exited: make(chan struct{})}
	f.rt.Containers["stray"].Spec.Name = "browserpier-stray"
	f.rt.Mu.Unlock()

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
		_, ok := f.rt.Container("stray")
		return !ok
	})
	if got, err := m.Stop(ctx, running.ID); err != nil || got.State != Stopped {
		t.Fatalf("Stop adopted session = %+v, %v", got, err)
	}
}

// A browser an older BrowserPier started (without the egress filter, say)
// is stopped, not picked up, and its login kept by the usual stop.
func TestReconcileStopsBrowsersFromAnOlderVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.Profiles().Create(ctx, profiles.New("github")); err != nil {
		t.Fatal(err)
	}
	old := f.start(f.m)
	current, _, err := f.m.Start(ctx, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	f.m.Close()
	f.rt.Mu.Lock()
	f.rt.Containers[old.ContainerID].Version = "" // from before the label
	stops := f.rt.Stops
	f.rt.Mu.Unlock()

	m := f.manager()
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := m.Live("google"); ok {
		t.Fatalf("the old browser was picked up: %+v", s)
	}
	if got := f.stored(old.ID); got.State != Stopped {
		t.Fatalf("old session: %+v", got)
	}
	f.rt.Mu.Lock()
	stopped := f.rt.Stops - stops
	f.rt.Mu.Unlock()
	if stopped != 1 {
		t.Fatalf("%d containers stopped, want the old one", stopped)
	}
	if s, ok := m.Live("github"); !ok || s.ID != current.ID {
		t.Fatalf("the current browser wasn't picked up: %+v, %t", s, ok)
	}
	// Starting it again gets a browser from this version.
	s, attached, err := m.Start(ctx, "google", "")
	if err != nil || attached || s.ID == old.ID {
		t.Fatalf("restart: %+v, %t, %v", s, attached, err)
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

func TestMarkerWriteDoesNotFollowSymlinks(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	// A compromised browser can leave a symlink where the marker goes.
	target := filepath.Join(f.dir, "outside")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.dir, "profiles", "google", "chrome", mountMarker)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	next := f.start(f.m)
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the marker write went through the symlink: target now %q", b)
	}
	if st, err := os.Lstat(marker); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("marker is not a regular file: %v, %v", st, err)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != next.ID {
		t.Fatalf("marker = %q", b)
	}
}

func TestClosedManagerRefuses(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.m.Close()
	if _, err := f.m.Stop(context.Background(), s.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("Stop after Close = %v", err)
	}
	if _, _, err := f.m.Start(context.Background(), "google", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v", err)
	}
	// Closing leaves the browser running for the next orchestrator.
	if c, _ := f.rt.Container(s.ContainerID); c.State != "running" {
		t.Fatalf("container %s after Close", c.State)
	}
}

func TestStartNeverRemovesSomeoneElsesContainer(t *testing.T) {
	f := newFixture(t)
	f.rt.Mu.Lock()
	f.rt.Containers["theirs"] = &sessionstest.FakeContainer{ID: "theirs", State: "exited", Foreign: true, Exited: make(chan struct{})}
	f.rt.Containers["theirs"].Spec.Name = "browserpier-google"
	f.rt.Mu.Unlock()

	s, _, err := f.m.Start(context.Background(), "google", "")
	if !errors.Is(err, ErrStartFailed) || !strings.Contains(s.Error, "already named browserpier-google") {
		t.Fatalf("Start = %+v, %v; want a name-collision failure", s, err)
	}
	if _, ok := f.rt.Container("theirs"); !ok {
		t.Fatal("an unrelated container was deleted")
	}

	// A stopped one of our own for this profile is cleared out of the way.
	f.rt.Mu.Lock()
	delete(f.rt.Containers, "theirs")
	f.rt.Containers["ours"] = &sessionstest.FakeContainer{ID: "ours", State: "exited", Exited: make(chan struct{})}
	f.rt.Containers["ours"].Spec = docker.BrowserSpec{Name: "browserpier-google", Profile: "google"}
	f.rt.Mu.Unlock()
	if s := f.start(f.m); s.State != Running {
		t.Fatalf("Start with a stale container of ours: %+v", s)
	}
	if _, ok := f.rt.Container("ours"); ok {
		t.Fatal("our stale container wasn't removed")
	}
}

func TestFailedSaveFailsTheStartAndHeals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.db.ExecContext(ctx, `CREATE TRIGGER no_updates BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`); err != nil {
		t.Fatal(err)
	}
	s, _, err := f.m.Start(ctx, "google", "")
	if !errors.Is(err, ErrStartFailed) || !strings.Contains(s.Error, "saving the session") {
		t.Fatalf("Start with a failing database = %+v, %v", s, err)
	}
	if f.rt.Count() != 0 {
		t.Fatal("a container was left behind with no record of it")
	}
	if _, err := f.m.db.ExecContext(ctx, `DROP TRIGGER no_updates`); err != nil {
		t.Fatal(err)
	}
	// The row still says starting; the next start must clear it, not trip
	// over the one-live-session index.
	if got := f.stored(s.ID); got.State != Starting {
		t.Fatalf("stored state %s; the test assumes the final save failed", got.State)
	}
	if next := f.start(f.m); next.State != Running {
		t.Fatalf("retry: %+v", next)
	}
	if got := f.stored(s.ID); got.State != Failed {
		t.Fatalf("stale row %s, want failed", got.State)
	}
}

func TestCloseReleasesWaitingCallers(t *testing.T) {
	f := newFixture(t)
	f.rt.Unhealthy = true
	f.m.readyTimeout = time.Hour
	errc := make(chan error, 1)
	go func() {
		_, _, err := f.m.Start(context.Background(), "google", "")
		errc <- err
	}()
	eventually(t, "the container to start", func() bool { return f.rt.Count() == 1 })
	f.m.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Start = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start still blocked after Close")
	}
}

func TestStopErrorLeavesTheSessionRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	f.rt.StopErr = errors.New("daemon hiccup")
	if _, err := f.m.Stop(ctx, s.ID); err == nil || !strings.Contains(err.Error(), "daemon hiccup") {
		t.Fatalf("Stop = %v, want the Docker error", err)
	}
	if got, _ := f.m.Live("google"); got.State != Running {
		t.Fatalf("after a failed stop the session is %s, want running", got.State)
	}
	if got := f.stored(s.ID); got.State != Running {
		t.Fatalf("stored %s, want running", got.State)
	}
	f.rt.StopErr = nil
	if got, err := f.m.Stop(ctx, s.ID); err != nil || got.State != Stopped {
		t.Fatalf("retry Stop = %+v, %v", got, err)
	}
}

func TestConcurrentStopsShareOneAttempt(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := f.m.Stop(context.Background(), s.ID); err != nil || got.State != Stopped {
				t.Errorf("Stop = %+v, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if f.rt.Stops != 1 {
		t.Fatalf("%d Docker stops for one session", f.rt.Stops)
	}
}

func TestDeleteProfileKeepsTheRecordIfTheFolderCantMove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	profilesDir := filepath.Join(f.dir, "profiles")
	if err := os.Chmod(profilesDir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := f.m.DeleteProfile(ctx, "google")
	_ = os.Chmod(profilesDir, 0o700)
	if err == nil {
		t.Fatal("DeleteProfile succeeded with an unwritable profiles folder")
	}
	if _, err := f.m.Profiles().Get(ctx, "google"); err != nil {
		t.Fatalf("the profile record is gone after a failed delete: %v", err)
	}
	if err := f.m.DeleteProfile(ctx, "google"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestReconcileRemovesTombstones(t *testing.T) {
	f := newFixture(t)
	tomb := filepath.Join(f.dir, "profiles", tombstonePrefix+"old-abc")
	if err := os.MkdirAll(filepath.Join(tomb, "chrome"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tomb); !os.IsNotExist(err) {
		t.Fatalf("tombstone still there: %v", err)
	}
}

func TestAttachShowsInUseByAndEndsWithTheSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	c1, r1, err := f.m.Attach(ctx, s.ID, "t1", "claude")
	if err != nil {
		t.Fatal(err)
	}
	_, r2, _ := f.m.Attach(ctx, s.ID, "t1", "claude")
	c3, r3, _ := f.m.Attach(ctx, s.ID, "t2", "browser-use")
	if got, _ := f.m.Live("google"); strings.Join(got.InUseBy, ",") != "browser-use,claude" {
		t.Fatalf("InUseBy = %v", got.InUseBy)
	}
	if got := f.m.ConnectedTokens(); len(got) != 2 {
		t.Fatalf("ConnectedTokens = %v", got)
	}
	r1()
	r1() // releasing twice is harmless
	if c1.Err() == nil {
		t.Fatal("a released connection's context is still open")
	}
	r3()
	if c3.Err() == nil {
		t.Fatal("release didn't cancel")
	}
	if got, _ := f.m.Get(ctx, s.ID); strings.Join(got.InUseBy, ",") != "claude" {
		t.Fatalf("InUseBy after release = %v", got.InUseBy)
	}
	r2()
	if _, _, err := f.m.Attach(ctx, "nope", "t1", "claude"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Attach unknown = %v", err)
	}

	// Stopping the session closes what's still attached.
	c4, r4, _ := f.m.Attach(ctx, s.ID, "t1", "claude")
	defer r4()
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if c4.Err() == nil {
		t.Fatal("a connection outlived its session")
	}
}

func TestHandoffClosesAgentConnections(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	c, release, _ := f.m.Attach(ctx, s.ID, "t1", "claude")
	defer release()
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if c.Err() == nil {
		t.Fatal("an agent stayed connected while a person has the browser")
	}
	if _, _, err := f.m.Attach(ctx, s.ID, "t1", "claude"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Attach during a handoff = %v", err)
	}
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	_, r, err := f.m.Attach(ctx, s.ID, "t1", "claude")
	if err != nil {
		t.Fatalf("Attach after hand-back = %v", err)
	}
	r()
}

func TestDisconnectToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	revoked, r1, _ := f.m.Attach(ctx, s.ID, "t1", "claude")
	defer r1()
	kept, r2, _ := f.m.Attach(ctx, s.ID, "t2", "other")
	defer r2()
	f.m.DisconnectToken("t1")
	if revoked.Err() == nil || kept.Err() != nil {
		t.Fatalf("after revoking t1: t1 %v, t2 %v", revoked.Err(), kept.Err())
	}
}

func TestProxyAndLocationHandover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, _ := f.m.Profiles().Get(ctx, "google")
	p.Proxy = "socks5://user:s3cret@proxy.example:1080"
	p.Locale = "de-DE"
	p.LocalNetwork = true
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Profiles().SetLocation(ctx, "google", profiles.Location{Country: "US", TimeZone: "America/Chicago"}); err != nil {
		t.Fatal(err)
	}
	f.m.SetGeoURL("https://geo.example/json")
	f.m.SetHostIPs([]string{"203.0.113.4", "100.72.0.9"})

	// The browser reads and deletes the proxy file itself; here the fake
	// checks it while "starting", and reports a fresh lookup.
	proxyPath := filepath.Join(f.dir, "profiles", "google", "run", proxyFile)
	var seen string
	var mode os.FileMode
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if seen == "" {
			b, _ := os.ReadFile(proxyPath)
			st, _ := os.Stat(proxyPath)
			seen = string(b)
			if st != nil {
				mode = st.Mode().Perm()
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /geo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"203.0.113.9","country":"DE","time_zone":"Europe/Berlin","source":"lookup"}`))
	})
	f.rt.Control = mux

	s := f.start(f.m)
	if seen != `{"url":"socks5://user:s3cret@proxy.example:1080"}` || mode != 0o600 {
		t.Fatalf("proxy file %q, mode %v", seen, mode)
	}
	c, _ := f.rt.Container(s.ContainerID)
	env := strings.Join(c.Spec.Env, "\n")
	for _, want := range []string{"BROWSERPIER_GEOIP_URL=https://geo.example/json", "BROWSERPIER_GEO_COUNTRY=US", "BROWSERPIER_GEO_TZ=America/Chicago", "BROWSERPIER_LOCALE_OVERRIDE=de-DE", "BROWSERPIER_TZ_OVERRIDE=", "BROWSERPIER_LOCAL_NETWORK=true", "BROWSERPIER_HOST_IPS=203.0.113.4,100.72.0.9"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
	if strings.Contains(env, "s3cret") {
		t.Fatal("proxy credentials in the container's environment")
	}
	got, _ := f.m.Profiles().Get(ctx, "google")
	if got.Location.Country != "DE" || got.Location.TimeZone != "Europe/Berlin" || got.Location.IP != "203.0.113.9" || got.Location.CheckedAt.IsZero() {
		t.Fatalf("location not recorded: %+v", got.Location)
	}

	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(proxyPath); !os.IsNotExist(err) {
		t.Fatal("the proxy credentials were left in the control folder")
	}
}

// StopAll ends every browser as a clean stop, not a failure, and no new
// one starts afterwards.
func TestStopAll(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Profiles().Create(context.Background(), profiles.New("mail")); err != nil {
		t.Fatal(err)
	}
	a := f.start(f.m)
	b, _, err := f.m.Start(context.Background(), "mail", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.StopAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := f.stored(id); got.State != Stopped {
			t.Fatalf("session %s: %s (%s)", id, got.State, got.Error)
		}
	}
	if f.rt.Count() != 0 {
		t.Fatal("a browser container is left")
	}
	if _, _, err := f.m.Start(context.Background(), "google", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after StopAll = %v", err)
	}
}
