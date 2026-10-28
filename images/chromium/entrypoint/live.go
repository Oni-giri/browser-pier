package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// liveInput feeds a person's mouse and keyboard from the live view to the
// X display, through one long-running "xdotool -" that reads commands on
// stdin as they come. Events are structured and every command line is
// built here from checked numbers and key names: nothing a viewer sends
// becomes text in an xdotool script (which can run programs).
type liveInput struct {
	width, height int
	start         func() (ioWriteCloser, error)

	mu    sync.Mutex
	stdin io.WriteCloser
	held  map[string]bool // keys and buttons down, to release on hand-back
}

func newLiveInput(width, height int) *liveInput {
	return &liveInput{width: width, height: height, held: map[string]bool{}, start: startXdotool}
}

// ioWriteCloser is xdotool's stdin.
type ioWriteCloser = io.WriteCloser

func startXdotool() (ioWriteCloser, error) {
	cmd := exec.Command("xdotool", "-")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	exited, err := procs.start(cmd)
	if err != nil {
		return nil, err
	}
	go func() { <-exited }()
	return in, nil
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

// send writes commands to xdotool, starting it if needed.
func (li *liveInput) send(lines []string) error {
	for attempt := 0; attempt < 2; attempt++ {
		if li.stdin == nil {
			in, err := li.start()
			if err != nil {
				return err
			}
			li.stdin = in
		}
		var err error
		for _, l := range lines {
			if _, err = io.WriteString(li.stdin, l+"\n"); err != nil {
				break
			}
		}
		if err == nil {
			return nil
		}
		// xdotool went away: start a new one and try again.
		_ = li.stdin.Close()
		li.stdin = nil
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
	for _, e := range evs {
		switch e.T {
		case "down":
			li.held["b"+strconv.Itoa(e.B)] = true
		case "up":
			delete(li.held, "b"+strconv.Itoa(e.B))
		case "keydown":
			li.held["k"+e.K] = true
		case "keyup":
			delete(li.held, "k"+e.K)
		}
	}
	if err := li.send(all); err != nil {
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
	names := make([]string, 0, len(li.held))
	for h := range li.held {
		names = append(names, h)
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
	li.held = map[string]bool{}
	if len(lines) > 0 {
		if err := li.send(lines); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
