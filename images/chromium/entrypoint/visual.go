package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// visual serves Visual mode: screenshots of the X display and OS-level
// input through xdotool. Nothing here touches CDP, so a page can't tell
// an agent from a person by a debugger being attached.
type visual struct {
	width, height int
	// run runs a command, returning what it wrote to fd 3.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	mu  sync.Mutex // one input action at a time, so two never interleave
	// live is the live view's continuous mouse and keyboard.
	live *liveInput
}

const maxTypeLen = 2000

func newVisual(width, height int) *visual {
	return &visual{width: width, height: height, run: runCommand, live: newLiveInput(width, height)}
}

func (v *visual) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /screenshot", v.screenshot)
	mux.HandleFunc("POST /input/click", v.click)
	mux.HandleFunc("POST /input/type", v.typeText)
	mux.HandleFunc("POST /input/scroll", v.scroll)
	mux.HandleFunc("POST /input/key", v.key)
	mux.HandleFunc("POST /input/drag", v.drag)
	if v.live != nil {
		mux.HandleFunc("POST /input/events", v.live.events)
		mux.HandleFunc("POST /input/release", v.live.release)
	}
}

// screenshotArgs captures one frame as PNG to fd 3, scaled to width (and
// the matching height) when width is smaller than the screen.
func (v *visual) screenshotArgs(width int) []string {
	args := []string{"-q", "ximagesrc", "display-name=" + display, "num-buffers=1", "use-damage=false", "show-pointer=false", "!", "videoconvert", "!"}
	if width > 0 && width < v.width {
		height := v.height * width / v.width
		args = append(args, "videoscale", "!", fmt.Sprintf("video/x-raw,width=%d,height=%d", width, height), "!")
	}
	return append(args, "pngenc", "!", "fdsink", "fd=3")
}

func (v *visual) screenshot(w http.ResponseWriter, r *http.Request) {
	width := 0
	if s := r.URL.Query().Get("width"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 16 {
			http.Error(w, "width: want a number of pixels, at least 16", http.StatusBadRequest)
			return
		}
		width = n
	}
	png, err := v.run(r.Context(), "gst-launch-1.0", v.screenshotArgs(width)...)
	if err != nil {
		http.Error(w, "screenshot failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Screen-Width", strconv.Itoa(v.width))
	w.Header().Set("X-Screen-Height", strconv.Itoa(v.height))
	_, _ = w.Write(png)
}

type clickRequest struct {
	X, Y   int
	Button string // left (default), middle or right
	Double bool
}

var buttons = map[string]string{"": "1", "left": "1", "middle": "2", "right": "3"}

// onScreen refuses a point outside the screen.
func (v *visual) onScreen(x, y int) error {
	if x < 0 || y < 0 || x >= v.width || y >= v.height {
		return fmt.Errorf("(%d, %d) is outside the %dx%d screen", x, y, v.width, v.height)
	}
	return nil
}

func (v *visual) clickArgs(c clickRequest) ([]string, error) {
	if err := v.onScreen(c.X, c.Y); err != nil {
		return nil, err
	}
	b, ok := buttons[c.Button]
	if !ok {
		return nil, fmt.Errorf("button %q: want left, middle or right", c.Button)
	}
	// No --sync on the moves here: the X server takes events in order
	// anyway, and --sync to where the pointer already is stalls 15 s.
	args := []string{"mousemove", strconv.Itoa(c.X), strconv.Itoa(c.Y), "click"}
	if c.Double {
		args = append(args, "--repeat", "2", "--delay", "80")
	}
	return append(args, b), nil
}

func (v *visual) click(w http.ResponseWriter, r *http.Request) {
	var c clickRequest
	if !decode(w, r, &c) {
		return
	}
	args, err := v.clickArgs(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.input(w, r, args)
}

type typeRequest struct{ Text string }

func typeArgs(text string) ([]string, error) {
	if text == "" || !utf8.ValidString(text) {
		return nil, fmt.Errorf("text: want non-empty UTF-8")
	}
	if n := utf8.RuneCountInString(text); n > maxTypeLen {
		return nil, fmt.Errorf("text: %d characters, at most %d per call", n, maxTypeLen)
	}
	// "--" so text starting with a dash is typed, not read as an option.
	return []string{"type", "--clearmodifiers", "--delay", "12", "--", text}, nil
}

func (v *visual) typeText(w http.ResponseWriter, r *http.Request) {
	var t typeRequest
	if !decode(w, r, &t) {
		return
	}
	args, err := typeArgs(t.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.input(w, r, args)
}

type scrollRequest struct {
	X, Y   int
	DX, DY int // notches: positive scrolls right and down
}

// maxScroll caps the notches of one scroll on each axis.
const maxScroll = 20

func (v *visual) scrollArgs(s scrollRequest) ([]string, error) {
	if err := v.onScreen(s.X, s.Y); err != nil {
		return nil, err
	}
	if s.DX == 0 && s.DY == 0 {
		return nil, errors.New("dx, dy: want a number of notches to scroll")
	}
	args := []string{"mousemove", strconv.Itoa(s.X), strconv.Itoa(s.Y)}
	steps := func(n int, neg, pos string) {
		b := pos
		if n < 0 {
			b, n = neg, -n
		}
		if n > 0 {
			args = append(args, "click", "--repeat", strconv.Itoa(min(n, maxScroll)), "--delay", "30", b)
		}
	}
	steps(s.DY, "4", "5") // up, down
	steps(s.DX, "6", "7") // left, right
	return args, nil
}

func (v *visual) scroll(w http.ResponseWriter, r *http.Request) {
	var s scrollRequest
	if !decode(w, r, &s) {
		return
	}
	args, err := v.scrollArgs(s)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.input(w, r, args)
}

// combos is one key combination ("ctrl+shift+t") or a list of them.
type combos []string

func (c *combos) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*c = combos{one}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("keys: want a key combination or a list of them")
	}
	*c = list
	return nil
}

type keyRequest struct{ Keys combos }

const (
	maxCombos    = 10
	maxComboKeys = 5
)

// keyAliases maps friendly key names, lower-cased, to X keysym names.
var keyAliases = map[string]string{
	"ctrl": "ctrl", "control": "ctrl", "alt": "alt", "option": "alt", "shift": "shift",
	"cmd": "super", "command": "super", "meta": "super", "super": "super", "win": "super",
	"enter": "Return", "return": "Return", "esc": "Escape", "escape": "Escape",
	"tab": "Tab", "space": "space", "backspace": "BackSpace", "delete": "Delete", "del": "Delete",
	"insert": "Insert", "home": "Home", "end": "End",
	"pageup": "Prior", "pgup": "Prior", "pagedown": "Next", "pgdn": "Next",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"arrowup": "Up", "arrowdown": "Down", "arrowleft": "Left", "arrowright": "Right",
}

// keyArgs presses each combination in turn. Every key must be an X keysym
// name (or an alias for one), so nothing else reaches xdotool.
// xdotoolCommand reports whether a word is one of xdotool's commands.
// xdotool key stops reading keys at the first argument that names a
// command (in any case, even after --) and runs the rest as that command:
// "Return exec touch x" would run a program. So no key may be one.
func xdotoolCommand(word string) bool {
	w := strings.ToLower(word)
	if xdotoolCommands[w] {
		return true
	}
	// Prefixes of whole families, for commands a newer xdotool may add.
	for _, p := range []string{"window", "get", "set_", "behave", "mouse"} {
		if strings.HasPrefix(w, p) {
			return true
		}
	}
	return false
}

// xdotoolCommands are `xdotool help`'s commands (3.x), plus a few from
// other versions.
var xdotoolCommands = map[string]bool{}

func init() {
	for _, c := range strings.Fields(`behave behave_screen_edge click exec get_desktop get_desktop_for_window
		get_desktop_viewport get_num_desktops getactivewindow getdisplaygeometry getmouselocation getwindowfocus
		getwindowgeometry getwindowname getwindowpid getwindowclassname help key keydown keyup mousedown mousemove
		mousemove_relative mouseup search selectwindow set_desktop set_desktop_for_window set_desktop_viewport
		set_num_desktops set_window sleep type version windowactivate windowclose windowfocus windowkill windowmap
		windowminimize windowmove windowraise windowreparent windowsize windowunmap windowstate windowquit
		windowlower windowunminimize`) {
		xdotoolCommands[c] = true
	}
}

func keyArgs(keys combos) ([]string, error) {
	if len(keys) == 0 || len(keys) > maxCombos {
		return nil, fmt.Errorf("keys: want 1 to %d key combinations", maxCombos)
	}
	args := []string{"key", "--clearmodifiers", "--delay", "50", "--"}
	for _, c := range keys {
		parts := strings.Split(c, "+")
		if len(parts) > maxComboKeys {
			return nil, fmt.Errorf("key %q: at most %d keys at once", c, maxComboKeys)
		}
		for i, k := range parts {
			k = strings.TrimSpace(k)
			if a, ok := keyAliases[strings.ToLower(k)]; ok {
				k = a
			}
			if !keysymRE.MatchString(k) {
				return nil, fmt.Errorf("key %q: want X keysym names joined by +, like ctrl+l or Return", c)
			}
			if xdotoolCommand(k) {
				return nil, fmt.Errorf("key %q: %q isn't a key", c, k)
			}
			parts[i] = k
		}
		args = append(args, strings.Join(parts, "+"))
	}
	return args, nil
}

func (v *visual) key(w http.ResponseWriter, r *http.Request) {
	var k keyRequest
	if !decode(w, r, &k) {
		return
	}
	args, err := keyArgs(k.Keys)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.input(w, r, args)
}

type point struct{ X, Y int }

type dragRequest struct {
	From, To point
	Button   string // left (default), middle or right
}

// dragSteps is how many moves a drag makes between press and release, so
// pages that track the pointer (sliders, drag and drop) see it travel.
const dragSteps = 8

func (v *visual) dragArgs(d dragRequest) ([]string, error) {
	if err := v.onScreen(d.From.X, d.From.Y); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if err := v.onScreen(d.To.X, d.To.Y); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	b, ok := buttons[d.Button]
	if !ok {
		return nil, fmt.Errorf("button %q: want left, middle or right", d.Button)
	}
	move := func(x, y int) []string { return []string{"mousemove", strconv.Itoa(x), strconv.Itoa(y)} }
	args := append(move(d.From.X, d.From.Y), "sleep", "0.05", "mousedown", b)
	for i := 1; i <= dragSteps; i++ {
		x := d.From.X + (d.To.X-d.From.X)*i/dragSteps
		y := d.From.Y + (d.To.Y-d.From.Y)*i/dragSteps
		args = append(append(args, "sleep", "0.02"), move(x, y)...)
	}
	return append(args, "sleep", "0.05", "mouseup", b), nil
}

func (v *visual) drag(w http.ResponseWriter, r *http.Request) {
	var d dragRequest
	if !decode(w, r, &d) {
		return
	}
	args, err := v.dragArgs(d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.run(r.Context(), "xdotool", args...); err != nil {
		// Don't leave the button held if the drag broke off half way.
		_, _ = v.run(context.WithoutCancel(r.Context()), "xdotool", "mouseup", buttons[d.Button])
		http.Error(w, "input failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (v *visual) input(w http.ResponseWriter, r *http.Request, args []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.run(r.Context(), "xdotool", args...); err != nil {
		http.Error(w, "input failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// runCommand runs a command through the reaper, killing it after 30 s or
// when ctx ends, and returns what it wrote to fd 3.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = pr.Close() }()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.ExtraFiles = []*os.File{pw}
	cmd.Stderr = os.Stderr
	exited, err := procs.start(cmd)
	_ = pw.Close()
	if err != nil {
		return nil, err
	}
	defer context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })()
	out, readErr := io.ReadAll(io.LimitReader(pr, maxOutput+1))
	if len(out) > maxOutput {
		// Stop reading and kill it; otherwise it would block on a full pipe.
		_ = pr.Close()
		_ = cmd.Process.Kill()
		<-exited
		return nil, fmt.Errorf("%s: output over %d bytes", name, maxOutput)
	}
	ws := <-exited
	if !ws.Exited() || ws.ExitStatus() != 0 {
		return nil, fmt.Errorf("%s: %s", name, describe(ws))
	}
	return out, readErr
}

// maxOutput caps what runCommand reads from a command, a screenshot say.
const maxOutput = 64 << 20
