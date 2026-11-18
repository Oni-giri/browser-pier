package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Visual mode's accessibility bus (PROJ-150). Chromium publishes its
// accessibility tree on it through the AT-SPI bridge (libatk-bridge,
// which the chromium package already pulls in), and the tree reader
// (tree.go) reads the page from it, so Visual mode reads the page without
// CDP. It is one private dbus-daemon (the image's own, which chromium's
// dependencies install) and nothing else: no session bus, no system bus,
// no at-spi2-core launcher or registry. Chromium finds it through
// AT_SPI_BUS_ADDRESS, and ACCESSIBILITY_ENABLED=1 makes it load the
// bridge with no session bus to ask (see images/chromium/ANTI-BOT.md).
//
// The bus listens on a path socket only, in a 0700 folder of the
// browser's uid under /tmp, never an abstract socket or TCP, and never in
// /run/browserpier, the folder the orchestrator shares. Only that uid may
// connect (dbus-daemon's EXTERNAL auth, and the folder's mode).
// Chromium's sandboxed renderers can't reach it: they have no access to
// the filesystem and no socket calls.
type a11yBus struct {
	dir  string
	proc *process
	log  *slog.Logger
}

// a11yDir is the bus's folder for uid.
func a11yDir(uid int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("browserpier-a11y-%d", uid))
}

// a11ySocket is the bus's socket in dir.
func a11ySocket(dir string) string { return filepath.Join(dir, "bus") }

// a11yConfig is the bus's dbus-daemon configuration: one path socket in
// dir, EXTERNAL authentication (the peer's uid, which must be the bus's
// own, as no <allow user> widens it), and a policy that lets connections
// call each other, which is what the bus is for.
func a11yConfig(dir string) string {
	return `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>accessibility</type>
  <listen>unix:path=` + a11ySocket(dir) + `</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*"/>
    <allow receive_sender="*"/>
    <allow own="*"/>
  </policy>
</busconfig>
`
}

// a11yEnv is the environment that points Chromium at the bus at addr.
func a11yEnv(addr string) []string {
	return []string{"AT_SPI_BUS_ADDRESS=" + addr, "ACCESSIBILITY_ENABLED=1"}
}

// prepareA11yDir makes the bus's folder, 0700 and the uid's own. A folder
// already there must be both, or someone else could reach the bus.
func prepareA11yDir(dir string, uid int) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("accessibility bus folder: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("accessibility bus folder: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != uid || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("accessibility bus folder %s: want a 0700 folder of uid %d, found %s", dir, uid, fi.Mode())
	}
	for _, name := range []string{"bus", "bus.conf"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("accessibility bus folder: %w", err)
		}
	}
	return nil
}

// startA11yBus starts the bus as a child the reaper knows and waits for
// its socket.
func startA11yBus(ctx context.Context, log *slog.Logger, uid int) (*a11yBus, error) {
	dir := a11yDir(uid)
	if err := prepareA11yDir(dir, uid); err != nil {
		return nil, err
	}
	conf := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(conf, []byte(a11yConfig(dir)), 0o600); err != nil {
		return nil, fmt.Errorf("accessibility bus: %w", err)
	}
	p, err := spawn(exec.Command("dbus-daemon", a11yArgs(conf)...))
	if err != nil {
		return nil, fmt.Errorf("start the accessibility bus: %w", err)
	}
	b := &a11yBus{dir: dir, proc: p, log: log}
	if err := waitFor(ctx, p, 10*time.Second, func() bool {
		fi, err := os.Stat(a11ySocket(dir))
		return err == nil && fi.Mode()&os.ModeSocket != 0
	}); err != nil {
		b.stop()
		return nil, fmt.Errorf("accessibility bus: %w", err)
	}
	return b, nil
}

// a11yArgs is dbus-daemon's command line: in the foreground, so the
// reaper has it, with no pid file and nothing sent to syslog.
func a11yArgs(conf string) []string {
	return []string{"--config-file=" + conf, "--nofork", "--nopidfile", "--nosyslog"}
}

// address is the bus's D-Bus address, or "" for no bus (API mode).
func (b *a11yBus) address() string {
	if b == nil {
		return ""
	}
	return "unix:path=" + a11ySocket(b.dir)
}

// done is closed when the bus has exited; nil (never) for no bus.
func (b *a11yBus) done() <-chan struct{} {
	if b == nil {
		return nil
	}
	return b.proc.done
}

// stop stops the bus; it is safe to call more than once, and on no bus.
func (b *a11yBus) stop() {
	if b == nil {
		return
	}
	select {
	case <-b.proc.done:
		return
	default:
	}
	b.proc.stop(syscall.SIGTERM, 5*time.Second)
}

// stopBrowser closes Chromium (closeChromium: gracefully, so recent
// cookie writes reach the profile), and only then stops the accessibility
// bus Chromium was connected to.
func stopBrowser(p *process, cdp string, timeout time.Duration, bus *a11yBus) error {
	err := closeChromium(p, cdp, timeout)
	bus.stop()
	return err
}

// waitEnd waits until Chromium exits (true) or a stop is asked for
// (false). The accessibility bus exiting ends neither: Chromium carries
// on without it, and tree reads answer an error until the session ends.
func waitEnd(ctx context.Context, log *slog.Logger, browser *process, bus *a11yBus) bool {
	busDone := bus.done()
	for {
		select {
		case <-browser.done:
			return true
		case <-ctx.Done():
			return false
		case <-busDone:
			log.Warn("the accessibility bus exited; Chromium carries on, without Visual mode's element tree", "status", describe(bus.proc.status))
			busDone = nil
		}
	}
}
