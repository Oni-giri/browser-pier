package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	RemoveStale(ctx context.Context, name, profile string) error
	List(ctx context.Context) ([]docker.Container, error)
	MemoryUsage(ctx context.Context, id string) (int64, error)
}

// Layout is where profile folders live:
//
//	<data dir>/profiles/<name>/chrome  Chromium's user data dir, mounted at /profile
//	<data dir>/profiles/<name>/run     the control socket, mounted at /run/browserpier
//	<data dir>/profiles/<name>/files   downloads/ and uploads/, mounted at /files
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

// FilesDir is a profile's downloads and uploads folder, as the orchestrator sees it.
func (l Layout) FilesDir(profile string) string { return l.dir(profile, "files") }

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
	geoURL       string // where browsers look up their exit location; "" for nowhere
	// mem is the memory cap, set by SetMemoryLimit; nil for none.
	mem atomic.Pointer[memLimits]

	ctx    context.Context // ends with Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	goMu   sync.Mutex
	closed bool // set by Close; guarded by goMu

	mu   sync.Mutex
	live map[string]*live // by profile name

	// admit is held from checking there is room for a new browser to
	// recording its session, so two starts can't both squeeze in.
	admit sync.Mutex
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
	// stop is the stop in progress, if any; everyone stopping waits on it.
	stop *stopAttempt
	// conns are the agents' open connections to the browser.
	conns map[*conn]struct{}
	// lastActivity is when an agent or person last used the browser.
	lastActivity time.Time
	// lastAgentInput is when an agent last clicked or typed.
	lastAgentInput time.Time
	// human is set while a person has control from the live view.
	human bool
	// viewers counts open live views.
	viewers int
	// activity is what agents did, oldest first; it goes with the session.
	activity []Activity
}

// conn is one agent connection to a browser; cancelling it closes it.
type conn struct {
	tokenID, agent string
	cancel         context.CancelFunc
}

type stopAttempt struct {
	done chan struct{}
	err  error // set before done closes
}

func newLive(s Session) *live {
	return &live{sess: s, ready: make(chan struct{}), done: make(chan struct{}), lastActivity: time.Now()}
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
	m.goMu.Lock()
	m.closed = true
	m.goMu.Unlock()
	m.cancel()
	m.wg.Wait()
}

// spawn runs f in a goroutine that Close waits for. Once Close has begun
// it runs nothing and returns false.
func (m *Manager) spawn(f func()) bool {
	m.goMu.Lock()
	defer m.goMu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		f()
	}()
	return true
}

// wait waits for ch, the caller's ctx, or the manager closing.
func (m *Manager) wait(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return ErrClosed
	}
}

func (m *Manager) Profiles() *profiles.Store { return m.profiles }

// SetGeoURL sets the echoip-style endpoint browsers look up their exit
// location with ("" turns the lookup off). Call it before Start.
func (m *Manager) SetGeoURL(u string) { m.geoURL = u }

func (m *Manager) Layout() Layout { return m.layout }

func containerName(profile string) string { return "browserpier-" + profile }

// Start starts the profile's browser, or attaches to it if it is already
// starting or running (attached is then true). It returns once the browser
// is ready or has failed. mode "" means the profile's default.
func (m *Manager) Start(ctx context.Context, profile string, mode profiles.Mode) (s Session, attached bool, err error) {
	if m.ctx.Err() != nil {
		return Session{}, false, ErrClosed
	}
	for {
		m.mu.Lock()
		l, ok := m.live[profile]
		m.mu.Unlock()
		if ok {
			if m.snapshot(l).State == Stopping {
				if err := m.wait(ctx, l.done); err != nil {
					return Session{}, false, err
				}
				continue
			}
			return m.awaitStart(ctx, l, true)
		}
		l, p, again, err := m.admitAndBegin(ctx, profile, mode)
		if err != nil {
			return Session{}, false, err
		}
		if again {
			continue // another start got there first: attach to it
		}
		if !m.spawn(func() { m.launch(l, p) }) {
			m.end(l, "", func(s *Session) { s.State, s.Error = Failed, "BrowserPier was shutting down" })
			return Session{}, false, ErrClosed
		}
		return m.awaitStart(ctx, l, false)
	}
}

// admitAndBegin makes room for a new browser and records its session, one
// start at a time: the admission lock is held from the memory check until
// the session counts towards memory. again is true if a session for the
// profile appeared meanwhile, to attach to instead.
func (m *Manager) admitAndBegin(ctx context.Context, profile string, mode profiles.Mode) (l *live, p profiles.Profile, again bool, err error) {
	// Check the request before stopping anyone's browser for it.
	if _, err := m.profiles.Get(ctx, profile); err != nil {
		return nil, p, false, err
	}
	if mode != "" && mode != profiles.ModeAPI && mode != profiles.ModeVisual {
		return nil, p, false, fmt.Errorf("mode %q: want %q or %q", mode, profiles.ModeAPI, profiles.ModeVisual)
	}
	m.admit.Lock()
	defer m.admit.Unlock()
	if m.hasLive(profile) {
		return nil, p, true, nil
	}
	if err := m.makeRoom(ctx); err != nil {
		return nil, p, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.live[profile]; ok {
		return nil, p, true, nil
	}
	l, p, err = m.begin(ctx, profile, mode)
	return l, p, false, err
}

func (m *Manager) hasLive(profile string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[profile]
	return ok
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
	// With no live session in memory, any live row for this profile is
	// stale, left by a save that failed; clear it so the profile isn't stuck.
	if _, err := m.db.ExecContext(ctx, `UPDATE sessions SET state = 'failed', error = 'its final state was never saved', updated_at = ?
		WHERE profile = ? AND state IN ('starting', 'running', 'needs_help', 'stopping')`, now.UnixMilli(), profile); err != nil {
		return nil, p, err
	}
	s := Session{ID: newID(), Profile: profile, Mode: mode, State: Starting, CreatedAt: now, UpdatedAt: now}
	if err := insertSession(ctx, m.db, s); err != nil {
		return nil, p, err
	}
	l := newLive(s)
	l.lastActivity = now
	m.live[profile] = l
	return l, p, nil
}

func (m *Manager) awaitStart(ctx context.Context, l *live, attached bool) (Session, bool, error) {
	if err := m.wait(ctx, l.ready); err != nil {
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
	if err := m.writeProxyFile(p); err != nil {
		m.abort(l, "", fmt.Sprintf("handing over the proxy: %v", err))
		return
	}
	// A container left behind by a crash would hold the name.
	if err := m.rt.RemoveStale(ctx, containerName(p.Name), p.Name); err != nil {
		m.abort(l, "", err.Error())
		return
	}
	cid, err := m.rt.Create(ctx, docker.BrowserSpec{
		Name:       containerName(p.Name),
		Profile:    p.Name,
		Session:    id,
		ProfileDir: m.layout.hostDir(p.Name, "chrome"),
		ControlDir: m.layout.hostDir(p.Name, "run"),
		FilesDir:   m.layout.hostDir(p.Name, "files"),
		User:       docker.User(m.layout.UID, m.layout.GID),
		Screen:     p.Screen(),
		MountCheck: id,
		Env: []string{
			"BROWSERPIER_GEOIP_URL=" + m.geoURL,
			"BROWSERPIER_GEO_COUNTRY=" + p.Location.Country,
			"BROWSERPIER_GEO_TZ=" + p.Location.TimeZone,
			"BROWSERPIER_TZ_OVERRIDE=" + p.TimeZone,
			"BROWSERPIER_LOCALE_OVERRIDE=" + p.Locale,
		},
	})
	if err != nil {
		m.abort(l, "", fmt.Sprintf("creating the browser container: %v", err))
		return
	}
	if err := m.update(l, func(s *Session) { s.ContainerID = cid }); err != nil {
		// Unrecorded, the container would be stopped as a stray after a restart.
		m.abort(l, cid, fmt.Sprintf("saving the session: %v", err))
		return
	}
	if err := m.rt.Start(ctx, cid); err != nil {
		m.abort(l, cid, fmt.Sprintf("starting the browser container: %v", err))
		return
	}
	m.watch(l, cid)
	m.awaitReady(l, cid)
}

// prepare creates the profile's folders and writes this start's marker.
// The browser can write anything inside both folders, symlinks included,
// so everything in them is touched through os.Root, which won't follow a
// link out of the folder.
func (m *Manager) prepare(profile, sessionID string) error {
	if err := m.prepareFiles(profile); err != nil {
		return err
	}
	for _, sub := range []string{"chrome", "run"} {
		dir := m.layout.dir(profile, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := m.writeMarker(dir, sessionID); err != nil {
			return err
		}
	}
	return nil
}

// prepareFiles creates files/downloads and files/uploads. The browser
// can replace either with a symlink, so they are checked through os.Root.
func (m *Manager) prepareFiles(profile string) error {
	dir := m.layout.FilesDir(profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, sub := range []string{".", "downloads", "uploads"} {
		if err := root.Mkdir(sub, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if os.Getuid() == 0 {
			if err := root.Lchown(sub, m.layout.UID, m.layout.GID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) writeMarker(dir, sessionID string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(mountMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.Remove("control.sock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(mountMarker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(sessionID + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		// Chromium won't run as root with its sandbox on, so a root
		// orchestrator hands the folders to the browser's user.
		if err := root.Lchown(".", m.layout.UID, m.layout.GID); err != nil {
			return err
		}
		if err := root.Lchown(mountMarker, m.layout.UID, m.layout.GID); err != nil {
			return err
		}
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
			err := m.update(l, func(s *Session) {
				if s.State == Starting {
					s.State = Running
				}
			})
			if err != nil {
				m.mu.Lock()
				l.failure = fmt.Sprintf("saving the session: %v", err)
				m.mu.Unlock()
				m.stopContainer(cid)
				return
			}
			m.recordLocation(sock, m.snapshot(l).Profile)
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
	c := controlClient(sock)
	c.Timeout = time.Second
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

// controlClient talks to a browser's control socket, one connection per request.
func controlClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
}

// watch finishes the session when its container exits.
func (m *Manager) watch(l *live, cid string) {
	m.spawn(func() {
		code, err := m.rt.Wait(m.ctx, cid)
		if m.ctx.Err() != nil {
			return // closing: leave the browser running for the next Reconcile
		}
		m.finish(l, cid, code, err)
	})
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

// proxyFile is where the proxy URL waits for the entrypoint, which reads
// and deletes it before Chromium starts (images/chromium/entrypoint).
const proxyFile = "proxy.json"

// writeProxyFile hands the profile's proxy, credentials included, to the
// browser container through its control folder, never its environment.
func (m *Manager) writeProxyFile(p profiles.Profile) error {
	root, err := os.OpenRoot(m.layout.dir(p.Name, "run"))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(proxyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.Proxy == "" {
		return nil
	}
	b, err := json.Marshal(map[string]string{"url": p.Proxy})
	if err != nil {
		return err
	}
	f, err := root.OpenFile(proxyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if os.Getuid() == 0 {
		if err := f.Chown(m.layout.UID, m.layout.GID); err != nil {
			_ = f.Close()
			return err
		}
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeProxyFile deletes the credentials if the browser never read them.
func (m *Manager) removeProxyFile(profile string) {
	root, err := os.OpenRoot(m.layout.dir(profile, "run"))
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.Remove(proxyFile)
}

// recordLocation saves where the browser found itself, so the next start
// can keep the same time zone (see the entrypoint's decideGeo).
func (m *Manager) recordLocation(sock, profile string) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/geo", nil)
	resp, err := controlClient(sock).Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var g struct {
		IP       string `json:"ip"`
		Country  string `json:"country"`
		TimeZone string `json:"time_zone"`
		Source   string `json:"source"`
		Warning  string `json:"warning"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&g) != nil {
		return
	}
	if g.Warning != "" {
		m.log.Warn("browser location", "profile", profile, "warning", g.Warning)
	}
	if g.Source != "lookup" {
		return
	}
	loc := profiles.Location{Country: g.Country, TimeZone: g.TimeZone, IP: g.IP, CheckedAt: m.now()}
	if err := m.profiles.SetLocation(ctx, profile, loc); err != nil {
		m.log.Warn("saving the browser's location", "profile", profile, "err", err)
	}
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

// end makes the session final and releases its profile, and makes sure no
// proxy credentials are left in its control folder. If the final
// state can't be saved after a few tries, the profile is released anyway:
// the next Start for it, or the next Reconcile, clears the stale row.
func (m *Manager) end(l *live, lastLog string, final func(*Session)) {
	m.mu.Lock()
	final(&l.sess)
	l.sess.LastLog = lastLog
	l.sess.UpdatedAt = m.now()
	s := l.sess
	m.mu.Unlock()
	m.removeProxyFile(s.Profile)
	for i, backoff := range []time.Duration{0, 200 * time.Millisecond, time.Second} {
		time.Sleep(backoff)
		err := updateSession(context.Background(), m.db, s)
		if err == nil {
			break
		}
		m.log.Error("saving the final session state", "session", s.ID, "state", s.State, "try", i+1, "err", err)
	}
	m.mu.Lock()
	if m.live[s.Profile] == l {
		delete(m.live, s.Profile)
	}
	l.disconnectLocked()
	m.mu.Unlock()
	if s.State == Failed {
		m.log.Warn("session failed", "session", s.ID, "profile", s.Profile, "err", s.Error, "last_log", s.LastLog)
	} else {
		m.log.Info("session stopped", "session", s.ID, "profile", s.Profile)
	}
	l.markReady()
	close(l.done)
}

// update changes the session and saves it. If the save fails, the
// change is undone and the error returned.
func (m *Manager) update(l *live, change func(*Session)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := l.sess
	change(&l.sess)
	l.sess.UpdatedAt = m.now()
	if err := updateSession(context.Background(), m.db, l.sess); err != nil {
		l.sess = before
		m.log.Error("saving session state", "session", before.ID, "err", err)
		return err
	}
	return nil
}

func (m *Manager) snapshot(l *live) Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return l.snapshotLocked()
}

func (l *live) snapshotLocked() Session {
	s := l.sess
	s.InUseBy = nil
	seen := map[string]bool{}
	for c := range l.conns {
		if !seen[c.agent] {
			seen[c.agent] = true
			s.InUseBy = append(s.InUseBy, c.agent)
		}
	}
	sort.Strings(s.InUseBy)
	s.HumanControl, s.Viewers = l.human, l.viewers
	return s
}

// Attach registers an agent's connection to a running session's browser.
// It returns a context that ends when the connection must close: when a
// person takes over (Needs help), when the session ends, or when the
// agent's token is revoked (DisconnectToken). Call release when the
// connection closes. The agent shows as "In use by" meanwhile.
func (m *Manager) Attach(ctx context.Context, id, tokenID, agent string) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || l.sess.State != Running {
		return nil, nil, ErrNotRunning
	}
	if l.human {
		return nil, nil, ErrHumanDriving
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &conn{tokenID: tokenID, agent: agent, cancel: cancel}
	if l.conns == nil {
		l.conns = map[*conn]struct{}{}
	}
	l.conns[c] = struct{}{}
	l.lastActivity = m.now()
	var once sync.Once
	return cctx, func() {
		once.Do(func() {
			cancel()
			m.mu.Lock()
			delete(l.conns, c)
			l.lastActivity = m.now()
			m.mu.Unlock()
		})
	}, nil
}

// disconnectLocked closes every agent connection to the session.
func (l *live) disconnectLocked() {
	for c := range l.conns {
		c.cancel()
	}
}

// DisconnectToken closes every connection opened with a token, for when
// it is revoked.
func (m *Manager) DisconnectToken(tokenID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.live {
		for c := range l.conns {
			if c.tokenID == tokenID {
				c.cancel()
			}
		}
	}
}

// ConnectedTokens lists the tokens with an open connection to any browser.
func (m *Manager) ConnectedTokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, l := range m.live {
		for c := range l.conns {
			if !seen[c.tokenID] {
				seen[c.tokenID] = true
				out = append(out, c.tokenID)
			}
		}
	}
	return out
}

// stopContainer asks Docker to stop a container without waiting for it; the
// watcher records the outcome.
func (m *Manager) stopContainer(cid string) {
	m.spawn(func() {
		ctx, cancel := context.WithTimeout(m.ctx, stopTimeout)
		defer cancel()
		if err := m.rt.Stop(ctx, cid); err != nil && m.ctx.Err() == nil {
			m.log.Warn("stopping the browser container", "container", cid, "err", err)
		}
	})
}

// Stop closes the session's browser gracefully and waits until its
// container is gone. Stopping a session that has already ended returns it.
// If Docker fails to stop the container, the session goes back to the
// state it was in and every caller gets the error.
func (m *Manager) Stop(ctx context.Context, id string) (Session, error) {
	return m.stopIf(ctx, id, nil)
}

// errStillInUse: stopIf found the session no longer met its condition.
var errStillInUse = errors.New("the browser is in use again")

// stopIf is Stop, but only if cond (checked under the lock, in the same
// step that moves the session to Stopping) holds; otherwise it returns
// errStillInUse. It closes the race between picking an idle browser and
// an agent attaching to it.
func (m *Manager) stopIf(ctx context.Context, id string, cond func(*live) bool) (Session, error) {
	if m.ctx.Err() != nil {
		return Session{}, ErrClosed
	}
	l := m.find(id)
	if l == nil {
		return getSession(ctx, m.db, id)
	}
	if err := m.wait(ctx, l.ready); err != nil {
		return Session{}, err
	}
	m.mu.Lock()
	attempt := l.stop
	if attempt == nil && (l.sess.State == Running || l.sess.State == NeedsHelp) {
		if cond != nil && !cond(l) {
			snap := l.snapshotLocked()
			m.mu.Unlock()
			return snap, errStillInUse
		}
		prev := l.sess.State
		l.sess.State = Stopping
		l.sess.UpdatedAt = m.now()
		if err := updateSession(context.Background(), m.db, l.sess); err != nil {
			// Stopping anyway beats refusing; the final state is saved with retries.
			m.log.Error("saving session state", "session", id, "err", err)
		}
		attempt = &stopAttempt{done: make(chan struct{})}
		l.stop = attempt
		cid := l.sess.ContainerID
		if !m.spawn(func() { m.runStop(l, attempt, cid, prev) }) {
			m.mu.Unlock()
			return Session{}, ErrClosed
		}
	}
	m.mu.Unlock()
	if attempt != nil {
		if err := m.wait(ctx, attempt.done); err != nil {
			return m.snapshot(l), err
		}
		if attempt.err != nil {
			return m.snapshot(l), fmt.Errorf("stopping the browser: %w", attempt.err)
		}
	}
	if err := m.wait(ctx, l.done); err != nil {
		return m.snapshot(l), err
	}
	return m.snapshot(l), nil
}

// runStop asks Docker to stop the container. On failure it puts the
// session back as it was, so a later Stop can try again.
func (m *Manager) runStop(l *live, attempt *stopAttempt, cid string, prev State) {
	ctx, cancel := context.WithTimeout(m.ctx, stopTimeout)
	defer cancel()
	err := m.rt.Stop(ctx, cid)
	if err != nil && m.ctx.Err() == nil {
		m.log.Warn("stopping the browser container", "container", cid, "err", err)
		m.mu.Lock()
		if l.sess.State == Stopping {
			l.sess.State = prev
			l.sess.UpdatedAt = m.now()
			if err := updateSession(context.Background(), m.db, l.sess); err != nil {
				m.log.Error("saving session state", "session", l.sess.ID, "err", err)
			}
		}
		l.stop = nil
		m.mu.Unlock()
	}
	attempt.err = err
	close(attempt.done)
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
		before := l.sess
		l.sess.State = to
		l.sess.UpdatedAt = m.now()
		if err := updateSession(context.Background(), m.db, l.sess); err != nil {
			l.sess = before
			return before, err
		}
		if to == NeedsHelp {
			// The agent is paused while a person has the browser: its
			// connections close, and new ones are refused until hand-back.
			l.disconnectLocked()
		}
		if to == Running {
			l.lastActivity = m.now() // idle time starts at hand-back
		}
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
		return l.snapshotLocked(), true
	}
	return Session{}, false
}

// List returns the live sessions, by profile.
func (m *Manager) List() []Session {
	m.mu.Lock()
	out := make([]Session, 0, len(m.live))
	for _, l := range m.live {
		out = append(out, l.snapshotLocked())
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
	// Move the folder aside first, so a failure leaves either everything
	// (the record and the folder) or a tombstone that Reconcile removes,
	// never logged-in browser data with no profile pointing at it. name was
	// validated when the profile was created, so this stays inside profiles/.
	dir := m.layout.dir(name)
	tomb := filepath.Join(m.layout.DataDir, "profiles", tombstonePrefix+name+"-"+newID())
	moved := true
	if err := os.Rename(dir, tomb); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("moving the profile folder aside: %w", err)
		}
		moved = false
	}
	if err := m.profiles.Delete(ctx, name); err != nil {
		if moved {
			if rerr := os.Rename(tomb, dir); rerr != nil {
				m.log.Error("putting the profile folder back", "profile", name, "err", rerr)
			}
		}
		return err
	}
	if moved {
		if err := os.RemoveAll(tomb); err != nil {
			m.log.Warn("removing a deleted profile's folder; will retry at the next start", "profile", name, "err", err)
		}
	}
	return nil
}

// tombstonePrefix names folders of deleted profiles not yet removed.
// Profile names can't start with a dot, so they never collide.
const tombstonePrefix = ".deleted-"

// removeTombstones finishes deleting profile folders a past deletion
// couldn't remove.
func (m *Manager) removeTombstones() {
	entries, err := os.ReadDir(filepath.Join(m.layout.DataDir, "profiles"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), tombstonePrefix) {
			if err := os.RemoveAll(filepath.Join(m.layout.DataDir, "profiles", e.Name())); err != nil {
				m.log.Warn("removing a deleted profile's folder", "folder", e.Name(), "err", err)
			}
		}
	}
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
