// Command entrypoint is PID 1 of the browser container. It starts Xvfb and
// Chromium, serves the control socket for the orchestrator, reaps every
// orphaned process, and on SIGTERM closes Chromium gracefully so recent
// cookie writes reach the profile before the container stops.
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
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
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

	if err := prepareProfile(cfg.ProfileDir); err != nil {
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

	chromium := exec.Command(chromiumBinary, chromiumArgs(cfg, smallShm())...)
	chromium.Env = append(os.Environ(), "DISPLAY="+display)
	browser, err := spawn(chromium)
	if err != nil {
		return fmt.Errorf("start Chromium: %w", err)
	}
	client := &http.Client{Timeout: time.Second}
	if err := waitFor(ctx, browser, 30*time.Second, func() bool {
		resp, err := client.Get("http://" + cdpAddr + "/json/version")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}); err != nil {
		browser.stop(syscall.SIGTERM, cfg.StopTimeout)
		return fmt.Errorf("chromium: %w", err)
	}

	ln, err := listenControl(cfg.ControlSocket)
	if err != nil {
		browser.stop(syscall.SIGTERM, cfg.StopTimeout)
		return err
	}
	defer func() { _ = os.Remove(cfg.ControlSocket) }()
	srv := &http.Server{
		Handler:           newControlHandler(cdpAddr, &streamer{command: func() *exec.Cmd { return exec.Command("gst-launch-1.0", streamArgs()...) }}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	log.Info("ready", "screen", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), "control_socket", cfg.ControlSocket)

	select {
	case <-browser.done:
		_ = srv.Close()
		return fmt.Errorf("chromium exited on its own: %s", describe(browser.status))
	case <-ctx.Done():
	}

	log.Info("stopping: closing Chromium", "timeout", cfg.StopTimeout)
	_ = srv.Close()
	if !browser.stop(syscall.SIGTERM, cfg.StopTimeout) {
		return fmt.Errorf("chromium did not close within %s and was killed; recent profile writes may be lost", cfg.StopTimeout)
	}
	log.Info("Chromium closed", "status", describe(browser.status))
	return nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
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

// process is a child started through the reaper.
type process struct {
	cmd    *exec.Cmd
	done   chan struct{}
	status syscall.WaitStatus // valid once done is closed
}

func spawn(cmd *exec.Cmd) (*process, error) {
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	exited, err := procs.start(cmd)
	if err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
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
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
	}
	_ = p.cmd.Process.Kill()
	<-p.done
	return false
}

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
			return fmt.Errorf("not ready after %s", timeout)
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
