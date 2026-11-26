package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"sync"
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

	mu sync.Mutex
	x  *xdo // the running xdotool, nil until the next input starts one
	// held are the keys and buttons down, to release on hand-back, each
	// with the takeover that pressed it: a hand-back lets go only of its
	// own takeover's and earlier ones'.
	held map[string]uint64
	// fence refuses what a person sends once their takeover has ended
	// (fence.go); the clipboard box's side shares it.
	fence *takeoverFence
}

func newLiveInput(width, height int) *liveInput {
	return &liveInput{width: width, height: height, held: map[string]uint64{}, start: startXdotool, fence: &takeoverFence{}}
}

// xdo is one running "xdotool -": its stdin, a channel closed once it has
// exited, and a way to kill it.
type xdo struct {
	stdin  io.WriteCloser
	exited <-chan struct{}
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
		return nil, err
	}
	exited := make(chan struct{})
	go func() { <-status; close(exited) }()
	return &xdo{stdin: in, exited: exited, kill: func() { _ = cmd.Process.Kill() }}, nil
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
// held. They are queued, not yet run: drain waits for that.
func (li *liveInput) send(lines []string) error {
	for attempt := 0; attempt < 2; attempt++ {
		if li.x == nil {
			x, err := li.start()
			if err != nil {
				return err
			}
			li.x = x
		}
		x := li.x
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
		// xdotool went away: start a new one and try again.
		x.kill()
		_ = x.stdin.Close()
		li.x = nil
	}
	return errors.New("xdotool isn't taking input")
}

// drain closes the running xdotool's input and waits for it to run what
// it was given and exit (drainWait; then it is killed, and whatever it
// hadn't run never runs). The next input starts a new one. li.mu must be
// held.
func (li *liveInput) drain() {
	x := li.x
	if x == nil {
		return
	}
	li.x = nil
	_ = x.stdin.Close()
	t := time.NewTimer(drainWait)
	defer t.Stop()
	select {
	case <-x.exited:
		return
	case <-t.C:
	}
	x.kill()
	<-x.exited
}

// releases are the commands that let go of the keys and buttons held by
// takeover upTo or an earlier one, in a stable order.
func (li *liveInput) releases(upTo uint64) (names, lines []string) {
	for h, gen := range li.held {
		if gen <= upTo {
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
	// A key or button counts as held from before its press is sent (a
	// failed send may have pressed it), and stops counting only once its
	// release has been sent: a release that fails leaves it for the
	// hand-back's lift (release) to let go of.
	held := maps.Clone(li.held)
	for _, e := range evs {
		switch e.T {
		case "down":
			li.held["b"+strconv.Itoa(e.B)] = gen
			held["b"+strconv.Itoa(e.B)] = gen
		case "up":
			delete(held, "b"+strconv.Itoa(e.B))
		case "keydown":
			li.held["k"+e.K] = gen
			held["k"+e.K] = gen
		case "keyup":
			delete(held, "k"+e.K)
		}
	}
	if err := li.send(all); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	li.held = held
	w.WriteHeader(http.StatusNoContent)
}

// release lets go of the keys and buttons still down, so a viewer who
// disconnects mid-drag or mid-shortcut leaves nothing stuck. They are
// forgotten only once xdotool has taken the releases: if it fails, they
// are still held here, so the orchestrator's next try sends them again
// (letting go of one already up is harmless).
//
// The orchestrator's lift as takeover N ends names N in fenceHeader. In
// one step, under the fence's write lock (so once the persons' actions
// under way are over), it fences N: nothing from N or earlier acts from
// then on. Then the live xdotool is drained, so whatever input it had
// queued has run, the keys and buttons N or an earlier takeover holds
// are let go through a fresh one, drained too, and only then does it
// answer 200 with the fence. A lift for N held up past the start of a
// later takeover (fence.go's stale) still fences N, which costs the later
// one nothing, but touches neither its input nor its keys.
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
			li.drain()
			names, lines := li.releases(n)
			if len(lines) == 0 {
				return nil
			}
			if err := li.send(lines); err != nil {
				return err
			}
			li.drain()
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
	names, lines := li.releases(gen)
	if len(lines) > 0 {
		if err := li.send(lines); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for _, h := range names {
		delete(li.held, h)
	}
	w.WriteHeader(http.StatusNoContent)
}
