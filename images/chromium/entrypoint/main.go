// Command entrypoint is PID 1 of the browser container. It starts Xvfb, a
// window manager and Chromium, serves the control socket for the
// orchestrator, reaps every orphaned process, and on SIGTERM closes
// Chromium gracefully so recent cookie writes reach the profile before the
// container stops.
//
// Graceful means CDP's Browser.close: Chromium's SIGTERM handler exits
// before the cookie store is flushed, which loses a login made in the
// last half minute or so.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

func main() {
	if check := checkMode(os.Args); check != nil {
		os.Exit(check())
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

// checkMode is the short-lived check args ask for instead of the
// browser, or nil. A check runs before anything of the browser's: no
// mounts checked, no profile, no Xvfb or Chromium.
func checkMode(args []string) func() int {
	if len(args) != 2 {
		return nil
	}
	switch args[1] {
	case sandboxCheckArg:
		return runSandboxCheck
	case proxyCheckArg:
		return func() int { return runProxyCheck(os.Stdout) }
	}
	return nil
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	if err := setSubreaper(); err != nil {
		return fmt.Errorf("set child subreaper: %w", err)
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	if err := checkMounts(cfg); err != nil {
		return err
	}
	if err := prepareProfile(cfg.ProfileDir); err != nil {
		return err
	}
	if err := prepareHome(); err != nil {
		return err
	}

	// The forwarder comes up before anything goes out: every connection
	// Chromium makes goes through it, so with no forwarder there is no browser.
	upstream, err := readProxyFile(cfg.ProxyFile)
	if err != nil {
		return err
	}
	fwd, err := newForwarder(upstream, egressPolicy{localNetwork: cfg.LocalNetwork, host: cfg.HostIPs}, log)
	if err != nil {
		return err
	}
	// The blocker is there even when off, so its controls answer the
	// same either way.
	var set *domainset.Set
	if cfg.BlockAds {
		// Blocking is a convenience, not a boundary: without a usable
		// list the browser starts anyway, blocking nothing.
		if set, err = readBlockFile(cfg.BlockFile); err != nil {
			log.Warn("no ad and tracker list: nothing is blocked", "err", err)
		} else {
			log.Info("blocking ads and trackers", "names", set.Len())
		}
	}
	fwd.block = newBlocker(set)
	fwd.block.log = log
	fln, err := net.Listen("tcp", forwarderAddr)
	if err != nil {
		return fmt.Errorf("start the proxy forwarder: %w", err)
	}
	defer func() { _ = fln.Close() }()
	go fwd.serve(fln)
	client := lookupClient(fwd)
	where, err := decideGeo(cfg, upstream != nil, func() (string, string, string, error) {
		return lookupGeo(ctx, client, cfg.GeoURL)
	})
	if err != nil {
		return err
	}
	if where.Warning != "" {
		log.Warn(where.Warning)
	}
	log.Info("location", "country", where.Country, "time_zone", where.TimeZone, "locale", where.Locale, "source", where.Source)
	if err := applyAcceptLanguages(cfg.ProfileDir, where); err != nil {
		return err
	}

	xvfb, err := spawn(exec.Command("Xvfb", display,
		"-screen", "0", fmt.Sprintf("%dx%dx24", cfg.Width, cfg.Height), "-nolisten", "tcp"))
	if err != nil {
		return fmt.Errorf("start Xvfb: %w", err)
	}
	defer xvfb.stop(syscall.SIGTERM, 5*time.Second)
	if err := waitFor(ctx, xvfb, 10*time.Second, func() bool {
		_, err := os.Stat("/tmp/.X11-unix/X" + display[1:])
		return err == nil
	}); err != nil {
		return fmt.Errorf("xvfb: %w", err)
	}
	desktop, err := startDesktop(ctx, windowManager(where.env()), desktopReady, 10*time.Second)
	if err != nil {
		return err
	}
	desktopStopping := make(chan struct{})
	watchDesktop(log, desktop, desktopStopping)
	defer func() {
		close(desktopStopping)
		desktop.stop(syscall.SIGTERM, 5*time.Second)
	}()
	// Chromium starts once the network has stopped changing, so its first
	// page loads aren't cut off as the network "changes".
	if err := settleNetwork(ctx, log, xvfb, 5*time.Second, ipv6Settled); err != nil {
		return err
	}

	// The session bus and the file chooser portal come up before
	// Chromium, which looks for the portal once, as it starts. Without
	// them the browser doesn't start: Chromium would show its GTK dialog,
	// which lets whoever drives it hand a page any file the browser can
	// read, the profile's cookies included (PROJ-183), and it aborts when
	// its bus goes anyway. In Visual mode Chromium's accessibility goes
	// over the same bus, for the element tree (PROJ-150).
	bus, portal, err := startChooser(ctx, log, cfg)
	if err != nil {
		return err
	}
	defer bus.stop()
	defer portal.close()

	chromium := exec.Command(chromiumBinary, chromiumArgs(cfg, smallShm())...)
	chromium.Env = chromiumEnv(os.Environ(), where.env(), bus, cfg.Visual)
	fatal, err := watchFatal(chromium, func(msg string) { log.Warn(msg) })
	if err != nil {
		return fmt.Errorf("start Chromium: %w", err)
	}
	browser, err := spawn(chromium)
	pid := 0
	if err == nil {
		pid = chromium.Process.Pid
	}
	fatal.started(pid)
	if err != nil {
		return fmt.Errorf("start Chromium: %w", err)
	}
	portal.setBrowser(browser.pid)
	cdp := &http.Client{Timeout: time.Second}
	if err := waitFor(ctx, browser, 30*time.Second, func() bool {
		resp, err := cdp.Get("http://" + cdpAddr + "/json/version")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}); err != nil {
		ended, exited := time.Now(), false
		select {
		case <-browser.done:
			exited = true
		default:
		}
		reason := giveUp(fatal, ended, exited, func() { _ = stopBrowser(browser, cdpAddr, cfg.StopTimeout, portal, bus) })
		return fmt.Errorf("chromium: %w%s", err, reason)
	}

	ln, err := listenControl(cfg.ControlSocket)
	if err != nil {
		_ = stopBrowser(browser, cdpAddr, cfg.StopTimeout, portal, bus)
		return err
	}
	defer func() { _ = os.Remove(cfg.ControlSocket) }()
	vis := newVisual(cfg.Width, cfg.Height)
	if cfg.Visual {
		vis.tree = newTreeReader(bus.address(), cfg.Width, cfg.Height)
	}
	srv := &http.Server{
		Handler: newControlHandler(cdpAddr,
			&streamer{command: func() *exec.Cmd { return exec.Command("gst-launch-1.0", streamArgs()...) }},
			&streamer{command: func() *exec.Cmd { return exec.Command("gst-launch-1.0", recordArgs(cfg.Width, cfg.Height)...) },
				contentType: "video/mp4", wait: 2 * time.Second},
			vis, fwd, where, portal, "the file chooser portal isn't running in this browser"),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	log.Info("ready", "screen", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), "control_socket", cfg.ControlSocket)

	if !waitEnd(ctx, log, browser, bus) {
		ended := time.Now()
		_ = srv.Close()
		portal.browserExited()
		why := ""
		if busGone(bus) {
			why = " (the session bus had exited, and Chromium aborts without it)"
		}
		return fmt.Errorf("chromium exited on its own: %s%s%s", describe(browser.status), why, fatal.reason(false, ended, true))
	}

	log.Info("stopping: closing Chromium", "timeout", cfg.StopTimeout)
	_ = srv.Close()
	if err := stopBrowser(browser, cdpAddr, cfg.StopTimeout, portal, bus); err != nil {
		return err
	}
	log.Info("Chromium closed", "status", describe(browser.status))
	return nil
}

// lookupClient is what the location lookup goes out with: the way pages
// do, through the upstream proxy if any, but to the endpoint the admin
// set, so unchecked. The proxy check (proxycheck.go) uses it too, so it
// predicts a start.
func lookupClient(fwd *forwarder) *http.Client {
	if fwd.upstream == nil {
		return &http.Client{}
	}
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		return fwd.connect(ctx, addr)
	}}}
}

// prepareProfile checks the profile is writable and clears Chromium's
// singleton lock. A lock left by a killed container names another hostname,
// which makes Chromium refuse the profile; the orchestrator already ensures
// only one session runs per profile.
func prepareProfile(dir string) error {
	f, err := os.CreateTemp(dir, ".writable-")
	if err != nil {
		return fmt.Errorf("profile %s is not writable: %w", dir, err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", name, err)
		}
	}
	return nil
}

// mountMarker is the file the orchestrator writes into the profile and
// control folders before each start (see internal/sessions).
const mountMarker = ".browserpier-mount"

// checkMounts makes sure the folders mounted here are the ones the
// orchestrator prepared. If its idea of the data dir's host path is wrong,
// Docker mounts some other folder, and Chromium would open an empty profile
// while the real one sits untouched elsewhere.
func checkMounts(c config) error {
	if c.MountCheck == "" {
		return nil
	}
	for _, dir := range []string{c.ProfileDir, filepath.Dir(c.ControlSocket)} {
		b, err := os.ReadFile(filepath.Join(dir, mountMarker)) //nolint:gosec // dir is the container's own config
		if err != nil || strings.TrimSpace(string(b)) != c.MountCheck {
			return fmt.Errorf("%s is not the folder the orchestrator prepared (no matching %s); check BROWSERPIER_HOST_DATA_DIR", dir, mountMarker)
		}
	}
	return nil
}

// prepareHome creates $HOME. The orchestrator runs the container as its own
// uid, which has no home directory in the image.
func prepareHome() error {
	home := os.Getenv("HOME")
	if home == "" || home == "/" {
		return nil
	}
	if err := os.MkdirAll(home, 0o700); err != nil { //nolint:gosec // HOME is the container's own config
		return fmt.Errorf("create HOME: %w", err)
	}
	return nil
}

// smallShm reports whether /dev/shm is under 512 MiB (Docker's default is
// 64 MiB), which is too small for Chromium's shared memory.
func smallShm() bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/dev/shm", &st); err != nil {
		return true
	}
	return st.Blocks*uint64(st.Bsize) < 512<<20 //nolint:gosec // Bsize is never negative
}

func listenControl(path string) (net.Listener, error) {
	// Traversable by others: the orchestrator runs as a different user.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // see above
		return nil, fmt.Errorf("control socket dir: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale control socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	// The orchestrator runs as a different user. Access is limited by who
	// can reach the socket's directory, which is shared only with it.
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // see above
		_ = ln.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	return ln, nil
}

// closeChromium closes Chromium through Browser.close over CDP at cdp and
// waits up to timeout, then kills it. If CDP doesn't answer it falls back to
// SIGTERM, which can lose recent cookie writes, so that is an error too:
// nil means the profile is safely on disk.
func closeChromium(p *process, cdp string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := closeBrowser(ctx, cdp)
	cancel()
	if err != nil {
		exited := p.stop(syscall.SIGTERM, timeout)
		return fmt.Errorf("Browser.close over CDP failed (%w); stopped Chromium with SIGTERM instead (exited in time: %t), so recent cookie writes may be lost", err, exited) //nolint:staticcheck // Browser.close is a CDP method name
	}
	if !p.wait(timeout) {
		return fmt.Errorf("chromium did not close within %s and was killed; recent profile writes may be lost", timeout)
	}
	return nil
}

// process is a child started through the reaper.
type process struct {
	cmd    *exec.Cmd
	pid    int
	done   chan struct{}
	status syscall.WaitStatus // valid once done is closed
}

func spawn(cmd *exec.Cmd) (*process, error) {
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stderr
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// Any other writer makes exec copy in a goroutine that only cmd.Wait
	// ends, and the reaper never calls it.
	_, out := cmd.Stdout.(*os.File)
	_, errOut := cmd.Stderr.(*os.File)
	if !out || !errOut {
		return nil, errors.New("spawn: output must go to a file")
	}
	exited, pid, err := procs.startPid(cmd)
	if err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, pid: pid, done: make(chan struct{})}
	go func() {
		p.status = <-exited
		close(p.done)
	}()
	return p, nil
}

// stop sends sig and waits up to timeout, then kills the process. It reports
// whether the process exited before the timeout.
func (p *process) stop(sig syscall.Signal, timeout time.Duration) bool {
	_ = p.cmd.Process.Signal(sig)
	return p.wait(timeout)
}

// wait waits up to timeout for the process to exit, then kills it. It reports
// whether the process exited before the timeout.
func (p *process) wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
	}
	_ = p.cmd.Process.Kill()
	<-p.done
	return false
}

// settleNetwork waits for settled while Xvfb runs. Only its timeout lets
// startup go on: a stop, even one that comes as the wait ends, or Xvfb
// exiting ends it.
func settleNetwork(ctx context.Context, log *slog.Logger, xvfb *process, timeout time.Duration, settled func() bool) error {
	err := waitFor(ctx, xvfb, timeout, settled)
	switch {
	case ctx.Err() != nil:
		return errors.New("network: stopped during startup")
	case errors.Is(err, errNotReady):
		log.Warn("IPv6 addresses still tentative; starting Chromium anyway", "err", err)
		return nil
	case err != nil:
		return fmt.Errorf("xvfb: %w", err)
	}
	return nil
}

// errNotReady is waitFor's timeout.
var errNotReady = errors.New("not ready")

// waitFor polls ready until it returns true, p exits, ctx ends or timeout passes.
func waitFor(ctx context.Context, p *process, timeout time.Duration, ready func() bool) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-p.done:
			return fmt.Errorf("exited during startup: %s", describe(p.status))
		case <-ctx.Done():
			return errors.New("stopped during startup")
		case <-deadline:
			return fmt.Errorf("%w after %s", errNotReady, timeout)
		case <-tick.C:
		}
	}
	return nil
}

func describe(ws syscall.WaitStatus) string {
	if ws.Signaled() {
		return "killed by " + ws.Signal().String()
	}
	return "exit status " + strconv.Itoa(ws.ExitStatus())
}
