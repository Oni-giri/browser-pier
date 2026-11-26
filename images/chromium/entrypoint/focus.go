package main

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// focuser moves the X input focus while a file chooser waits (PROJ-152).
// A desktop's file dialog is a window of its own, which takes the focus
// from the browser's while it is open: the page sees its window blur as
// the chooser opens and focus again once it is answered, as it does with
// Chromium's own GTK dialog. The portal shows no window, so the focus
// goes to the desktop's taskbar instead, and comes back to the browser's
// window when the last chooser ends.
type focuser interface {
	away()
	back()
}

// noFocus moves nothing (tests, and a portal with no display).
type noFocus struct{}

func (noFocus) away() {}
func (noFocus) back() {}

// xFocus moves the focus with xdotool, off the caller's goroutine: a
// D-Bus call never waits for X. Calls set the state wanted (away or
// back), and one worker brings X to it: a burst of calls coalesces into
// the last, and none is ever dropped, so the focus always comes back.
type xFocus struct {
	log           *slog.Logger
	width, height int
	output        func(ctx context.Context, name string, args ...string) ([]byte, error)
	wake          chan struct{} // capacity 1: the wanted state changed

	mu     sync.Mutex
	want   bool   // away from the browser's window
	window string // the browser's window, while the focus is away from it
}

func newXFocus(log *slog.Logger, width, height int) *xFocus {
	f := &xFocus{log: log, width: width, height: height, output: runStdout, wake: make(chan struct{}, 1)}
	go f.work()
	return f
}

func (f *xFocus) away() { f.set(true) }
func (f *xFocus) back() { f.set(false) }

func (f *xFocus) set(away bool) {
	f.mu.Lock()
	f.want = away
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default: // already woken: it reads the latest state
	}
}

func (f *xFocus) work() {
	for range f.wake {
		f.mu.Lock()
		want, moved := f.want, f.window != ""
		f.mu.Unlock()
		switch {
		case want && !moved:
			f.moveAway()
		case !want && moved:
			f.moveBack()
		}
	}
}

func (f *xFocus) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := f.output(ctx, "xdotool", args...)
	return strings.TrimSpace(string(out)), err
}

// moveAway gives the focus to the taskbar, if the browser's window has it.
func (f *xFocus) moveAway() {
	focused, err := f.run("getwindowfocus")
	if err != nil || focused == "" {
		return
	}
	name, err := f.run("getwindowname", focused)
	if err != nil || !strings.HasSuffix(name, "Chromium") {
		return // something else has the focus: leave it there
	}
	tray := f.taskbar()
	if tray == "" {
		f.log.Warn("no taskbar to give the focus to while a file chooser waits")
		return
	}
	if _, err := f.run("windowfocus", tray); err != nil {
		f.log.Warn("couldn't move the focus while a file chooser waits", "err", err)
		return
	}
	f.mu.Lock()
	f.window = focused
	f.mu.Unlock()
}

// moveBack gives the focus back to the browser's window, if it was taken.
func (f *xFocus) moveBack() {
	f.mu.Lock()
	w := f.window
	f.window = ""
	f.mu.Unlock()
	if w == "" {
		return
	}
	if _, err := f.run("windowactivate", w); err == nil {
		return
	}
	// The window id went stale, or xdotool stalled: find the browser's
	// window afresh, and try again once.
	for try := range 2 {
		ids, err := f.run("search", "--onlyvisible", "--name", "Chromium$")
		if err == nil {
			if fs := strings.Fields(ids); len(fs) > 0 {
				if _, err := f.run("windowactivate", fs[0]); err == nil {
					return
				}
			}
		}
		if try == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	f.log.Warn("couldn't give the focus back to the browser after a file chooser; a click on its window does")
}

// taskbar finds JWM's tray: the window across the bottom of the screen,
// panelHeight tall (jwmrc).
func (f *xFocus) taskbar() string {
	ids, err := f.run("search", "--onlyvisible", "--classname", "")
	if err != nil {
		return ""
	}
	for _, id := range strings.Fields(ids) {
		geo, err := f.run("getwindowgeometry", "--shell", id)
		if err != nil {
			continue
		}
		g := map[string]int{}
		for _, line := range strings.Split(geo, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				g[k], _ = strconv.Atoi(v)
			}
		}
		if g["X"] == 0 && g["Y"] == f.height-panelHeight && g["WIDTH"] == f.width && g["HEIGHT"] == panelHeight {
			return id
		}
	}
	return ""
}
