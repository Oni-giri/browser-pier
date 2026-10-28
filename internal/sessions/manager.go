package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// Runtime runs browser containers; *docker.Client is the real one.
type Runtime interface {
	Create(ctx context.Context, spec docker.BrowserSpec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Wait(ctx context.Context, id string) (int, error)
	LastLogLine(ctx context.Context, id string) (string, error)
	Remove(ctx context.Context, id string) error
	List(ctx context.Context) ([]docker.Container, error)
}

// Layout is where profile folders live:
//
//	<data dir>/profiles/<name>/chrome  Chromium's user data dir, mounted at /profile
//	<data dir>/profiles/<name>/run     the control socket, mounted at /run/browserpier
type Layout struct {
	DataDir     string // absolute, as the orchestrator sees it
	HostDataDir string // the same folder as the Docker daemon sees it
	// UID and GID run browser containers and own profile folders.
	UID, GID int
}

// mountMarker is the file each start writes into both folders; the browser
// image's entrypoint refuses to start unless it finds it (see
// images/chromium/entrypoint).
const mountMarker = ".browserpier-mount"

// maxSocketPath is the longest path a Unix socket can be dialled at.
const maxSocketPath = 107

func (l Layout) dir(profile string, sub ...string) string {
	return filepath.Join(append([]string{l.DataDir, "profiles", profile}, sub...)...)
}

func (l Layout) hostDir(profile, sub string) string {
	return filepath.Join(l.HostDataDir, "profiles", profile, sub)
}

// ControlSocket is the path of a profile's control socket, as the orchestrator sees it.
func (l Layout) ControlSocket(profile string) string { return l.dir(profile, "run", "control.sock") }

func (l Layout) check() error {
	if !filepath.IsAbs(l.DataDir) || !filepath.IsAbs(l.HostDataDir) {
		return errors.New("the data dir and its host path must be absolute")
	}
	if p := l.ControlSocket(strings.Repeat("x", 32)); len(p) > maxSocketPath {
		return fmt.Errorf("the data dir %s is too long: control sockets under it would exceed %d bytes; use a shorter path", l.DataDir, maxSocketPath)
	}
	return nil
}

const (
	// ReadyTimeout is how long a browser gets from container start to answering on its control socket.
	ReadyTimeout = 60 * time.Second
	// createTimeout covers pulling the image on first use.
	createTimeout = 10 * time.Minute
	stopTimeout   = docker.StopTimeout + 15*time.Second
)

// Manager starts, stops and tracks sessions. It holds one live session per
// profile; the database records every session.
type Manager struct {
	db           *sql.DB
	profiles     *profiles.Store
	rt           Runtime
	layout       Layout
	log          *slog.Logger
	readyTimeout time.Duration
	now          func() time.Time

	ctx    context.Context // ends with Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	live map[string]*live // by profile name
}

// live is a session with a container, from Starting until it is final.
type live struct {
	sess      Session // guarded by Manager.mu
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{} // closed once the session is final and its container gone
	// failure, when set, is why the orchestrator gave up on the session;
	// it's applied once the container has exited.
	failure string
}

func newLive(s Session) *live {
	return &live{sess: s, ready: make(chan struct{}), done: make(chan struct{})}
}

// markReady unblocks everyone waiting for the start to resolve, either way.
func (l *live) markReady() { l.readyOnce.Do(func() { close(l.ready) }) }

func NewManager(db *sql.DB, rt Runtime, layout Layout, log *slog.Logger) (*Manager, error) {
	if err := layout.check(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		db:           db,
		profiles:     profiles.NewStore(db),
		rt:           rt,
		layout:       layout,
		log:          log,
		readyTimeout: ReadyTimeout,
		now:          func() time.Time { return time.Now().Truncate(time.Millisecond) },
		ctx:          ctx,
		cancel:       cancel,
		live:         map[string]*live{},
	}, nil
}

// Close stops tracking sessions. Browsers keep running; Reconcile picks them
// up on the next start, so restarting the orchestrator doesn't log anyone out.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) Profiles() *profiles.Store { return m.profiles }

func (m *Manager) Layout() Layout { return m.layout }

func containerName(profile string) string { return "browserpier-" + profile }

// Start starts the profile's browser, or attaches to it if it is already
// starting or running (attached is then true). It returns once the browser
// is ready or has failed. mode "" means the profile's default.
func (m *Manager) Start(ctx context.Context, profile string, mode profiles.Mode) (s Session, attached bool, err error) {
	for {
		m.mu.Lock()
		l, ok := m.live[profile]
		if !ok {
			break // keep the lock
		}
		state := l.sess.State
		m.mu.Unlock()
		if state == Stopping {
			if err := wait(ctx, l.done); err != nil {
				return Session{}, false, err
			}
			continue
		}
		return m.awaitStart(ctx, l, true)
	}

	l, p, err := m.begin(ctx, profile, mode)
	m.mu.Unlock()
	if err != nil {
		return Session{}, false, err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.launch(l, p)
	}()
	return m.awaitStart(ctx, l, false)
}

// begin records a new Starting session. m.mu must be held.
func (m *Manager) begin(ctx context.Context, profile string, mode profiles.Mode) (*live, profiles.Profile, error) {
	p, err := m.profiles.Get(ctx, profile)
	if err != nil {
		return nil, p, err
	}
	if mode == "" {
		mode = p.DefaultMode
	}
	if mode != profiles.ModeAPI && mode != profiles.ModeVisual {
		return nil, p, fmt.Errorf("mode %q: want %q or %q", mode, profiles.ModeAPI, profiles.ModeVisual)
	}
	now := m.now()
	s := Session{ID: newID(), Profile: profile, Mode: mode, State: Starting, CreatedAt: now, UpdatedAt: now}
	if err := insertSession(ctx, m.db, s); err != nil {
		return nil, p, err
	}
	l := newLive(s)
	m.live[profile] = l
	return l, p, nil
}

func (m *Manager) awaitStart(ctx context.Context, l *live, attached bool) (Session, bool, error) {
	if err := wait(ctx, l.ready); err != nil {
		return Session{}, attached, err
	}
	s := m.snapshot(l)
	if s.State == Failed {
		return s, attached, fmt.Errorf("%w: %s", ErrStartFailed, s.Error)
	}
	return s, attached, nil
}

// launch creates and starts the container, then waits for it to be ready.
func (m *Manager) launch(l *live, p profiles.Profile) {
	id := m.snapshot(l).ID
	ctx, cancel := context.WithTimeout(m.ctx, createTimeout)
	defer cancel()

	if err := m.prepare(p.Name, id); err != nil {
		m.abort(l, "", fmt.Sprintf("preparing the profile folder: %v", err))
		return
	}
	// A container left behind by a crash would hold the name.
	if err := m.rt.Remove(ctx, containerName(p.Name)); err != nil {
		m.abort(l, "", fmt.Sprintf("an old container for this profile is in the way: %v", err))
		return
	}
	cid, err := m.rt.Create(ctx, docker.BrowserSpec{
		Name:       containerName(p.Name),
		Profile:    p.Name,
		Session:    id,
		ProfileDir: m.layout.hostDir(p.Name, "chrome"),
		ControlDir: m.layout.hostDir(p.Name, "run"),
		User:       docker.User(m.layout.UID, m.layout.GID),
		Screen:     p.Screen(),
		MountCheck: id,
	})
	if err != nil {
		m.abort(l, "", fmt.Sprintf("creating the browser container: %v", err))
		return
	}
	m.update(l, func(s *Session) { s.ContainerID = cid })
	if err := m.rt.Start(ctx, cid); err != nil {
		m.abort(l, cid, fmt.Sprintf("starting the browser container: %v", err))
		return
	}
	m.watch(l, cid)
	m.awaitReady(l, cid)
}

// prepare creates the profile's folders and writes this start's marker.
func (m *Manager) prepare(profile, sessionID string) error {
	for _, sub := range []string{"chrome", "run"} {
		dir := m.layout.dir(profile, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		marker := filepath.Join(dir, mountMarker)
		if err := os.WriteFile(marker, []byte(sessionID+"\n"), 0o600); err != nil {
			return err
		}
		if os.Getuid() == 0 {
			// Chromium won't run as root with its sandbox on, so a root
			// orchestrator hands the folders to the browser's user.
			for _, p := range []string{dir, marker} {
				if err := os.Lchown(p, m.layout.UID, m.layout.GID); err != nil {
					return err
				}
			}
		}
	}
	if err := os.Remove(m.layout.ControlSocket(profile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// awaitReady polls the control socket until the browser answers, the
// container exits, or ReadyTimeout passes.
func (m *Manager) awaitReady(l *live, cid string) {
	sock := m.layout.ControlSocket(m.snapshot(l).Profile)
	deadline := time.NewTimer(m.readyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if healthy(m.ctx, sock) {
			m.update(l, func(s *Session) {
				if s.State == Starting {
					s.State = Running
				}
			})
			l.markReady()
			return
		}
		select {
		case <-l.done:
			return
		case <-m.ctx.Done():
			return
		case <-deadline.C:
			m.mu.Lock()
			l.failure = fmt.Sprintf("the browser wasn't ready after %s", m.readyTimeout)
			m.mu.Unlock()
			m.stopContainer(cid)
			return
		case <-tick.C:
		}
	}
}

func healthy(ctx context.Context, sock string) bool {
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// watch finishes the session when its container exits.
func (m *Manager) watch(l *live, cid string) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		code, err := m.rt.Wait(m.ctx, cid)
		if m.ctx.Err() != nil {
			return // closing: leave the browser running for the next Reconcile
		}
		m.finish(l, cid, code, err)
	}()
}

// finish records how the session ended and removes its container.
func (m *Manager) finish(l *live, cid string, code int, waitErr error) {
	lastLog := m.cleanup(cid)
	m.end(l, lastLog, func(s *Session) {
		switch {
		case l.failure != "":
			s.State, s.Error = Failed, l.failure
		case waitErr != nil:
			s.State, s.Error = Failed, fmt.Sprintf("lost track of the browser container: %v", waitErr)
		case s.State == Stopping && code == 0:
			s.State = Stopped
		case s.State == Stopping:
			s.State, s.Error = Failed, fmt.Sprintf("the browser didn't close cleanly (exit status %d), so recent logins may be lost", code)
		default:
			s.State, s.Error = Failed, fmt.Sprintf("the browser exited unexpectedly (exit status %d)", code)
		}
	})
}

// abort fails a session whose container never got going.
func (m *Manager) abort(l *live, cid, reason string) {
	lastLog := ""
	if cid != "" {
		lastLog = m.cleanup(cid)
	}
	m.end(l, lastLog, func(s *Session) { s.State, s.Error = Failed, reason })
}

// cleanup reads a stopped container's last log line and removes it.
func (m *Manager) cleanup(cid string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lastLog, err := m.rt.LastLogLine(ctx, cid)
	if err != nil {
		m.log.Warn("reading the browser container's logs", "container", cid, "err", err)
	}
	if err := m.rt.Remove(ctx, cid); err != nil {
		m.log.Warn("removing the browser container", "container", cid, "err", err)
	}
	return lastLog
}

// end makes the session final and releases its profile.
func (m *Manager) end(l *live, lastLog string, final func(*Session)) {
	m.mu.Lock()
	final(&l.sess)
	l.sess.LastLog = lastLog
	l.sess.UpdatedAt = m.now()
	if m.live[l.sess.Profile] == l {
		delete(m.live, l.sess.Profile)
	}
	s := l.sess
	m.persistLocked(s)
	m.mu.Unlock()
	if s.State == Failed {
		m.log.Warn("session failed", "session", s.ID, "profile", s.Profile, "err", s.Error, "last_log", s.LastLog)
	} else {
		m.log.Info("session stopped", "session", s.ID, "profile", s.Profile)
	}
	l.markReady()
	close(l.done)
}

func (m *Manager) update(l *live, change func(*Session)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	change(&l.sess)
	l.sess.UpdatedAt = m.now()
	m.persistLocked(l.sess)
}

func (m *Manager) persistLocked(s Session) {
	if err := updateSession(context.Background(), m.db, s); err != nil {
		m.log.Error("saving session state", "session", s.ID, "state", s.State, "err", err)
	}
}

func (m *Manager) snapshot(l *live) Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return l.sess
}

// stopContainer asks Docker to stop a container without waiting for it; the
// watcher records the outcome.
func (m *Manager) stopContainer(cid string) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, stopTimeout)
		defer cancel()
		if err := m.rt.Stop(ctx, cid); err != nil && m.ctx.Err() == nil {
			m.log.Warn("stopping the browser container", "container", cid, "err", err)
		}
	}()
}

// Stop closes the session's browser gracefully and waits until its
// container is gone. Stopping a session that has already ended returns it.
func (m *Manager) Stop(ctx context.Context, id string) (Session, error) {
	l := m.find(id)
	if l == nil {
		return getSession(ctx, m.db, id)
	}
	if err := wait(ctx, l.ready); err != nil {
		return Session{}, err
	}
	m.mu.Lock()
	first := l.sess.State == Running || l.sess.State == NeedsHelp
	if first {
		l.sess.State = Stopping
		l.sess.UpdatedAt = m.now()
		m.persistLocked(l.sess)
	}
	cid := l.sess.ContainerID
	m.mu.Unlock()
	if first {
		m.stopContainer(cid)
	}
	if err := wait(ctx, l.done); err != nil {
		return m.snapshot(l), err
	}
	return m.snapshot(l), nil
}

// SetNeedsHelp moves a running session to Needs help and back.
func (m *Manager) SetNeedsHelp(id string, needsHelp bool) (Session, error) {
	from, to := Running, NeedsHelp
	if !needsHelp {
		from, to = NeedsHelp, Running
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil {
		return Session{}, ErrNotRunning
	}
	switch l.sess.State {
	case to:
	case from:
		l.sess.State = to
		l.sess.UpdatedAt = m.now()
		m.persistLocked(l.sess)
	default:
		return l.sess, ErrNotRunning
	}
	return l.sess, nil
}

// Get returns a session, live or past.
func (m *Manager) Get(ctx context.Context, id string) (Session, error) {
	if l := m.find(id); l != nil {
		return m.snapshot(l), nil
	}
	return getSession(ctx, m.db, id)
}

// Live returns the profile's live session, if it has one.
func (m *Manager) Live(profile string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.live[profile]; ok {
		return l.sess, true
	}
	return Session{}, false
}

// List returns the live sessions, by profile.
func (m *Manager) List() []Session {
	m.mu.Lock()
	out := make([]Session, 0, len(m.live))
	for _, l := range m.live {
		out = append(out, l.sess)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// DeleteProfile deletes a stopped profile, its session history and its
// folder: its logins and files are gone for good.
func (m *Manager) DeleteProfile(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.live[name]; ok {
		return ErrProfileBusy
	}
	if err := m.profiles.Delete(ctx, name); err != nil {
		return err
	}
	// name was validated when the profile was created, so this stays inside profiles/.
	return os.RemoveAll(m.layout.dir(name))
}

func (m *Manager) find(id string) *live {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.findLocked(id)
}

func (m *Manager) findLocked(id string) *live {
	for _, l := range m.live {
		if l.sess.ID == id {
			return l
		}
	}
	return nil
}

func wait(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
