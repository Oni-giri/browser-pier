package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	// desktopConfig is JWM's configuration, from images/chromium/jwmrc.
	desktopConfig = "/etc/browserpier/jwmrc"
	// desktopReady is the file jwmrc's startup command creates once the
	// window manager and its taskbar are up.
	desktopReady = "/tmp/.browserpier-desktop-ready"
)

// windowManager is the command for JWM and its taskbar. env is the
// location's, so the taskbar's clock shows the same time zone as pages.
func windowManager(env []string) *exec.Cmd {
	cmd := exec.Command("jwm", "-display", display, "-f", desktopConfig)
	cmd.Env = append(append(os.Environ(), "DISPLAY="+display), env...)
	return cmd
}

// startDesktop starts the window manager and waits until it creates ready,
// so Chromium's window opens on a desktop like a person's: a taskbar that
// reserves the bottom of the screen, which pages see as screen.availHeight.
// Without one, Xvfb is a bare screen, as headless is.
func startDesktop(ctx context.Context, wm *exec.Cmd, ready string, timeout time.Duration) (*process, error) {
	if err := os.Remove(ready); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("window manager: %w", err)
	}
	p, err := spawn(wm)
	if err != nil {
		return nil, fmt.Errorf("start the window manager: %w", err)
	}
	if err := waitFor(ctx, p, timeout, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}); err != nil {
		p.stop(syscall.SIGTERM, 5*time.Second)
		return nil, fmt.Errorf("window manager: %w", err)
	}
	return p, nil
}

// watchDesktop logs a warning if the window manager exits before stopping
// is closed. The browser carries on without it: input, screenshots and a
// graceful stop don't need it, only the taskbar goes. The returned channel
// is closed once the watch is over.
func watchDesktop(log *slog.Logger, wm *process, stopping <-chan struct{}) <-chan struct{} {
	over := make(chan struct{})
	go func() {
		defer close(over)
		select {
		case <-wm.done:
			select {
			case <-stopping: // stopped on purpose
			default:
				log.Warn("the window manager exited; the browser carries on without a taskbar", "status", describe(wm.status))
			}
		case <-stopping:
		}
	}()
	return over
}
