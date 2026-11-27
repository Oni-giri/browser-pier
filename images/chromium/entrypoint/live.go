package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	start         func() (*xdo, error)

	// runSync runs one xdotool command line in a process of its own, to
	// its end.
	runSync func(args ...string) error

	mu sync.Mutex
	x  *xdo // the running xdotool, nil until the next input starts one
	// held are the keys and buttons down, to release on hand-back, each
	// with the takeover that pressed it (a hand-back lets go only of its
	// own takeover's and earlier ones'): each stays until its release is
	// known to have run. released are those whose release has been
	// written to the running xdotool but may not have run yet; drain
	// moves them out once that xdotool has exited cleanly. lost are those
	// whose release went to an xdotool that died before it was known to
	// run it: the next xdotool gets it again before anything else, or
	// drain runs it in one of its own.
	held           map[string]uint64
	released, lost map[string]bool
	// fence refuses what a person sends once their takeover has ended
	// (fence.go); the clipboard box's side shares it.
	fence *takeoverFence
}

func newLiveInput(width, height int) *liveInput {
	return &liveInput{width: width, height: height, held: map[string]uint64{}, released: map[string]bool{}, lost: map[string]bool{}, start: startXdotool, fence: &takeoverFence{},
		runSync: func(args ...string) error {
			_, err := runCommand(context.Background(), "xdotool", args...)
			return err
		}}
}

// xdo is one running "xdotool -": its stdin, a channel closed once it has
// exited, whether it then had exited cleanly (having run all it was
// given), and a way to kill it.
type xdo struct {
	stdin  io.WriteCloser
	exited <-chan struct{}
	clean  func() bool // once exited is closed
	kill   func()
}

// writeWait bounds a write to xdotool's stdin: one that doesn't read it
// (stalled, its pipe full) is killed, so a write never holds the input,
// or the fence (fence.go), for longer. drainWait bounds how long drain
// waits for xdotool to run what it was given. Tests shorten them.
var (
	writeWait = 2 * time.Second
	drainWait = 2 * time.Second
)

func startXdotool() (*xdo, error) {
	cmd := exec.Command("xdotool", "-")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	status, err := procs.start(cmd)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	exited := make(chan struct{})
	var ws syscall.WaitStatus
	go func() { ws = <-status; close(exited) }()
	return &xdo{stdin: in, exited: exited, kill: func() { _ = cmd.Process.Kill() },
		clean: func() bool { return ws.Exited() && ws.ExitStatus() == 0 }}, nil
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

// send writes commands to xdotool, starting it if needed. li.mu must be
// held. They are queued, not yet run: drain waits for that. build makes
// them for the process they go to, so it runs again for a new one: after
// a write fails, the old one may not have run a release it was sent, so
// nothing counts as released any more, and build sees that. The releases
// a dead xdotool was sent (lost) go first to the next one.
func (li *liveInput) send(build func() []string) error {
	for attempt := 0; attempt < 2; attempt++ {
		if li.x == nil {
			// A new xdotool has run nothing yet. A release written to the
			// last one, if it died, may not have run.
			li.loseReleased()
			x, err := li.start()
			if err != nil {
				return err
			}
			li.x = x
		}
		x := li.x
		// The lost releases first, so this xdotool runs them before
		// anything else; marked released (for build), and lost again if
		// the write fails.
		_, lines := releaseLinesOf(li.held, func(h string, _ uint64) bool { return li.lost[h] })
		for h := range li.lost {
			if _, ok := li.held[h]; ok {
				li.released[h] = true
			}
		}
		clear(li.lost)
		lines = append(lines, build()...)
		// An xdotool that doesn't read its input within writeWait is
		// killed, which ends the write.
		watchdog := time.AfterFunc(writeWait, x.kill)
		var err error
		for _, l := range lines {
			if _, err = io.WriteString(x.stdin, l+"\n"); err != nil {
				break
			}
		}
		watchdog.Stop()
		if err == nil {
			return nil
		}
		// xdotool went away: start a new one and try again. A release
		// queued in the old one may not have run, so its key is lost
		// until a release that has (the next start).
		x.kill()
		_ = x.stdin.Close()
		li.x = nil
	}
	li.loseReleased()
	return errors.New("xdotool isn't taking input")
}

// loseReleased marks the releases written to an xdotool that is gone as
// lost: they may not have run. li.mu must be held.
func (li *liveInput) loseReleased() {
	for h := range li.released {
		if _, ok := li.held[h]; ok {
			li.lost[h] = true
		}
	}
	clear(li.released)
}

// drain returns once the running xdotool has run every command sent so
// far: it closes its input and waits for it to exit (drainWait; then it
// is killed, and whatever it hadn't run never runs). The next input
// starts a new one. li.mu must be held.
//
// Only if xdotool exited cleanly is a release written to it known to
// have run, and its key forgotten; with none running, nothing marked
// released can be shown to have run. Releases a dead xdotool was sent
// (lost) run here, in one of their own. If it didn't end cleanly,
// everything held is let go by a new, synchronous xdotool (runSync), and
// drain fails if that fails, the keys still held.
func (li *liveInput) drain() error {
	clean := len(li.released) == 0
	if x := li.x; x != nil {
		li.x = nil
		_ = x.stdin.Close()
		t := time.NewTimer(drainWait)
		select {
		case <-x.exited:
			clean = x.clean()
		case <-t.C:
			x.kill()
			<-x.exited
			clean = false
		}
		t.Stop()
	}
	if clean {
		for h := range li.released {
			delete(li.held, h)
		}
		clear(li.released)
		names, lines := releaseLinesOf(li.held, func(h string, _ uint64) bool { return li.lost[h] })
		if err := li.runReleases(lines); err != nil {
			return fmt.Errorf("couldn't let go of the live view's keys: %w", err)
		}
		for _, h := range names {
			delete(li.held, h)
		}
		clear(li.lost)
		return nil
	}
	li.loseReleased()
	names, lines := li.releaseLines(true, math.MaxUint64)
	if err := li.runReleases(lines); err != nil {
		return fmt.Errorf("couldn't let go of the live view's keys: %w", err)
	}
	for _, h := range names {
		delete(li.held, h)
	}
	clear(li.lost)
	return nil
}

// runReleases runs release lines, all in one synchronous xdotool.
func (li *liveInput) runReleases(lines []string) error {
	var args []string
	for _, l := range lines {
		args = append(args, strings.Fields(l)...)
	}
	if len(args) == 0 {
		return nil
	}
	return li.runSync(args...)
}

// releaseLines are the keys and buttons held by takeover upTo or an
// earlier one (all of them, or only those with no release queued yet),
// and the commands that let go of them, in a stable order.
func (li *liveInput) releaseLines(all bool, upTo uint64) (names, lines []string) {
	return releaseLinesOf(li.held, func(h string, gen uint64) bool { return gen <= upTo && (all || !li.released[h]) })
}

// releaseLinesOf are the keys and buttons of set that keep says to, and
// the commands that let go of them, sorted.
func releaseLinesOf(set map[string]uint64, keep func(h string, gen uint64) bool) (names, lines []string) {
	for h, gen := range set {
		if keep(h, gen) {
			names = append(names, h)
		}
	}
	sort.Strings(names)
	for _, h := range names {
		if h[0] == 'b' {
			lines = append(lines, "mouseup "+h[1:])
		} else {
			lines = append(lines, "keyup "+h[1:])
		}
	}
	return names, lines
}

func (li *liveInput) events(w http.ResponseWriter, r *http.Request) {
	gen, person, err := li.fence.takeover(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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
	// Right before the input goes to xdotool: a takeover that ended
	// while this waited sends nothing.
	done, ok := li.fence.admit(gen, person)
	if !ok {
		refuseFenced(w)
		return
	}
	defer done()
	li.mu.Lock()
	defer li.mu.Unlock()
	// A press counts as held even if the write fails part way (it may
	// have run); a release counts as queued only once written to the
	// running xdotool, and its key stays held until it is known to have
	// run (drain): until then the hand-back's lift (release) lets go of
	// it.
	err = li.send(func() []string { return all })
	written := err == nil
	for _, e := range evs {
		h := "k" + e.K
		if e.T == "down" || e.T == "up" {
			h = "b" + strconv.Itoa(e.B)
		}
		switch e.T {
		case "down", "keydown":
			li.held[h] = gen
			if written {
				delete(li.released, h)
				delete(li.lost, h)
			}
		case "up", "keyup":
			if _, ok := li.held[h]; ok && written {
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

// release lets go of the keys and buttons still down, so a viewer who
// disconnects mid-drag or mid-shortcut leaves nothing stuck. They are
// forgotten only once their releases are known to have run: until then
// they are still held here, so the orchestrator's next try sends them
// again (letting go of one already up is harmless).
//
// The orchestrator's lift as takeover N ends names N in fenceHeader. In
// one step, under the fence's write lock (so once the persons' actions
// under way are over), it fences N: nothing from N or earlier acts from
// then on. Then the live xdotool is drained, so whatever input it had
// queued has run (or, if it had to be killed, everything held is let
// go), the keys and buttons N or an earlier takeover still holds are let
// go through a synchronous run of their own (runSync), and only then does
// it answer 200 with the fence; if any of that fails, it answers 500,
// the keys still held. A lift for N held up past the start of a later
// takeover (fence.go's stale) still fences N, which costs the later one
// nothing, but touches neither its input nor its keys.
//
// The person's own lift (their page losing focus) names their takeover
// instead, is refused once it has ended, and lets go of their keys.
func (li *liveInput) release(w http.ResponseWriter, r *http.Request) {
	gen, person, err := li.fence.takeover(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n, fenced, err := fenceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if fenced {
		fence, err := li.fence.cleanup(n, func(stale bool) error {
			if stale {
				return nil
			}
			li.mu.Lock()
			defer li.mu.Unlock()
			if err := li.drain(); err != nil {
				return err
			}
			names, lines := li.releaseLines(true, n)
			if err := li.runReleases(lines); err != nil {
				return fmt.Errorf("couldn't let go of the live view's keys: %w", err)
			}
			for _, h := range names {
				delete(li.held, h)
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		answerFence(w, fence)
		return
	}
	done, ok := li.fence.admit(gen, person)
	if !ok {
		refuseFenced(w)
		return
	}
	defer done()
	li.mu.Lock()
	defer li.mu.Unlock()
	// What already has a release queued in the running xdotool needs
	// none; everything stays held until its release is known to have run
	// (drain), and counts as queued only once written. The lines are made
	// for the process they go to: a new one gets a release for everything
	// held. With nothing new to send but releases still queued, a no-op
	// checks the xdotool that has them is still there; if it isn't, its
	// releases go to a new one.
	if names, _ := li.releaseLines(true, gen); len(names) > 0 {
		var sent []string
		err := li.send(func() []string {
			var lines []string
			sent, lines = li.releaseLines(false, gen)
			if len(lines) == 0 {
				lines = []string{"sleep 0"}
			}
			return lines
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, h := range sent {
			li.released[h] = true
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
