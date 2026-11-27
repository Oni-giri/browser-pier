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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/recording"
	"github.com/Oni-giri/browser-pier/internal/scrub"
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
	// Outdated reports whether a container labelled with this definition
	// version (docker.Container.Spec) must not run on: Reconcile stops it.
	Outdated(spec string) bool
	// TestProxy runs the proxy check with the proxy file in spec.Dir
	// (see TestProxy).
	TestProxy(ctx context.Context, spec docker.ProxyTestSpec) (docker.ProxyResult, error)
}

// Layout is where profile folders live:
//
//	<data dir>/profiles/<name>/chrome  Chromium's user data dir, mounted at /profile
//	<data dir>/profiles/<name>/run     the control socket, mounted at /run/browserpier
//	<data dir>/profiles/<name>/files   downloads/ and uploads/, mounted at /files
//	<data dir>/profiles/<name>/icon.png  the icon read from chrome/ at each stop, not mounted
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

// ProfileDir is a profile's whole folder, as the orchestrator sees it.
func (l Layout) ProfileDir(profile string) string { return l.dir(profile) }

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
	hostIPs      string // the Docker host's own addresses, comma separated
	// blocklist returns the ad and tracker list's file for a browser
	// container, nil for none (SetBlocklist).
	blocklist func() []byte
	// mem is the memory cap, set by SetMemoryLimit; nil for none.
	mem atomic.Pointer[memLimits]
	// hostFree reads what the host has free, set by SetHostFree; nil to
	// leave the host unchecked.
	hostFree func() (int64, error)

	ctx    context.Context // ends with Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	goMu   sync.Mutex
	closed bool // set by Close; guarded by goMu

	mu   sync.Mutex
	live map[string]*live // by profile name
	held map[string]bool  // profiles whose folder is being exported or imported (Hold)
	// draining is set by StopAll: no browser starts any more.
	draining bool
	// inputsDone is signalled, with mu, when an agent input action ends,
	// and when a person's input or hand-back does.
	inputsDone *sync.Cond
	// onEnd is called with a session's ID once it has ended (OnEnd).
	onEnd []func(id string)
	// onFailed is called with each session that failed, and how (OnFailed).
	onFailed []func(s Session, how EndReason)
	// secrets are the values BrowserPier typed into each live session's
	// page, by session ID (RegisterSecret); in memory only. Its lock
	// nests inside mu.
	secrets *scrub.Registry
	// markMu makes RegisterSecret's first secret wait until the session's
	// mark is stored, so no second caller types before it is.
	markMu sync.Mutex
	// kept are the runs whose sessions ended, by profile, oldest first
	// (Runs); in memory only.
	kept map[string][]*keptRun

	// rec records screens and recStore keeps the recordings
	// (SetRecorder); nil for none.
	rec      Recorder
	recStore *recording.Store

	// admit is held from checking there is room for a new browser to
	// recording its session, so two starts can't both squeeze in.
	admit sync.Mutex
	// proxyTest is held by the one proxy test running (TestProxy).
	proxyTest sync.Mutex
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
	// stopPrev is the state a failed stop puts back: what the session was
	// as the stop began, or what a handoff ending meanwhile left it
	// (SetNeedsHelp).
	stopPrev State
	// opCtx is for this session's calls on its profile's control socket
	// that run without m.mu: the person's input (HumanInput) and a
	// hand-back's lift and clipboard emptying. It ends as the session
	// does (end), and end waits for those calls (sockOps, humanInputs)
	// before it lets go of the profile, so none outlives the session to
	// reach the next browser on the same socket.
	opCtx    context.Context
	opCancel context.CancelFunc
	sockOps  int
	// stops counts the stops begun, failed ones included: a Take control
	// that waited across one gives up (TakeControlTurn).
	stops uint64
	// takeCtx is for the person's operations admitted under the takeover
	// in progress (HumanInput): a child of opCtx, cancelled the moment
	// its hand-back begins, so nothing admitted for it can reach the
	// browser once the clipboard is being emptied.
	takeCtx    context.Context
	takeCancel context.CancelFunc
	// conns are the agents' open connections to the browser.
	conns map[*conn]struct{}
	// lastActivity is when an agent or person last used the browser.
	lastActivity time.Time
	// lastAgentInput is when an agent last clicked or typed.
	lastAgentInput time.Time
	// inputs are agent input actions under way (AgentInput leases).
	inputs map[*lease]struct{}
	// leases are BrowserPier's own calls into the page under way
	// (PageLease); turn lets one run at a time.
	leases map[*lease]struct{}
	turn   chan struct{}
	// queued are page calls waiting for their turn, which a token's
	// revocation ends as it ends leases.
	queued map[*lease]struct{}
	// holds counts HoldSession holds: page calls wait while there is one.
	holds int
	// generation changes each time a person takes the browser.
	generation uint64
	// human is set while a person has control from the live view, and
	// holder names who: the opaque id TakeControl was given (PROJ-184).
	// humanGen is the generation the person in control took it at, which
	// is the number of their turn (TakeControlTurn): only input and a
	// hand-back that name it count, so a live view whose control ended
	// elsewhere can't use, or end, the next person's (PROJ-196).
	human    bool
	holder   string
	humanGen uint64
	// releasing is set while ReleaseControl empties the clipboard, before
	// human is cleared; humanInputs counts the person's inputs under way
	// (HumanInput), which it waits for first.
	releasing   bool
	humanInputs int
	// clipboardLeft is set when a hand-back came while the browser was
	// stopping and left its clipboard as it was: the container takes it
	// along, but if the stop fails, runStop empties it, for the takeover
	// clipboardLeftGen, before agents get the browser back.
	clipboardLeft    bool
	clipboardLeftGen uint64
	// released is closed once the hand-back in progress (releasing) is
	// over; releaseStarted is when it began.
	released       chan struct{}
	releaseStarted time.Time
	// viewers counts open live views.
	viewers int
	// exemptFull is set once the browser refused an exemption from the ad
	// and tracker list for its cap (logged once).
	exemptFull atomic.Bool
	// activity is the run's log, oldest first, and activityBytes what it
	// counts for (entrySize); it moves to the kept runs when the session
	// ends (keepRunLocked).
	activity      []Activity
	activityBytes int
	// lastTitle is a hash of the last tab title recorded (NoteTitle).
	lastTitle [32]byte
	// unwatchedAt is when the last live view went, if its "Nobody is
	// watching" entry may still be taken back.
	unwatchedAt time.Time
	// startedBy and fromApp say who started the browser (StartOptions).
	startedBy string
	fromApp   bool
	// stopWhy is why the stop in progress was asked for (stopPrev, what
	// it stopped from).
	stopWhy StopReason
	// record is the profile's Record sessions setting, as of the start or
	// the admin's last change (RefreshRecording).
	record bool
	// agentAt is when an agent last began or ended an action (a CDP
	// connection, a page call, an input action), for the recording's
	// tail; zero once a person takes the browser.
	agentAt time.Time
	// recPauses counts PauseRecording holds.
	recPauses int
	// recAllow and recWhy are what the recorder was last told
	// (regateLocked); recTimer looks again when the tail ends.
	recAllow bool
	recWhy   recording.Reason
	recEver  bool // the recorder was told yes once
	recTimer *time.Timer
	// calls are the receipts of calls with an id (PROJ-156), by token and
	// id; endSeq counts the ones that ended. They go with the session.
	calls  map[callKey]*receipt
	endSeq uint64
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

func newLive(parent context.Context, s Session) *live {
	l := &live{sess: s, ready: make(chan struct{}), done: make(chan struct{}), lastActivity: time.Now()}
	l.opCtx, l.opCancel = context.WithCancel(parent)
	return l
}

// markReady unblocks everyone waiting for the start to resolve, either way.
func (l *live) markReady() { l.readyOnce.Do(func() { close(l.ready) }) }

func NewManager(db *sql.DB, rt Runtime, layout Layout, log *slog.Logger) (*Manager, error) {
	if err := layout.check(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
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
		held:         map[string]bool{},
		secrets:      scrub.NewRegistry(),
		kept:         map[string][]*keptRun{},
	}
	m.inputsDone = sync.NewCond(&m.mu)
	return m, nil
}

// OnEnd registers f to be called, without the manager's lock, with each
// session's ID once the session has ended (stopped or failed), so what
// others keep about it can go.
func (m *Manager) OnEnd(f func(id string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEnd = append(m.onEnd, f)
}

// OnFailed registers f to be called, without the manager's lock, with each
// session that fails and how: one of the failed EndReasons (EndReason.
// Failed), in fixed words, never its error or its container's log, which a
// page can write to. A start cut short by BrowserPier shutting down ends
// with EndShutdown, so it isn't one. f must not wait.
func (m *Manager) OnFailed(f func(s Session, how EndReason)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onFailed = append(m.onFailed, f)
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

// SetHostIPs gives the Docker host's own addresses (its public IP, say),
// which pages may reach only with the profile's local-network opt-in, as
// the host's private ones. Call it before Start.
func (m *Manager) SetHostIPs(ips []string) { m.hostIPs = strings.Join(ips, ",") }

func (m *Manager) Layout() Layout { return m.layout }

func containerName(profile string) string { return "browserpier-" + profile }

// StartOptions are what a start may ask beyond the profile's settings.
type StartOptions struct {
	// Mode is the session's mode; "" means the profile's default.
	Mode profiles.Mode
	// AllowAds turns the profile's ad and tracker blocking off for this
	// session, a running one included. Blocking can only be turned off
	// this way, never on, and the profile's setting doesn't change.
	AllowAds bool
	// By is the name of the agent's token starting it, if an agent is.
	By string
	// FromApp: the admin starts it from the web UI. The run's log then
	// starts with "Started from the app".
	FromApp bool
}

// Start starts the profile's browser, or attaches to it if it is already
// starting or running (attached is then true). It returns once the browser
// is ready or has failed. mode "" means the profile's default.
func (m *Manager) Start(ctx context.Context, profile string, mode profiles.Mode) (s Session, attached bool, err error) {
	return m.StartWith(ctx, profile, StartOptions{Mode: mode})
}

// StartWith is Start with options.
func (m *Manager) StartWith(ctx context.Context, profile string, opts StartOptions) (s Session, attached bool, err error) {
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
			s, attached, err := m.awaitStart(ctx, l, true)
			if err == nil && opts.AllowAds && s.BlockAds {
				s = m.allowAds(ctx, l)
			} else if err == nil && opts.AllowAds && s.AdListMissing {
				// Nothing to turn off, but it is off now by the agent's
				// choice, not for want of a list.
				_ = m.update(l, func(s *Session) { s.AdListMissing = false })
				s = m.snapshot(l)
			}
			return s, attached, err
		}
		l, p, again, err := m.admitAndBegin(ctx, profile, opts)
		if err != nil {
			return Session{}, false, err
		}
		if again {
			continue // another start got there first: attach to it
		}
		if !m.spawn(func() { m.launch(l, p) }) {
			m.end(l, "", func(s *Session) StopReason {
				s.State, s.Error = Failed, "BrowserPier was shutting down"
				return stopShutdown // not a failure: no webhook is told
			})
			return Session{}, false, ErrClosed
		}
		return m.awaitStart(ctx, l, false)
	}
}

// admitAndBegin makes room for a new browser and records its session, one
// start at a time: the admission lock is held from the memory check until
// the session counts towards memory. again is true if a session for the
// profile appeared meanwhile, to attach to instead.
func (m *Manager) admitAndBegin(ctx context.Context, profile string, opts StartOptions) (l *live, p profiles.Profile, again bool, err error) {
	mode := opts.Mode
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
	l, p, err = m.begin(ctx, profile, opts)
	return l, p, false, err
}

func (m *Manager) hasLive(profile string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[profile]
	return ok
}

// begin records a new Starting session. m.mu must be held.
func (m *Manager) begin(ctx context.Context, profile string, opts StartOptions) (*live, profiles.Profile, error) {
	mode := opts.Mode
	if m.draining {
		return nil, profiles.Profile{}, ErrClosed
	}
	if m.held[profile] {
		return nil, profiles.Profile{}, ErrProfileHeld
	}
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
	s := Session{ID: newID(), Profile: profile, Mode: mode, State: Starting, CreatedAt: now, UpdatedAt: now, BlockAds: p.BlockAds && !opts.AllowAds}
	if err := insertSession(ctx, m.db, s); err != nil {
		return nil, p, err
	}
	l := newLive(m.ctx, s)
	l.lastActivity = now
	l.record = p.Record
	l.startedBy, l.fromApp = opts.By, opts.FromApp
	if opts.FromApp {
		l.noteLocked(now, KindSession, startedFromApp)
	}
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
	if err := m.forgetTabsIfFilled(ctx, p.Name, id); err != nil {
		m.abort(l, "", fmt.Sprintf("closing the tabs of a session a secret was typed into: %v", err))
		return
	}
	if err := m.writeProxyFile(ctx, p); err != nil {
		m.abort(l, "", fmt.Sprintf("handing over the proxy: %v", err))
		return
	}
	wantBlocking := m.snapshot(l).BlockAds
	blocking, err := m.writeBlockFile(p.Name, wantBlocking)
	if err != nil {
		m.abort(l, "", fmt.Sprintf("handing over the ad and tracker list: %v", err))
		return
	}
	if wantBlocking && !blocking {
		// No list yet: the session says what the browser does, which is
		// block nothing, and why.
		if err := m.update(l, func(s *Session) { s.BlockAds, s.AdListMissing = false, true }); err != nil {
			m.abort(l, "", fmt.Sprintf("saving the session: %v", err))
			return
		}
	}
	// A container left behind by a crash would hold the name.
	if err := m.rt.RemoveStale(ctx, containerName(p.Name), p.Name); err != nil {
		m.abort(l, "", err.Error())
		return
	}
	spec := docker.BrowserSpec{
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
			"BROWSERPIER_LOCAL_NETWORK=" + strconv.FormatBool(p.LocalNetwork),
			"BROWSERPIER_HOST_IPS=" + m.hostIPs,
			"BROWSERPIER_BLOCK_ADS=" + strconv.FormatBool(blocking),
			// Visual mode turns on Chromium's accessibility, for its
			// element tree (PROJ-150); fixed for the session, as its
			// mode is. The guard allows it (internal/docker/guard.go).
			"BROWSERPIER_MODE=" + string(m.snapshot(l).Mode),
		},
	}
	var cid string
	for retried := false; ; retried = true {
		var err error
		cid, err = m.rt.Create(ctx, spec)
		if errors.Is(err, docker.ErrOutsideEnvelope) {
			// It says which container it refused, and why.
			m.abort(l, "", err.Error())
			return
		}
		if err != nil {
			m.abort(l, "", fmt.Sprintf("creating the browser container: %v", err))
			return
		}
		if err := m.update(l, func(s *Session) { s.ContainerID = cid }); err != nil {
			// Unrecorded, the container would be stopped as a stray after a restart.
			m.abort(l, cid, fmt.Sprintf("saving the session: %v", err))
			return
		}
		err = m.rt.Start(ctx, cid)
		if err == nil {
			break
		}
		// The AppArmor profile BrowserPier chose was unloaded since, say with
		// apparmor_parser -R: the runtime checks again at the next Create.
		if !retried && errors.Is(err, docker.ErrAppArmorNotLoaded) {
			m.log.Warn("the browser's AppArmor profile isn't loaded any more; choosing again", "profile", p.Name, "err", err)
			if rmErr := m.rt.Remove(ctx, cid); rmErr == nil {
				continue
			}
		}
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
	// A browser that closed cleanly has written and let go of its
	// databases: read its icon now, while the session is still Stopping,
	// so nothing can start it again or export it meanwhile.
	m.mu.Lock()
	graceful := l.failure == "" && waitErr == nil && code == 0 && l.sess.State == Stopping
	profile := l.sess.Profile
	m.mu.Unlock()
	if graceful {
		m.refreshIcon(profile)
	}
	m.end(l, lastLog, func(s *Session) StopReason {
		switch {
		case l.failure != "":
			// Set while it was starting: it never got to Running.
			s.State, s.Error = Failed, l.failure
			return endStartFailed
		case waitErr != nil:
			s.State, s.Error = Failed, fmt.Sprintf("lost track of the browser container: %v", waitErr)
			return endLost
		case s.State == Stopping && code == 0:
			s.State = Stopped
			if l.stopWhy.end == "" {
				return endUnknown // a stop the BrowserPier before a restart began
			}
			return l.stopWhy
		case s.State == Stopping:
			s.State, s.Error = Failed, fmt.Sprintf("the browser didn't close cleanly (exit status %d), so recent logins may be lost", code)
			return endUnclean
		case s.State == Starting:
			s.State, s.Error = Failed, fmt.Sprintf("the browser exited unexpectedly (exit status %d)", code)
			return endStartFailed
		default:
			s.State, s.Error = Failed, fmt.Sprintf("the browser exited unexpectedly (exit status %d)", code)
			return endCrashed
		}
	})
}

// CrashReason is why the browser's entrypoint says it exited, from the
// container's last log line: Chromium's FATAL line, if there was one
// (PROJ-36), cleaned and capped. A compromised page could have written it,
// so it is never part of Session.Error: show it next to the error, and
// fence it for agents.
func CrashReason(lastLog string) string {
	why := entrypointError(lastLog)
	if why == "" {
		return ""
	}
	return capRunes(fence.Clean(why), 400)
}

// entrypointError is the err of the browser container's last log line,
// when the entrypoint ended on one: `level=ERROR msg=exiting err="..."`.
// It carries Chromium's FATAL line if there was one (PROJ-36).
func entrypointError(lastLog string) string {
	head := lastLog
	if strings.HasPrefix(head, "time=") {
		_, head, _ = strings.Cut(head, " ")
	}
	if !strings.HasPrefix(head, "level=ERROR ") {
		return ""
	}
	_, v, ok := strings.Cut(lastLog, " err=")
	if !ok {
		return ""
	}
	if !strings.HasPrefix(v, `"`) {
		v, _, _ = strings.Cut(v, " ")
		return v
	}
	q, err := strconv.QuotedPrefix(v)
	if err != nil {
		// Cut short by the log line cap: keep what there is.
		return strings.TrimSuffix(v[1:], "…")
	}
	v, err = strconv.Unquote(q)
	if err != nil {
		return ""
	}
	return v
}

func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// abort fails a session whose container never got going. With
// BrowserPier shutting down, which cancels whatever the start was
// waiting on, it is no failure to tell anyone of.
func (m *Manager) abort(l *live, cid, reason string) {
	lastLog := ""
	if cid != "" {
		lastLog = m.cleanup(cid)
	}
	closing := m.ctx.Err() != nil
	m.end(l, lastLog, func(s *Session) StopReason {
		if closing {
			// Ended with BrowserPier, not a failure: no webhook is told.
			s.State, s.Error = Failed, "BrowserPier was shutting down: "+reason
			return stopShutdown
		}
		s.State, s.Error = Failed, reason
		return endStartFailed
	})
}

// proxyFile is where the proxy URL waits for the entrypoint, which reads
// and deletes it before Chromium starts (images/chromium/entrypoint).
const proxyFile = "proxy.json"

// writeProxyFile hands the profile's proxy, credentials included, to the
// browser container through its control folder, never its environment.
// A template's {sticky} is filled in here, and only here, with the
// profile's own token: what is stored and shown stays the template.
func (m *Manager) writeProxyFile(ctx context.Context, p profiles.Profile) error {
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
	proxy := p.Proxy
	if profiles.HasSticky(proxy) {
		// Read again rather than taken from p: a profile from before
		// tokens gets its own here, once.
		tok, err := m.profiles.StickyToken(ctx, p.Name)
		if err != nil {
			return err
		}
		proxy = profiles.FillSticky(proxy, tok)
	}
	b, err := json.Marshal(map[string]string{"url": proxy})
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
// proxy credentials are left in its control folder. final sets the final
// state and says how the run ended, which ends its activity log before
// the log is kept (Runs). If the final state can't be saved after a few
// tries, the profile is released anyway: the next Start for it, or the
// next Reconcile, clears the stale row.
func (m *Manager) end(l *live, lastLog string, final func(*Session) StopReason) {
	// The container's words are kept and shown after the session, when
	// its secrets are gone: scrubbed now (PROJ-125).
	sc := m.lockScrubber(l.sess.ID)
	// A handoff was open: it is cancelled once the session has ended.
	inHelp := l.sess.State == NeedsHelp || (l.sess.State == Stopping && l.stop != nil && l.stopPrev == NeedsHelp)
	why := final(&l.sess)
	l.sess.LastLog = sc.String(lastLog)
	l.sess.Error = sc.String(l.sess.Error)
	l.sess.UpdatedAt = m.now()
	m.regateLocked(l)
	// No more calls on the control socket for this session: those under
	// way break off (and are waited for below), and HumanInput and a
	// hand-back admit no more, the session being final.
	l.opCancel()
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
	// The profile's next browser listens on the same socket: nothing
	// admitted for this one may still be on its way there.
	m.waitSockOpsLocked(l)
	m.mu.Unlock()
	// The session's final secrets: one registered while the state was
	// saved is in them.
	sc = m.lockScrubber(s.ID)
	if m.live[s.Profile] == l {
		delete(m.live, s.Profile)
	}
	m.inputsDone.Broadcast() // a hand-back waiting on this session's input
	// Scrubbed with them before they are forgotten: after that, nothing
	// could hide them in the kept log (PROJ-158).
	m.keepRunLocked(l, s, why, inHelp, sc)
	m.secrets.Forget(s.ID)
	l.calls = nil
	l.disconnectLocked()
	l.cancelAllLocked(ErrStopping)
	hooks := slices.Clone(m.onEnd)
	if l.recTimer != nil {
		l.recTimer.Stop()
		l.recTimer = nil
	}
	failed := slices.Clone(m.onFailed)
	m.mu.Unlock()
	if m.rec != nil {
		m.rec.End(s.ID)
	}
	for _, f := range hooks {
		f(s.ID)
	}
	if s.State == Failed && why.Reason().Failed() {
		for _, f := range failed {
			f(s, why.Reason())
		}
	}
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
	m.regateLocked(l)
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
	s.Record, s.Recording = l.record, l.recAllow
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
	m.agentActedLocked(l)
	var once sync.Once
	return cctx, func() {
		once.Do(func() {
			cancel()
			m.mu.Lock()
			delete(l.conns, c)
			l.lastActivity = m.now()
			if m.live[l.sess.Profile] == l {
				m.agentActedLocked(l)
			}
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
// it is revoked, and cancels its page calls (PageLease) and input actions
// (AgentInput) under way with ErrRevoked.
func (m *Manager) DisconnectToken(tokenID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.live {
		for c := range l.conns {
			if c.tokenID == tokenID {
				c.cancel()
			}
		}
		for ls := range l.leases {
			if ls.tokenID == tokenID {
				ls.cancel(ErrRevoked)
			}
		}
		for q := range l.queued {
			if q.tokenID == tokenID {
				q.cancel(ErrRevoked)
			}
		}
		for in := range l.inputs {
			if in.tokenID == tokenID {
				in.cancel(ErrRevoked)
			}
		}
	}
}

// ConnectedTokens lists the tokens with an open connection to any
// browser, or a page call (PageLease) under way or waiting for its turn,
// or an input action (AgentInput) under way.
func (m *Manager) ConnectedTokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, l := range m.live {
		for c := range l.conns {
			add(c.tokenID)
		}
		for ls := range l.leases {
			add(ls.tokenID)
		}
		for q := range l.queued {
			add(q.tokenID) // waiting for its turn
		}
		for in := range l.inputs {
			add(in.tokenID)
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

// Stop is StopFor with StopFromApp, the admin's reason.
func (m *Manager) Stop(ctx context.Context, id string) (Session, error) {
	return m.StopFor(ctx, id, StopFromApp)
}

// StopFor closes the session's browser gracefully and waits until its
// container is gone; why says who asked, in the run's log and its kept
// run (the first stop's reason, when several ask at once). Stopping a
// session that has already ended returns it. If Docker fails to stop
// the container, the session goes back to the state it was in and every
// caller gets the error.
func (m *Manager) StopFor(ctx context.Context, id string, why StopReason) (Session, error) {
	return m.stopIf(ctx, id, nil, why)
}

// errStillInUse: stopIf found the session no longer met its condition.
var errStillInUse = errors.New("the browser is in use again")

// stopIf is Stop, but only if cond (checked under the lock, in the same
// step that moves the session to Stopping) holds; otherwise it returns
// errStillInUse. It closes the race between picking an idle browser and
// an agent attaching to it.
func (m *Manager) stopIf(ctx context.Context, id string, cond func(*live) bool, why StopReason) (Session, error) {
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
		l.stopPrev = l.sess.State
		l.sess.State = Stopping
		l.stops++
		l.sess.UpdatedAt = m.now()
		m.regateLocked(l)
		if err := updateSession(context.Background(), m.db, l.sess); err != nil {
			// Stopping anyway beats refusing; the final state is saved with retries.
			m.log.Error("saving session state", "session", id, "err", err)
		}
		attempt = &stopAttempt{done: make(chan struct{})}
		l.stop = attempt
		l.stopWhy = why
		// Page calls under way stop first (an act lets go of what it
		// pressed), then the browser closes.
		l.cancelAllLocked(ErrStopping)
		for len(l.leases) > 0 {
			m.inputsDone.Wait()
		}
		// The wait let go of the lock: the browser may have exited and
		// the session ended meanwhile, with nothing left to stop.
		if m.findLocked(id) != l || l.sess.State != Stopping {
			close(attempt.done)
			m.mu.Unlock()
			return m.waitStopped(ctx, l)
		}
		cid := l.sess.ContainerID
		if !m.spawn(func() { m.runStop(l, attempt, cid) }) {
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

// waitStopped waits for a session to end and returns it.
func (m *Manager) waitStopped(ctx context.Context, l *live) (Session, error) {
	if err := m.wait(ctx, l.done); err != nil {
		return m.snapshot(l), err
	}
	return m.snapshot(l), nil
}

// runStop asks Docker to stop the container. On failure it puts the
// session back as it was, so a later Stop can try again. An error that
// comes back after the container has exited and the session has ended is
// no failure: the session's final state says how the browser stopped
// (PROJ-64).
func (m *Manager) runStop(l *live, attempt *stopAttempt, cid string) {
	ctx, cancel := context.WithTimeout(m.ctx, stopTimeout)
	defer cancel()
	err := m.rt.Stop(ctx, cid)
	if err != nil && m.ctx.Err() == nil {
		select {
		case <-l.done:
			m.log.Warn("the browser container stopped, but Docker reported an error stopping it; check the host's container runtime",
				"container", cid, "err", err)
			err = nil
		default:
			m.log.Warn("stopping the browser container", "container", cid, "err", err)
			m.mu.Lock()
			if l.sess.State == Stopping && l.clipboardLeft {
				// A person's control ended during the stop, with no
				// clipboard emptied: empty it now, while the session is
				// still Stopping and agents are kept out, before the
				// gate or any agent can have the browser (PROJ-194,
				// PROJ-23).
				l.clipboardLeft = false
				id, profile, gen := l.sess.ID, l.sess.Profile, l.clipboardLeftGen
				m.unlockedOpLocked(l, func(ctx context.Context) {
					// As ReleaseControl does: a copy pressed just before
					// the stop lands first, and is emptied too.
					if settle(ctx) {
						m.emptyClipboard(ctx, id, profile, gen)
					}
				})
			}
			if l.sess.State == Stopping {
				l.sess.State = l.stopPrev
				l.sess.UpdatedAt = m.now()
				m.regateLocked(l)
				if err := updateSession(context.Background(), m.db, l.sess); err != nil {
					m.log.Error("saving session state", "session", l.sess.ID, "err", err)
				}
			}
			l.stop = nil
			l.stopWhy = StopReason{}
			m.mu.Unlock()
		}
	}
	attempt.err = err
	close(attempt.done)
}

// ErrHelpOpen: the browser already waits for help (another handoff).
var ErrHelpOpen = errors.New("the browser already waits for help")

// OpenHelp moves a running session to Needs help for a handoff, and
// returns which time it went there (its help epoch), which the handoff
// keeps to end only its own help (EndHandoff). A session that already
// waits for help refuses, with ErrHelpOpen: two handoffs never share one.
func (m *Manager) OpenHelp(id string) (epoch uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	switch {
	case l == nil:
		return 0, ErrNotRunning
	case l.sess.State == NeedsHelp:
		return 0, ErrHelpOpen
	case l.sess.State != Running:
		return 0, ErrNotRunning
	}
	if _, err := m.setNeedsHelpLocked(l, true); err != nil {
		return 0, err
	}
	return l.sess.helpEpoch, nil
}

// SetNeedsHelp moves a running session to Needs help and back. A handoff
// opens with OpenHelp and ends with EndHandoff, which also ends its
// person's control.
func (m *Manager) SetNeedsHelp(id string, needsHelp bool) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil {
		return Session{}, ErrNotRunning
	}
	return m.setNeedsHelpLocked(l, needsHelp)
}

// EndHandoff ends the handoff that opened Needs help as epoch (OpenHelp),
// in one step under m.mu: the browser stops waiting for that help, and
// the control of whoever has it then, the person helping, begins to end.
// It doesn't wait for that itself: done is closed once agents have the
// browser (nil if no one had control), and the handoff waits for it
// before it tells anyone the handoff has ended, so that is only ever said
// once true (internal/handoff). Whoever takes control after it is someone
// else, whose control this never ends; nor does an end for another
// handoff's epoch, which changes nothing and returns ErrHandoffEnded
// (PROJ-196).
//
// The browser stops waiting for help even if that can't be saved (it is
// logged, and a restart finds no open handoff and puts it back Running):
// a handoff that has ended never leaves its agent paused.
func (m *Manager) EndHandoff(id string, epoch uint64) (done <-chan struct{}, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil {
		return nil, ErrNotRunning
	}
	if epoch == 0 || l.sess.helpEpoch != epoch {
		return nil, ErrHandoffEnded
	}
	switch {
	case l.sess.State == NeedsHelp:
		l.sess.State = Running
		l.sess.UpdatedAt = m.now()
		if err := updateSession(context.Background(), m.db, l.sess); err != nil {
			m.log.Error("saving session state", "session", id, "err", err)
		}
		m.personTookLocked(l)
		// A file chooser open passes back to the agent with its whole
		// time limit (PROJ-181).
		m.restartChoosers(l.sess.Profile)
	case l.sess.State == Stopping && l.stop != nil && l.stopPrev == NeedsHelp:
		// Should the stop fail, it is put back Running.
		l.stopPrev = Running
	default:
		return nil, ErrHandoffEnded
	}
	l.lastActivity = m.now() // idle time starts at hand-back
	if l.human {
		done = m.beginReleaseLocked(l, id)
	}
	return done, nil
}

// setNeedsHelpLocked is SetNeedsHelp, with m.mu held.
func (m *Manager) setNeedsHelpLocked(l *live, needsHelp bool) (Session, error) {
	from, to := Running, NeedsHelp
	if !needsHelp {
		from, to = NeedsHelp, Running
	}
	switch l.sess.State {
	case to:
	case Stopping:
		if needsHelp || l.stop == nil || l.stopPrev != from {
			return l.sess, ErrNotRunning
		}
		// A handoff ending while the browser stops: should the stop
		// fail, it is put back Running, not waiting for help that is
		// over (and with no one left to hand back).
		l.stopPrev = to
		l.lastActivity = m.now() // idle time starts at hand-back
	case from:
		before := l.sess
		l.sess.State = to
		if to == NeedsHelp {
			l.sess.helpEpoch++ // saved with the state
		}
		l.sess.UpdatedAt = m.now()
		if err := updateSession(context.Background(), m.db, l.sess); err != nil {
			l.sess = before
			return before, err
		}
		// Recording stops before anything else, and before the handoff
		// can open: the person's input mustn't be kept.
		m.personTookLocked(l)
		if to == NeedsHelp {
			// The agent is paused while a person has the browser: its
			// connections close, and new ones are refused until hand-back.
			l.disconnectLocked()
			l.generation++
			// Page calls and agent input under way are cancelled (a long
			// human-paced type breaks off at once); they finish before
			// the person gets the browser. New ones are refused from now
			// on.
			l.cancelAllLocked(ErrHandedOver)
			m.waitLeasesLocked(l)
		}
		if to == Running {
			l.lastActivity = m.now() // idle time starts at hand-back
		}
		// A file chooser open at either end passes to the person, or back
		// to the agent, with its whole time limit (PROJ-181).
		m.restartChoosers(l.sess.Profile)
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
	if m.held[name] {
		return ErrProfileHeld
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
	putBack := func() {
		if moved {
			if rerr := os.Rename(tomb, dir); rerr != nil {
				m.log.Error("putting the profile folder back", "profile", name, "err", rerr)
			}
		}
	}
	// Its recordings go with it, moved aside the same way.
	restoreRec, removeRec := func() error { return nil }, func() {}
	if m.recStore != nil {
		var err error
		if restoreRec, removeRec, err = m.recStore.MoveAside(name, newID()); err != nil {
			putBack()
			return err
		}
	}
	if err := m.profiles.Delete(ctx, name); err != nil {
		putBack()
		if rerr := restoreRec(); rerr != nil {
			m.log.Error("putting the profile's recordings back", "profile", name, "err", rerr)
		}
		return err
	}
	removeRec()
	if moved {
		if err := os.RemoveAll(tomb); err != nil {
			m.log.Warn("removing a deleted profile's folder; will retry at the next start", "profile", name, "err", err)
		}
	}
	delete(m.kept, name)
	return nil
}

// tombstonePrefix names folders of deleted profiles not yet removed, and
// ImportPrefix folders an import was filling when it stopped. Profile
// names can't start with a dot, so they never collide.
const (
	tombstonePrefix = ".deleted-"
	ImportPrefix    = ".importing-"
)

// removeTombstones finishes deleting profile folders a past deletion
// couldn't remove.
func (m *Manager) removeTombstones() {
	entries, err := os.ReadDir(filepath.Join(m.layout.DataDir, "profiles"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), tombstonePrefix) || strings.HasPrefix(e.Name(), ImportPrefix)) {
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
