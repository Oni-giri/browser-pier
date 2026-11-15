package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/motion"
)

// visual serves Visual mode: screenshots of the X display and OS-level
// input through xdotool. Nothing here touches CDP, so a page can't tell
// an agent from a person by a debugger being attached.
type visual struct {
	width, height int
	// run runs a command, returning what it wrote to fd 3; output runs
	// one returning what it wrote to stdout.
	run, output func(ctx context.Context, name string, args ...string) ([]byte, error)
	mu          sync.Mutex // one input action at a time, so two never interleave
	// mot paces input like a hand (PROJ-113), with v.mu held. Each
	// container is one session, so its seed, from crypto/rand, is this
	// session's own.
	mot *motion.Motion
	// live is the live view's continuous mouse and keyboard.
	live *liveInput
}

// maxTypeLen caps one type action; at human pace, which takes about a
// quarter of a second a character, maxHumanTypeLen does (500 characters
// is about two minutes).
const (
	maxTypeLen      = 2000
	maxHumanTypeLen = 500
)

func newVisual(width, height int) *visual {
	return &visual{width: width, height: height, run: runCommand, output: runStdout, mot: motion.NewRandom(), live: newLiveInput(width, height)}
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

// Pace is how an input action moves (PROJ-113). At human pace, the
// default, the pointer glides along a hand's path, buttons and keys are
// held, and keystrokes come at a typist's uneven rhythm (internal/motion).
// At instant pace, the arguments are what they were before PROJ-113.
const (
	paceHuman   = "human"
	paceInstant = "instant"
)

// instant reports whether a request's pace is instant, or why it is
// neither pace.
func instant(pace string) (bool, error) {
	switch pace {
	case "", paceHuman:
		return false, nil
	case paceInstant:
		return true, nil
	}
	return false, fmt.Errorf("pace %q: want human or instant", pace)
}

// aimSize is the target size, in pixels, Visual mode assumes for Fitts's
// law and the aim's scatter: it sees only a point, not what is there. A
// click lands within 40% of it (about 18 px) of the point asked for.
const aimSize = 44

type clickRequest struct {
	X, Y   int
	Button string // left (default), middle or right
	Double bool
	Pace   string
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

// humanClick glides from the pointer to a point scattered around the one
// asked for, pauses, and presses and holds the button (twice for a
// double click). The request has been checked (clickArgs).
func (v *visual) humanClick(from hand, c clickRequest) []string {
	b := buttons[c.Button]
	at := v.mot.Aim(motion.Point{X: float64(c.X), Y: float64(c.Y)}, motion.Size{W: aimSize, H: aimSize})
	args := v.glide(nil, from, at)
	args = append(args, "sleep", secs(v.mot.Dwell()))
	presses := 1
	if c.Double {
		presses = 2
	}
	for i := range presses {
		if i > 0 {
			args = append(args, "sleep", secs(v.mot.JitterDuration(0.09, 0.02)))
		}
		args = append(args, "mousedown", b, "sleep", secs(v.mot.KeyHold()), "mouseup", b)
	}
	return args
}

func (v *visual) click(w http.ResponseWriter, r *http.Request) {
	var c clickRequest
	if !decode(w, r, &c) {
		return
	}
	now, err := instant(c.Pace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args, err := v.clickArgs(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	release := []string{"mouseup", buttons[c.Button]}
	v.perform(w, r, func(from func() hand) []xcmd {
		if !now {
			args = v.humanClick(from(), c)
		}
		return []xcmd{{args, release}}
	})
}

// xcmd is one xdotool run and, if it fails or is cut short, the commands
// that let go of whatever it may have left held.
type xcmd struct{ args, release []string }

// perform runs an action's xdotool commands one after another, with v.mu
// held: build can ask where the pointer is now. If one fails, or the request
// ends (a handoff interrupting a long type, say), the rest don't run and
// its release does, so no button or key stays held.
func (v *visual) perform(w http.ResponseWriter, r *http.Request, build func(from func() hand) []xcmd) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ctx := r.Context()
	for _, c := range build(func() hand { return v.pointer(ctx) }) {
		err := ctx.Err()
		if err == nil {
			_, err = v.run(ctx, "xdotool", c.args...)
		}
		if err != nil {
			if len(c.release) > 0 {
				_, _ = v.run(context.WithoutCancel(ctx), "xdotool", c.release...)
			}
			http.Error(w, "input failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// hand is where an action's pointer starts from.
type hand struct {
	at motion.Point
	// enter: the pointer has never moved, and glide first puts it at at,
	// just inside an edge, as if a hand came in from outside the window.
	enter bool
}

// pointer is where the X pointer is now: wherever the last action, or a
// person in the live view, left it. A pointer that has never moved sits
// in the middle of the screen, where no hand starts from.
func (v *visual) pointer(ctx context.Context) hand {
	out, err := v.output(ctx, "xdotool", "getmouselocation")
	var x, y int
	if err == nil {
		_, err = fmt.Sscanf(string(out), "x:%d y:%d", &x, &y)
	}
	if err != nil || (x == v.width/2 && y == v.height/2) {
		return hand{v.mot.Entry(motion.Size{W: float64(v.width), H: float64(v.height)}), true}
	}
	return hand{motion.Point{X: float64(x), Y: float64(y)}, false}
}

// glide appends the moves of a hand's path from the pointer to a point.
func (v *visual) glide(args []string, from hand, to motion.Point) []string {
	if from.enter {
		args = append(args, v.moveTo(from.at)...)
	}
	last := v.screenPoint(from.at)
	var wait time.Duration
	for _, s := range v.mot.Path(from.at, to, aimSize) {
		if p := v.screenPoint(motion.Point{X: s.X, Y: s.Y}); p != last {
			if wait > 0 {
				args = append(args, "sleep", secs(wait))
			}
			args = append(args, "mousemove", strconv.Itoa(p.X), strconv.Itoa(p.Y))
			last, wait = p, 0
		}
		wait += s.Pause
	}
	return args
}

func (v *visual) moveTo(p motion.Point) []string {
	s := v.screenPoint(p)
	return []string{"mousemove", strconv.Itoa(s.X), strconv.Itoa(s.Y)}
}

// screenPoint rounds a point to a pixel on the screen.
func (v *visual) screenPoint(p motion.Point) point {
	clamp := func(f float64, n int) int { return min(max(int(math.Round(f)), 0), n-1) }
	return point{clamp(p.X, v.width), clamp(p.Y, v.height)}
}

// secs writes a pause as xdotool's sleep takes it, in seconds.
func secs(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 3, 64) }

type typeRequest struct {
	Text string
	Pace string
}

func typeArgs(text string) ([]string, error) {
	if err := checkText(text, maxTypeLen); err != nil {
		return nil, err
	}
	// "--" so text starting with a dash is typed, not read as an option.
	return []string{"type", "--clearmodifiers", "--delay", "12", "--", text}, nil
}

func checkText(text string, most int) error {
	if text == "" || !utf8.ValidString(text) {
		return fmt.Errorf("text: want non-empty UTF-8")
	}
	if n := utf8.RuneCountInString(text); n > most {
		msg := fmt.Sprintf("text: %d characters, at most %d per call", n, most)
		if most == maxHumanTypeLen {
			msg += " at human pace (about two minutes); split it, or type it at instant pace"
		}
		return errors.New(msg)
	}
	return nil
}

// charKeys names the X keysym of each character a US keyboard types,
// apart from letters and digits, which are their own names.
var charKeys = map[rune]string{
	' ': "space", '\n': "Return", '\t': "Tab",
	'!': "exclam", '"': "quotedbl", '#': "numbersign", '$': "dollar", '%': "percent", '&': "ampersand",
	'\'': "apostrophe", '(': "parenleft", ')': "parenright", '*': "asterisk", '+': "plus", ',': "comma",
	'-': "minus", '.': "period", '/': "slash", ':': "colon", ';': "semicolon", '<': "less", '=': "equal",
	'>': "greater", '?': "question", '@': "at", '[': "bracketleft", '\\': "backslash", ']': "bracketright",
	'^': "asciicircum", '_': "underscore", '`': "grave", '{': "braceleft", '|': "bar", '}': "braceright",
	'~': "asciitilde",
}

// keysym is the fixed X keysym name that types r, if it has one here.
func keysym(r rune) (string, bool) {
	if r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
		return string(r), true
	}
	k, ok := charKeys[r]
	return k, ok
}

// typeChunk is how many keystrokes one xdotool run types at human pace:
// at most about 2 s each, well inside runCommand's 30 s, and a cancel
// stops the rest between runs as well as killing the one under way.
const typeChunk = 8

// humanType types text one keystroke at a time: each key held for a
// typist's hold, with a typist's gaps between presses. The keys come
// from charKeys; a character with none there keeps the instant path
// ("type --"), on its own. Each run's release lets go of its keys.
func (v *visual) humanType(text string) []xcmd {
	runes := []rune(text)
	var cmds []xcmd
	var args, held []string
	keys := 0
	flush := func() {
		if len(args) > 0 {
			release := []string(nil)
			if len(held) > 0 {
				release = append([]string{"keyup"}, held...)
			}
			cmds = append(cmds, xcmd{args, release})
		}
		args, held, keys = nil, nil, 0
	}
	for i, r := range runes {
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		gap := v.mot.KeyDelay(r, next)
		k, ok := keysym(r)
		if !ok {
			// type takes every argument after it as text, so it ends a run.
			args = append(args, "type", "--clearmodifiers", "--", string(r))
			flush()
			if next != 0 {
				args = append(args, "sleep", secs(gap))
			}
			continue
		}
		hold := v.mot.KeyHold()
		args = append(args, "keydown", k, "sleep", secs(hold), "keyup", k)
		if !slices.Contains(held, k) {
			held = append(held, k)
		}
		if next != 0 {
			args = append(args, "sleep", secs(max(gap-hold, 20*time.Millisecond)))
		}
		if keys++; keys == typeChunk {
			flush()
		}
	}
	flush()
	return cmds
}

func (v *visual) typeText(w http.ResponseWriter, r *http.Request) {
	var t typeRequest
	if !decode(w, r, &t) {
		return
	}
	now, err := instant(t.Pace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if now {
		args, err := typeArgs(t.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v.input(w, r, args)
		return
	}
	if err := checkText(t.Text, maxHumanTypeLen); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.perform(w, r, func(func() hand) []xcmd { return v.humanType(t.Text) })
}

type scrollRequest struct {
	X, Y   int
	DX, DY int // notches: positive scrolls right and down
	Pace   string
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

// humanScroll glides to the point and turns the wheel in notches that
// come faster in the middle of the turn than at its ends.
func (v *visual) humanScroll(from hand, s scrollRequest) []string {
	args := v.glide(nil, from, motion.Point{X: float64(s.X), Y: float64(s.Y)})
	args = append(args, "sleep", secs(v.mot.JitterDuration(0.12, 0.04)))
	steps := func(n int, neg, pos string) {
		b := pos
		if n < 0 {
			b, n = neg, -n
		}
		n = min(n, maxScroll)
		if n == 0 {
			return
		}
		// A notch's pause is shorter the bigger its share of the turn.
		ticks := v.mot.WheelTicks(float64(n) * 60)
		for _, t := range ticks {
			pause := time.Duration(float64(t.Pause) * 60 / t.Delta)
			args = append(args, "click", b, "sleep", secs(min(max(pause, 12*time.Millisecond), 150*time.Millisecond)))
		}
	}
	steps(s.DY, "4", "5")
	steps(s.DX, "6", "7")
	return args
}

func (v *visual) scroll(w http.ResponseWriter, r *http.Request) {
	var s scrollRequest
	if !decode(w, r, &s) {
		return
	}
	now, err := instant(s.Pace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args, err := v.scrollArgs(s)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if now {
		v.input(w, r, args)
		return
	}
	v.perform(w, r, func(from func() hand) []xcmd {
		// The wheel's notches are clicks, pressed and released inside
		// xdotool: nothing stays held between commands.
		return []xcmd{{v.humanScroll(from(), s), nil}}
	})
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

type keyRequest struct {
	Keys combos
	Pace string
}

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

var keyArgsPrefix = []string{"key", "--clearmodifiers", "--delay", "50", "--"}

func keyArgs(keys combos) ([]string, error) {
	if len(keys) == 0 || len(keys) > maxCombos {
		return nil, fmt.Errorf("keys: want 1 to %d key combinations", maxCombos)
	}
	args := slices.Clone(keyArgsPrefix)
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

// humanKeys presses each combination (checked by keyArgs) and holds it
// as a hand would, with a pause between combinations.
func (v *visual) humanKeys(combos []string) []string {
	var args []string
	for i, c := range combos {
		if i > 0 {
			args = append(args, "sleep", secs(v.mot.JitterDuration(0.18, 0.06)))
		}
		args = append(args, "keydown", c, "sleep", secs(v.mot.KeyHold()), "keyup", c)
	}
	return args
}

func (v *visual) key(w http.ResponseWriter, r *http.Request) {
	var k keyRequest
	if !decode(w, r, &k) {
		return
	}
	now, err := instant(k.Pace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args, err := keyArgs(k.Keys)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if now {
		v.input(w, r, args)
		return
	}
	combos := args[len(keyArgsPrefix):]
	v.perform(w, r, func(func() hand) []xcmd {
		return []xcmd{{v.humanKeys(combos), append([]string{"keyup"}, combos...)}}
	})
}

type point struct{ X, Y int }

type dragRequest struct {
	From, To point
	Button   string // left (default), middle or right
	Pace     string
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

// humanDrag glides to the start, presses, carries the pointer along a
// hand's path to the end and lets go. Both ends are exact: what a drag
// grabs and where it drops it matter more than a click's aim.
func (v *visual) humanDrag(from hand, d dragRequest) []string {
	b := buttons[d.Button]
	start, end := motion.Point{X: float64(d.From.X), Y: float64(d.From.Y)}, motion.Point{X: float64(d.To.X), Y: float64(d.To.Y)}
	args := v.glide(nil, from, start)
	args = append(args, "sleep", secs(v.mot.Dwell()), "mousedown", b, "sleep", secs(v.mot.JitterDuration(0.12, 0.03)))
	args = v.glide(args, hand{at: start}, end)
	return append(args, "sleep", secs(v.mot.JitterDuration(0.1, 0.03)), "mouseup", b)
}

func (v *visual) drag(w http.ResponseWriter, r *http.Request) {
	var d dragRequest
	if !decode(w, r, &d) {
		return
	}
	now, err := instant(d.Pace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args, err := v.dragArgs(d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Don't leave the button held if the drag breaks off half way.
	release := []string{"mouseup", buttons[d.Button]}
	v.perform(w, r, func(from func() hand) []xcmd {
		if !now {
			args = v.humanDrag(from(), d)
		}
		return []xcmd{{args, release}}
	})
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
	return runTo(ctx, false, name, args...)
}

// runStdout is runCommand for what a command writes to stdout.
func runStdout(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runTo(ctx, true, name, args...)
}

func runTo(ctx context.Context, stdout bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = pr.Close() }()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	if stdout {
		cmd.Stdout = pw
	} else {
		cmd.ExtraFiles = []*os.File{pw}
	}
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
