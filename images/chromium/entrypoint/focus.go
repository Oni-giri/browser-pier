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

// xFocus moves the focus with xdotool. Its moves run one at a time, in
// order, off the caller's goroutine: a D-Bus call never waits for X.
type xFocus struct {
	log           *slog.Logger
	width, height int
	output        func(ctx context.Context, name string, args ...string) ([]byte, error)
	queue         chan bool // true: away, false: back

	mu     sync.Mutex
	window string // the browser's window, while the focus is away from it
}

func newXFocus(log *slog.Logger, width, height int) *xFocus {
	f := &xFocus{log: log, width: width, height: height, output: runStdout, queue: make(chan bool, 64)}
	go func() {
		for away := range f.queue {
			if away {
				f.moveAway()
			} else {
				f.moveBack()
			}
		}
	}()
	return f
}

func (f *xFocus) away() { f.send(true) }
func (f *xFocus) back() { f.send(false) }

func (f *xFocus) send(away bool) {
	select {
	case f.queue <- away:
	default: // a stuck X server: never block the bus
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
	if _, err := f.run("windowactivate", w); err != nil {
		f.log.Warn("couldn't give the focus back after a file chooser", "err", err)
	}
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
