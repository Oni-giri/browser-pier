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

// The session bus (PROJ-152): one private dbus-daemon, the image's own,
// for Chromium, the file chooser portal (portal.go) and, in Visual mode,
// the tree reader (tree.go, a11y.go) only. Chromium asks the desktop
// portal on it for its file choosers and "Save as"
// (images/chromium/ANTI-BOT.md), and the portal answers them, so no
// dialog shows on the screen. In Visual mode Chromium's accessibility
// bridge publishes the page's tree on it too (PROJ-150). Nothing else is
// on it: no system bus, no xdg-desktop-portal, no AT-SPI launcher or
// registry, no service activation.
//
// It listens on a path socket only, in a 0700 folder of the browser's
// uid under /tmp, never an abstract socket or TCP, and never in
// /run/browserpier (shared with the orchestrator), /files or /profile.
// Only that uid may connect: dbus-daemon's EXTERNAL authentication
// accepts the daemon's own uid and no <allow user> widens it, and the
// folder's mode keeps everyone else from the socket. Chromium's
// sandboxed renderers can't reach it: they have no filesystem and no
// socket calls.
//
// Chromium aborts when its session bus goes away ("D-Bus connection was
// disconnected. Aborting."), so the bus is as vital to the browser as
// Xvfb: it is stopped only after Chromium has closed.
type sessionBus struct {
	dir  string
	proc *process
}

// busDir is the session bus's folder for uid.
func busDir(uid int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("browserpier-bus-%d", uid))
}

// busSocket is the bus's socket in dir.
func busSocket(dir string) string { return filepath.Join(dir, "bus") }

// busConfig is the bus's dbus-daemon configuration: one path socket in
// dir, EXTERNAL authentication, no service directory (so nothing is ever
// started on demand), and a policy that lets the connections, all of the
// browser's uid, call each other, and own no well-known name but the
// portal's (the portal takes it first). No connection may eavesdrop.
func busConfig(dir string) string {
	return `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=` + busSocket(dir) + `</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="false"/>
    <allow receive_sender="*"/>
    <allow own="` + portalName + `"/>
  </policy>
</busconfig>
`
}

// busArgs is dbus-daemon's command line: in the foreground, so the
// reaper has it, with no pid file and nothing sent to syslog.
func busArgs(conf string) []string {
	return []string{"--config-file=" + conf, "--nofork", "--nopidfile", "--nosyslog"}
}

// prepareBusDir makes the bus's folder, 0700 and the uid's own. A folder
// already there must be both, or someone else could reach the bus.
func prepareBusDir(dir string, uid int) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("session bus folder: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("session bus folder: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != uid || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("session bus folder %s: want a 0700 folder of uid %d, found %s", dir, uid, fi.Mode())
	}
	for _, name := range []string{"bus", "bus.conf"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session bus folder: %w", err)
		}
	}
	return nil
}

// startSessionBus starts the bus as a child the reaper knows and waits
// for its socket.
func startSessionBus(ctx context.Context, uid int) (*sessionBus, error) {
	dir := busDir(uid)
	if err := prepareBusDir(dir, uid); err != nil {
		return nil, err
	}
	conf := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(conf, []byte(busConfig(dir)), 0o600); err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	p, err := spawn(exec.Command("dbus-daemon", busArgs(conf)...))
	if err != nil {
		return nil, fmt.Errorf("start the session bus: %w", err)
	}
	b := &sessionBus{dir: dir, proc: p}
	if err := waitFor(ctx, p, 10*time.Second, func() bool {
		fi, err := os.Stat(busSocket(dir))
		return err == nil && fi.Mode()&os.ModeSocket != 0
	}); err != nil {
		b.stop()
		return nil, fmt.Errorf("session bus: %w", err)
	}
	return b, nil
}

// address is the bus's D-Bus address, or "" for no bus.
func (b *sessionBus) address() string {
	if b == nil {
		return ""
	}
	return "unix:path=" + busSocket(b.dir)
}

// env is what points Chromium, and nothing else, at the bus.
func (b *sessionBus) env() []string {
	if b == nil {
		return nil
	}
	return []string{"DBUS_SESSION_BUS_ADDRESS=" + b.address()}
}

// done is closed when the bus has exited; nil (never) for no bus.
func (b *sessionBus) done() <-chan struct{} {
	if b == nil {
		return nil
	}
	return b.proc.done
}

// stop stops the bus; it is safe to call more than once, and on no bus.
func (b *sessionBus) stop() {
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

// stopBrowser closes Chromium gracefully (closeChromium, so recent
// cookie writes reach the profile), then ends the portal's requests and
// only then stops the bus Chromium was connected to: stopped first, the
// bus would make Chromium abort before it had flushed anything.
func stopBrowser(p *process, cdp string, timeout time.Duration, portal *portal, bus *sessionBus) error {
	err := closeChromium(p, cdp, timeout)
	portal.close()
	bus.stop()
	return err
}

// startChooser starts the session bus and the file chooser portal on it.
// Either failing fails the browser's start (PROJ-181): Chromium's own
// dialog, its fallback, browses the whole container (PROJ-183).
func startChooser(ctx context.Context, log *slog.Logger, cfg config) (*sessionBus, *portal, error) {
	return startChooserWith(ctx, log, cfg, startPortal)
}

// startChooserWith is startChooser with the portal started by start
// (tests make it fail).
func startChooserWith(ctx context.Context, log *slog.Logger, cfg config, start func(*slog.Logger, string, time.Duration, focuser) (*portal, error)) (*sessionBus, *portal, error) {
	bus, err := startSessionBus(ctx, os.Getuid())
	if err != nil {
		return nil, nil, fmt.Errorf("the session bus for the file chooser portal: %w", err)
	}
	p, err := start(log, bus.address(), cfg.ChooserTimeout, newXFocus(log, cfg.Width, cfg.Height))
	if err != nil {
		bus.stop()
		return nil, nil, fmt.Errorf("the file chooser portal: %w", err)
	}
	return bus, p, nil
}

// waitEnd waits until Chromium exits (false) or a stop is asked for
// (true). The bus exiting is logged: Chromium aborts once it notices,
// which ends the session the same way.
func waitEnd(ctx context.Context, log *slog.Logger, browser *process, bus *sessionBus) bool {
	busDone := bus.done()
	for {
		select {
		case <-browser.done:
			return false
		case <-ctx.Done():
			return true
		case <-busDone:
			log.Error("the session bus exited; Chromium aborts without it", "status", describe(bus.proc.status))
			busDone = nil
		}
	}
}

// busGone reports whether the bus has exited.
func busGone(b *sessionBus) bool {
	if b == nil {
		return false
	}
	select {
	case <-b.proc.done:
		return true
	default:
		return false
	}
}

// chromiumEnv is Chromium's environment: the entrypoint's own, the
// display, the location's, the bus's address, Mesa's settings (mesaEnv),
// and in Visual mode (visual) the accessibility's pointer to the same bus
// (a11yEnv, PROJ-150). The addresses go to Chromium only, never into the
// entrypoint's environment (base), which Xvfb, JWM, xdotool and GStreamer
// get.
func chromiumEnv(base, geo []string, bus *sessionBus, visual bool) []string {
	env := append(append([]string{}, base...), "DISPLAY="+display)
	env = append(append(append(env, geo...), bus.env()...), mesaEnv()...)
	if visual && bus != nil {
		env = append(env, a11yEnv(bus.address())...)
	}
	return env
}
