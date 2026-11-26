package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// liveInput feeds a person's mouse and keyboard from the live view to the
// X display, through one long-running "xdotool -" that reads commands on
// stdin as they come. Events are structured and every command line is
// built here from checked numbers and key names: nothing a viewer sends
// becomes text in an xdotool script (which can run programs).
type liveInput struct {
	width, height int
	start         func() (ioWriteCloser, error)

	// runSync runs one xdotool command line in a process of its own, to
	// its end.
	runSync func(args ...string) error

	mu    sync.Mutex
	stdin io.WriteCloser
	// held is keys and buttons down, to release on hand-back: each stays
	// until its release is known to have run. released is those whose
	// release has been written to the running xdotool but may not have
	// run yet; drain moves them out once that xdotool has exited cleanly.
	held, released map[string]bool
}

func newLiveInput(width, height int) *liveInput {
	return &liveInput{width: width, height: height, held: map[string]bool{}, released: map[string]bool{}, start: startXdotool,
		runSync: func(args ...string) error {
			_, err := runCommand(context.Background(), "xdotool", args...)
			return err
		}}
}

// ioWriteCloser is xdotool's stdin.
type ioWriteCloser = io.WriteCloser

func startXdotool() (ioWriteCloser, error) {
	cmd := exec.Command("xdotool", "-")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	stderr, err := withheldStderr("xdotool")
	if err != nil {
		return nil, err
	}
	defer func() { _ = stderr.Close() }()
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	exited, err := procs.start(cmd)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	x := &xdotoolStdin{WriteCloser: in, exited: make(chan struct{}), proc: cmd.Process}
	go func() { x.status = <-exited; close(x.exited) }()
	return x, nil
}

// xdotoolStdin is the live xdotool's input. Closing it waits until
// xdotool has run every command written and exited (killing it after
// 2 s); it fails unless xdotool exited cleanly, so drain can tell
// whether everything written has run.
type xdotoolStdin struct {
	io.WriteCloser
	exited chan struct{}
	status syscall.WaitStatus // once exited is closed
	proc   *os.Process
}

func (x *xdotoolStdin) Close() error {
	err := x.WriteCloser.Close()
	select {
	case <-x.exited:
	case <-time.After(2 * time.Second):
		_ = x.proc.Kill()
		<-x.exited
		return errors.New("xdotool was killed with commands still queued")
	}
	if !x.status.Exited() || x.status.ExitStatus() != 0 {
		return fmt.Errorf("xdotool: %s", describe(x.status))
	}
	return err
}

// drain returns once xdotool has run every command sent so far, with
// li.mu held: it ends the xdotool process, and the next event starts a
// new one. Typing with a binding (typeMapped) drains first, so a queued
// key of the person's can't bind a keycode of its own meanwhile. If
// xdotool didn't exit cleanly, a release it was sent may not have run,
// so everything held is let go by a new xdotool first; drain fails if
// that fails.
func (li *liveInput) drain() error {
	// With no xdotool running, nothing should be marked released; if
	// something is, its release can't be shown to have run.
	clean := len(li.released) == 0
	if li.stdin != nil {
		clean = li.stdin.Close() == nil
		li.stdin = nil
	}
	if clean {
		for h := range li.released {
			delete(li.held, h)
		}
		clear(li.released)
		return nil
	}
	var args []string
	for _, l := range li.releaseLines(true) {
		args = append(args, strings.Fields(l)...)
	}
	if len(args) > 0 {
		if err := li.runSync(args...); err != nil {
			return fmt.Errorf("couldn't let go of the live view's keys: %w", err)
		}
	}
	clear(li.held)
	clear(li.released)
	return nil
}

// releaseLines is the commands that let go of what's held, sorted: all
// of it, or only what has no release queued yet.
func (li *liveInput) releaseLines(all bool) []string {
	names := make([]string, 0, len(li.held))
	for h := range li.held {
		if all || !li.released[h] {
			names = append(names, h)
		}
	}
	sort.Strings(names)
	var lines []string
	for _, h := range names {
		if h[0] == 'b' {
			lines = append(lines, "mouseup "+h[1:])
		} else {
			lines = append(lines, "keyup "+h[1:])
		}
	}
	return lines
}

// event is one input from a viewer.
type event struct {
	T  string `json:"t"` // move, down, up, wheel, keydown, keyup
	X  int    `json:"x"`
	Y  int    `json:"y"`
	B  int    `json:"b"`  // mouse button, 1 to 3
	DX int    `json:"dx"` // wheel steps
	DY int    `json:"dy"`
	K  string `json:"k"` // X keysym name: a, Return, Shift_L, U00E9…
}

var keysymRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// lines turns an event into xdotool commands, or refuses it.
func (li *liveInput) lines(e event) ([]string, error) {
	switch e.T {
	case "move":
		if e.X < 0 || e.Y < 0 || e.X >= li.width || e.Y >= li.height {
			return nil, fmt.Errorf("(%d, %d) is outside the screen", e.X, e.Y)
		}
		return []string{fmt.Sprintf("mousemove %d %d", e.X, e.Y)}, nil
	case "down", "up":
		if e.B < 1 || e.B > 3 {
			return nil, fmt.Errorf("button %d: want 1 to 3", e.B)
		}
		return []string{"mouse" + e.T + " " + strconv.Itoa(e.B)}, nil
	case "wheel":
		var out []string
		steps := func(n, neg, pos int) {
			b := pos
			if n < 0 {
				b, n = neg, -n
			}
			for range min(n, 10) {
				out = append(out, "click "+strconv.Itoa(b))
			}
		}
		steps(e.DY, 4, 5) // up, down
		steps(e.DX, 6, 7) // left, right
		return out, nil
	case "keydown", "keyup":
		if !keysymRE.MatchString(e.K) || xdotoolCommand(e.K) {
			return nil, fmt.Errorf("key %q: want an X keysym name", e.K)
		}
		return []string{e.T + " " + e.K}, nil
	}
	return nil, fmt.Errorf("event %q: want move, down, up, wheel, keydown or keyup", e.T)
}

// send writes commands to xdotool, starting it if needed. build makes
// them for the process they go to, so it runs again for a new one: after
// a write fails, the old one may not have run a release it was sent, so
// nothing counts as released any more, and build sees that.
func (li *liveInput) send(build func() []string) error {
	for attempt := 0; attempt < 2; attempt++ {
		if li.stdin == nil {
			// A new xdotool has run nothing yet.
			clear(li.released)
			in, err := li.start()
			if err != nil {
				return err
			}
			li.stdin = in
		}
		var err error
		for _, l := range build() {
			if _, err = io.WriteString(li.stdin, l+"\n"); err != nil {
				break
			}
		}
		if err == nil {
			return nil
		}
		// xdotool went away: start a new one and try again. A release
		// queued in the old one may not have run, so its key is held
		// again until a release that has.
		_ = li.stdin.Close()
		li.stdin = nil
		clear(li.released)
	}
	return errors.New("xdotool isn't taking input")
}

func (li *liveInput) events(w http.ResponseWriter, r *http.Request) {
	var evs []event
	if !decode(w, r, &evs) {
		return
	}
	if len(evs) > 256 {
		http.Error(w, "at most 256 events at a time", http.StatusBadRequest)
		return
	}
	var all []string
	for _, e := range evs {
		ls, err := li.lines(e)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		all = append(all, ls...)
	}
	li.mu.Lock()
	defer li.mu.Unlock()
	// A press counts as held even if the write fails part way (it may
	// have run); a release counts as queued only once written, and its
	// key stays held until it is known to have run (drain).
	err := li.send(func() []string { return all })
	written := err == nil
	for _, e := range evs {
		h := "k" + e.K
		if e.T == "down" || e.T == "up" {
			h = "b" + strconv.Itoa(e.B)
		}
		switch e.T {
		case "down", "keydown":
			li.held[h] = true
			if written {
				delete(li.released, h)
			}
		case "up", "keyup":
			if written && li.held[h] {
				li.released[h] = true
			}
		}
	}
	if !written {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// release lets go of every key and button still down, so a viewer who
// disconnects mid-drag or mid-shortcut leaves nothing stuck.
func (li *liveInput) release(w http.ResponseWriter, _ *http.Request) {
	li.mu.Lock()
	defer li.mu.Unlock()
	// What already has a release queued in the running xdotool needs
	// none; everything stays held until its release is known to have run
	// (drain), and counts as queued only once written. The lines are made
	// for the process they go to: a new one gets a release for everything
	// held. With nothing new to send but releases still queued, a no-op
	// checks the xdotool that has them is still there; if it isn't, its
	// releases go to a new one.
	if len(li.held) > 0 {
		err := li.send(func() []string {
			lines := li.releaseLines(false)
			if len(lines) == 0 {
				lines = []string{"sleep 0"}
			}
			return lines
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for h := range li.held {
			li.released[h] = true
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
