package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/motion"
)

type call struct {
	name string
	args []string
}

func newTestVisual(fail bool) (*visual, *[]call) {
	var calls []call
	v := newVisual(1280, 800)
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name, args})
		if fail {
			return nil, errors.New("boom")
		}
		return []byte("\x89PNG fake"), nil
	}
	v.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "xdotool" || strings.Join(args, " ") != "getmouselocation" {
			return nil, fmt.Errorf("unexpected %s %q", name, args)
		}
		return []byte("x:100 y:700 screen:0 window:4194311\n"), nil
	}
	v.mot = motion.New(1)
	// Never the real display's keyboard map: a map with no spare keys,
	// unless a test gives it one (keymapVisual).
	v.keyboard = newFakeKeyboard(&calls).open
	v.settle = 0
	return v, &calls
}

func serveVisual(v *visual, method, target, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	v.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rec
}

func TestScreenshot(t *testing.T) {
	v, calls := newTestVisual(false)
	rec := serveVisual(v, http.MethodGet, "/screenshot", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("X-Screen-Width") != "1280" || rec.Header().Get("X-Screen-Height") != "800" {
		t.Fatalf("screenshot: %d %v", rec.Code, rec.Header())
	}
	got := strings.Join((*calls)[0].args, " ")
	if (*calls)[0].name != "gst-launch-1.0" || !strings.Contains(got, "ximagesrc display-name=:99 num-buffers=1") || strings.Contains(got, "videoscale") || !strings.HasSuffix(got, "pngenc ! fdsink fd=3") {
		t.Fatalf("args %q", got)
	}

	serveVisual(v, http.MethodGet, "/screenshot?width=640", "")
	if got := strings.Join((*calls)[1].args, " "); !strings.Contains(got, "videoscale ! video/x-raw,width=640,height=400") {
		t.Fatalf("scaled args %q", got)
	}
	serveVisual(v, http.MethodGet, "/screenshot?width=4000", "") // larger than the screen: not scaled up
	if got := strings.Join((*calls)[2].args, " "); strings.Contains(got, "videoscale") {
		t.Fatalf("scaled up: %q", got)
	}
	if rec := serveVisual(v, http.MethodGet, "/screenshot?width=x", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad width: %d", rec.Code)
	}
}

func TestClick(t *testing.T) {
	v, calls := newTestVisual(false)
	rec := serveVisual(v, http.MethodPost, "/input/click", `{"x": 40, "y": 50, "pace": "instant"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("click: %d %s", rec.Code, rec.Body)
	}
	if c := (*calls)[0]; c.name != "xdotool" || strings.Join(c.args, " ") != "mousemove 40 50 click 1" {
		t.Fatalf("call %+v", c)
	}
	serveVisual(v, http.MethodPost, "/input/click", `{"x": 1, "y": 2, "button": "right", "double": true, "pace": "instant"}`)
	if got := strings.Join((*calls)[1].args, " "); got != "mousemove 1 2 click --repeat 2 --delay 80 3" {
		t.Fatalf("args %q", got)
	}
	for _, body := range []string{`{"x": 1280, "y": 0}`, `{"x": -1, "y": 0}`, `{"x": 0, "y": 800}`, `{"x": 1, "y": 1, "button": "--help"}`, `{"x": 1, "y": 1, "extra": 1}`, `not json`} {
		if rec := serveVisual(v, http.MethodPost, "/input/click", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("bad requests ran xdotool: %d calls", len(*calls))
	}
}

func TestType(t *testing.T) {
	v, calls := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "--version hello\n", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	args := (*calls)[0].args
	if args[len(args)-2] != "--" || args[len(args)-1] != "--version hello\n" {
		t.Fatalf("text not passed after --: %q", args)
	}
	long := `{"pace": "instant", "text": "` + strings.Repeat("é", maxTypeLen+1) + `"}`
	for _, body := range []string{`{"text": ""}`, long} {
		if rec := serveVisual(v, http.MethodPost, "/input/type", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%.30s: %d", body, rec.Code)
		}
	}
}

func TestScroll(t *testing.T) {
	v, calls := newTestVisual(false)
	for body, want := range map[string]string{
		`{"x": 640, "y": 400, "dy": 3, "pace": "instant"}`:         "mousemove 640 400 click --repeat 3 --delay 30 5",
		`{"x": 1, "y": 2, "dx": -2, "dy": -1, "pace": "instant"}`:  "mousemove 1 2 click --repeat 1 --delay 30 4 click --repeat 2 --delay 30 6",
		`{"x": 1, "y": 2, "dx": 99, "pace": "instant"}`:            "mousemove 1 2 click --repeat 20 --delay 30 7",
		`{"x": 1279, "y": 799, "dy": -1000000, "pace": "instant"}`: "mousemove 1279 799 click --repeat 20 --delay 30 4",
	} {
		*calls = nil
		if rec := serveVisual(v, http.MethodPost, "/input/scroll", body); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
		if c := (*calls)[0]; c.name != "xdotool" || strings.Join(c.args, " ") != want {
			t.Errorf("%s: args %q, want %q", body, strings.Join(c.args, " "), want)
		}
	}
	*calls = nil
	for _, body := range []string{`{"x": 1280, "y": 0, "dy": 1}`, `{"x": -1, "y": 0, "dy": 1}`, `{"x": 1, "y": 1}`, `{"x": 1, "y": 1, "dy": 1.5}`, `{"x": 1, "y": 1, "dy": "1"}`, `{"x": 1, "y": 1, "dz": 1}`} {
		if rec := serveVisual(v, http.MethodPost, "/input/scroll", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("bad requests ran xdotool: %d calls", len(*calls))
	}
}

func TestKey(t *testing.T) {
	v, calls := newTestVisual(false)
	for body, want := range map[string]string{
		`{"keys": "ctrl+l"}`:                                     "ctrl+l",
		`{"keys": "Ctrl+Shift+T"}`:                               "ctrl+shift+T",
		`{"keys": ["Enter", "esc", "PageDown", "cmd+a", "Tab"]}`: "Return Escape Next super+a Tab",
		`{"keys": "ctrl + alt + Delete"}`:                        "ctrl+alt+Delete",
		`{"keys": ["F5", "U00E9", "BackSpace"]}`:                 "F5 U00E9 BackSpace",
	} {
		*calls = nil
		instant := strings.TrimSuffix(body, "}") + `, "pace": "instant"}`
		if rec := serveVisual(v, http.MethodPost, "/input/key", instant); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
		if c := (*calls)[0]; c.name != "xdotool" || strings.Join(c.args, " ") != "key --clearmodifiers --delay 50 -- "+want {
			t.Errorf("%s: args %q", body, c.args)
		}
	}
	*calls = nil
	eleven := `{"keys": [` + strings.TrimSuffix(strings.Repeat(`"a",`, maxCombos+1), ",") + `]}`
	for _, body := range []string{
		`{"keys": ""}`, `{"keys": []}`, `{"keys": "ctrl+"}`, `{"keys": "ctrl++"}`, `{"keys": "a b"}`,
		`{"keys": "--help"}`, `{"keys": "exec sh"}`, `{"keys": "a;b"}`, `{"keys": "a\nkey b"}`, `{"keys": "é"}`,
		`{"keys": "a+b+c+d+e+f"}`, `{"keys": "` + strings.Repeat("x", 33) + `"}`, eleven,
		`{"keys": 5}`, `{"keys": [1]}`, `{}`, `{"keys": "a", "text": "b"}`,
		// xdotool would run these as commands, not press them.
		`{"keys": ["Return", "exec", "touch", "pwned"]}`, `{"keys": ["EXEC"]}`, `{"keys": ["windowkill"]}`,
		`{"keys": ["ctrl+getactivewindow"]}`, `{"keys": ["sleep"]}`, `{"keys": ["Type"]}`, `{"keys": ["mousemove_relative"]}`,
	} {
		if rec := serveVisual(v, http.MethodPost, "/input/key", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("bad requests ran xdotool: %v", *calls)
	}
}

func TestDrag(t *testing.T) {
	v, calls := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/drag", `{"from": {"x": 100, "y": 200}, "to": {"x": 180, "y": 120}, "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("drag: %d %s", rec.Code, rec.Body)
	}
	got := strings.Join((*calls)[0].args, " ")
	want := "mousemove 100 200 sleep 0.05 mousedown 1" +
		" sleep 0.02 mousemove 110 190 sleep 0.02 mousemove 120 180 sleep 0.02 mousemove 130 170" +
		" sleep 0.02 mousemove 140 160 sleep 0.02 mousemove 150 150 sleep 0.02 mousemove 160 140" +
		" sleep 0.02 mousemove 170 130 sleep 0.02 mousemove 180 120 sleep 0.05 mouseup 1"
	if (*calls)[0].name != "xdotool" || got != want {
		t.Fatalf("args\n%q\nwant\n%q", got, want)
	}
	serveVisual(v, http.MethodPost, "/input/drag", `{"from": {"x": 0, "y": 0}, "to": {"x": 1279, "y": 799}, "button": "right", "pace": "instant"}`)
	if got := strings.Join((*calls)[1].args, " "); !strings.Contains(got, "mousedown 3") || !strings.HasSuffix(got, "mousemove 1279 799 sleep 0.05 mouseup 3") {
		t.Fatalf("right drag: %q", got)
	}
	*calls = nil
	for _, body := range []string{
		`{"from": {"x": 1, "y": 1}, "to": {"x": 1280, "y": 1}}`, `{"from": {"x": -1, "y": 1}, "to": {"x": 1, "y": 1}}`,
		`{"from": {"x": 1, "y": 1}, "to": {"x": 1, "y": 800}}`, `{"from": {"x": 1, "y": 1}, "to": {"x": 2, "y": 2}, "button": "4"}`,
		`{"from": {"x": 1, "y": 1}, "to": {"x": 2, "y": 2, "z": 3}}`,
	} {
		if rec := serveVisual(v, http.MethodPost, "/input/drag", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("bad requests ran xdotool: %v", *calls)
	}
}

func TestDragReleasesTheButtonWhenItFails(t *testing.T) {
	v, calls := newTestVisual(true)
	if rec := serveVisual(v, http.MethodPost, "/input/drag", `{"from": {"x": 1, "y": 1}, "to": {"x": 2, "y": 2}, "button": "middle"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing drag: %d", rec.Code)
	}
	if len(*calls) != 2 || strings.Join((*calls)[1].args, " ") != "mouseup 2" {
		t.Fatalf("calls %v, want a mouseup after the failed drag", *calls)
	}
}

func TestVisualCommandFailure(t *testing.T) {
	v, _ := newTestVisual(true)
	if rec := serveVisual(v, http.MethodPost, "/input/click", `{"x": 1, "y": 1}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing xdotool: %d", rec.Code)
	}
	if rec := serveVisual(v, http.MethodGet, "/screenshot", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing gst: %d", rec.Code)
	}
}

func TestRunCommand(t *testing.T) {
	out, err := runCommand(context.Background(), "sh", "-c", "printf hello >&3")
	if err != nil || string(out) != "hello" {
		t.Fatalf("runCommand = %q, %v", out, err)
	}
	if _, err := runCommand(context.Background(), "sh", "-c", "exit 2"); err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("failing command: %v", err)
	}
}

func TestRunCommandStopsAnOverlongOutput(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := runCommand(context.Background(), "sh", "-c", "yes >&3")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "output over") {
			t.Fatalf("runCommand = %v, want an output-too-large error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runCommand hung on a command that never stops writing")
	}
}

// moves returns the points of an xdotool run's mousemoves, and the
// arguments after the last one.
func moves(t *testing.T, args []string) (pts []point, rest []string) {
	t.Helper()
	last := -1
	for i := 0; i < len(args); i++ {
		if args[i] == "mousemove" {
			var p point
			if _, err := fmt.Sscanf(args[i+1]+" "+args[i+2], "%d %d", &p.X, &p.Y); err != nil {
				t.Fatalf("mousemove %q %q", args[i+1], args[i+2])
			}
			pts = append(pts, p)
			last = i + 2
		}
	}
	return pts, args[last+1:]
}

func TestHumanClickGlidesAimsAndHolds(t *testing.T) {
	v, calls := newTestVisual(false)
	// The pointer is at (100, 700): a 500 px throw.
	if rec := serveVisual(v, http.MethodPost, "/input/click", `{"x": 600, "y": 700}`); rec.Code != http.StatusNoContent {
		t.Fatalf("click: %d %s", rec.Code, rec.Body)
	}
	if len(*calls) != 1 || (*calls)[0].name != "xdotool" {
		t.Fatalf("calls %v", *calls)
	}
	args := (*calls)[0].args
	if slices.Contains(args, "click") {
		t.Fatalf("a human click used xdotool's click: %q", args)
	}
	pts, rest := moves(t, args)
	if len(pts) <= 5 {
		t.Fatalf("%d moves for a 500 px click, want a glide", len(pts))
	}
	if len(rest) != 8 || rest[0] != "sleep" || rest[2] != "mousedown" || rest[3] != "1" || rest[4] != "sleep" || rest[6] != "mouseup" || rest[7] != "1" {
		t.Fatalf("after the glide %q, want sleep, mousedown 1, sleep, mouseup 1", rest)
	}
	hold, _ := strconv.ParseFloat(rest[5], 64)
	if hold < 0.06 || hold > 0.2 {
		t.Fatalf("held %v s", hold)
	}

	// Over many clicks at one point, the landing scatters, but within 40%
	// of the 8 px landing size, so a click asked for on a 13 px checkbox
	// stays on it.
	seen := map[point]bool{}
	for seed := range uint64(300) {
		*calls = nil
		v.mot = motion.New(seed)
		serveVisual(v, http.MethodPost, "/input/click", `{"x": 640, "y": 400, "button": "right"}`)
		pts, rest := moves(t, (*calls)[0].args)
		at := pts[len(pts)-1]
		if d := max(abs(at.X-640), abs(at.Y-400)); d > 3 {
			t.Fatalf("seed %d: landed at %v, %d px from the point", seed, at, d)
		}
		if rest[3] != "3" {
			t.Fatalf("right click pressed %q", rest[3])
		}
		seen[at] = true
	}
	if len(seen) < 10 {
		t.Fatalf("300 clicks landed on only %d pixels", len(seen))
	}
}

func abs(n int) int { return max(n, -n) }

func TestHumanDoubleClickPressesTwice(t *testing.T) {
	v, calls := newTestVisual(false)
	serveVisual(v, http.MethodPost, "/input/click", `{"x": 300, "y": 300, "double": true}`)
	_, rest := moves(t, (*calls)[0].args)
	got := strings.Join(slices.DeleteFunc(slices.Clone(rest), func(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil && s != "1" }), " ")
	if got != "sleep mousedown 1 sleep mouseup 1 sleep mousedown 1 sleep mouseup 1" {
		t.Fatalf("double click %q", rest)
	}
	gap, _ := strconv.ParseFloat(rest[9], 64)
	if gap < 0.07 || gap > 0.11 {
		t.Fatalf("%v s between the presses, want about 90 ms", gap)
	}
}

func TestPointerEntersFromAnEdge(t *testing.T) {
	for _, where := range []string{"x:640 y:400 screen:0 window:1", "garbage"} {
		v, calls := newTestVisual(false)
		v.output = func(context.Context, string, ...string) ([]byte, error) { return []byte(where), nil }
		serveVisual(v, http.MethodPost, "/input/click", `{"x": 600, "y": 300}`)
		pts, _ := moves(t, (*calls)[0].args)
		if p := pts[0]; p.X > 2 && p.X < 1277 && p.Y < 797 {
			t.Fatalf("%q: first move to %v, not an edge", where, p)
		}
	}
}

// Once an action has moved the pointer, the middle of the screen is
// where it was left, not a pointer that never moved: the next glide
// starts there instead of coming in from an edge.
func TestPointerLeftInTheMiddleStaysThere(t *testing.T) {
	for _, first := range []string{
		`{"x": 640, "y": 400}`,
		`{"x": 640, "y": 400, "pace": "instant"}`,
	} {
		v, calls := newTestVisual(false)
		at := "x:640 y:400 screen:0 window:1"
		v.output = func(context.Context, string, ...string) ([]byte, error) { return []byte(at), nil }
		serveVisual(v, http.MethodPost, "/input/click", first)
		*calls = nil
		serveVisual(v, http.MethodPost, "/input/click", `{"x": 200, "y": 300}`)
		pts, _ := moves(t, (*calls)[0].args)
		if p := pts[0]; abs(p.X-640) > 60 || abs(p.Y-400) > 60 {
			t.Fatalf("after %s, the glide from the middle started at %v", first, p)
		}
	}
}

// typed reads back what a human-paced type types, and the gaps after
// each keystroke.
func typed(t *testing.T, cmds []call) (text string, gaps []string) {
	t.Helper()
	names := map[string]rune{}
	for r, k := range charKeys {
		if r != '\r' { // Return reads back as \n
			names[k] = r
		}
	}
	for _, c := range cmds {
		if c.name != "xdotool" {
			continue
		}
		a := c.args
		for i := 0; i < len(a); i++ {
			switch a[i] {
			case "keydown":
				k := a[i+1]
				if a[i+2] != "sleep" || a[i+4] != "keyup" || a[i+5] != k {
					t.Fatalf("keydown %s not followed by sleep and its keyup: %q", k, a[i:])
				}
				if r, ok := names[k]; ok {
					text += string(r)
				} else if cp, err := strconv.ParseUint(strings.TrimPrefix(k, "U"), 16, 32); strings.HasPrefix(k, "U") && err == nil {
					text += string(rune(cp))
				} else {
					text += k
				}
				i += 5
			case "sleep":
				gaps = append(gaps, a[i+1])
				i++
			default:
				t.Fatalf("unexpected %q in %q", a[i], a)
			}
		}
	}
	return text, gaps
}

func TestHumanTypeHasATypistsRhythm(t *testing.T) {
	v, calls, _ := keymapVisual(t, 8)
	text := "Hello, World! it's 9:30 am\tok"
	if n := len([]rune(text)); n != 29 {
		t.Fatal(n)
	}
	text += "é\n" // 31: an accent is a key too, once bound (keymap.go)
	body, _ := json.Marshal(map[string]string{"text": text})
	if rec := serveVisual(v, http.MethodPost, "/input/type", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	got, gaps := typed(t, *calls)
	if got != text {
		t.Fatalf("typed %q, want %q", got, text)
	}
	// Holds and gaps interleave: hold, gap, hold, gap…; the gaps vary.
	var between []string
	for i := 1; i < len(gaps); i += 2 {
		between = append(between, gaps[i])
	}
	for i := 1; i < len(between); i++ {
		if between[i] == between[i-1] {
			t.Fatalf("two gaps in a row of %s s: %q", between[i], between)
		}
	}
	for _, c := range *calls {
		if n := strings.Count(strings.Join(c.args, " "), "keydown"); n > typeChunk {
			t.Fatalf("%d keystrokes in one run", n)
		}
	}
	if len(*calls) < 4 {
		t.Fatalf("31 characters in %d runs, want them split", len(*calls))
	}
}

func TestHumanTypeLimit(t *testing.T) {
	v, calls := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "`+strings.Repeat("a", maxHumanTypeLen+1)+`"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "instant") {
		t.Fatalf("over the human limit: %d %s", rec.Code, rec.Body)
	}
	if len(*calls) != 0 {
		t.Fatal("ran xdotool")
	}
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "`+strings.Repeat("a", maxHumanTypeLen+1)+`", "pace": "instant"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("instant: %d", rec.Code)
	}
}

func TestCharKeysAreKeys(t *testing.T) {
	for r, k := range charKeys {
		if !keysymRE.MatchString(k) || xdotoolCommand(k) {
			t.Errorf("%q: %q", r, k)
		}
	}
	for r := rune(0x20); r < 0x7f; r++ {
		if _, ok := keysym(r); !ok {
			t.Errorf("no key for %q", r)
		}
	}
	for _, r := range "é€😀\x1b\x00" {
		if k, ok := keysym(r); ok {
			t.Errorf("%q has key %q", r, k)
		}
	}
}

// A type that fails or is cut short lets go of the keys of the run under
// way, and the runs after it don't happen.
func TestHumanTypeReleasesKeysWhenItFails(t *testing.T) {
	v, calls := newTestVisual(false)
	n := 0
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		if n++; n == 2 {
			return nil, errors.New("killed")
		}
		return nil, nil
	}
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "abcdefgh ijklmnop qrstuvwx"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing type: %d", rec.Code)
	}
	if len(*calls) != 3 {
		t.Fatalf("%d runs, want 2 and a release", len(*calls))
	}
	if got := strings.Join((*calls)[2].args, " "); got != "keyup space i j k l m n o" {
		t.Fatalf("release %q", got)
	}

	// A request already over runs nothing but the release.
	*calls = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mux := http.NewServeMux()
	v.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/input/click", strings.NewReader(`{"x": 5, "y": 5}`)))
	if rec.Code != http.StatusInternalServerError || len(*calls) != 1 || strings.Join((*calls)[0].args, " ") != "mouseup 1" {
		t.Fatalf("cancelled click: %d, calls %v", rec.Code, *calls)
	}
}

func TestHumanScrollEases(t *testing.T) {
	v, calls := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/scroll", `{"x": 640, "y": 400, "dy": 9, "dx": -2}`); rec.Code != http.StatusNoContent {
		t.Fatalf("scroll: %d %s", rec.Code, rec.Body)
	}
	pts, rest := moves(t, (*calls)[0].args)
	if len(pts) <= 5 || pts[len(pts)-1] != (point{640, 400}) {
		t.Fatalf("glide %v", pts)
	}
	var down []float64
	left := 0
	for i := 2; i < len(rest); i += 4 {
		if rest[i] != "click" || rest[i+2] != "sleep" {
			t.Fatalf("notches %q", rest)
		}
		p, _ := strconv.ParseFloat(rest[i+3], 64)
		switch rest[i+1] {
		case "5":
			down = append(down, p)
		case "6":
			left++
		}
	}
	if len(down) != 9 || left != 2 {
		t.Fatalf("%d down and %d left notches", len(down), left)
	}
	if down[4] >= down[0] || down[4] >= down[8] {
		t.Fatalf("pauses %v: the middle notches should come faster", down)
	}
}

func TestHumanDragCarriesThePointer(t *testing.T) {
	v, calls := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/drag", `{"from": {"x": 100, "y": 200}, "to": {"x": 500, "y": 260}, "button": "middle"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("drag: %d %s", rec.Code, rec.Body)
	}
	args := (*calls)[0].args
	down := slices.Index(args, "mousedown")
	before, _ := moves(t, args[:down])
	after, rest := moves(t, args[down:])
	if before[len(before)-1] != (point{100, 200}) || args[down+1] != "2" {
		t.Fatalf("pressed at %v with %q", before[len(before)-1], args[down+1])
	}
	if len(after) <= 5 || after[len(after)-1] != (point{500, 260}) {
		t.Fatalf("carried along %v", after)
	}
	if len(rest) != 4 || rest[2] != "mouseup" || rest[3] != "2" {
		t.Fatalf("after the drag %q", rest)
	}
}

func TestHumanKeysAreHeld(t *testing.T) {
	v, calls := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/key", `{"keys": ["ctrl+l", "Return"]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("key: %d %s", rec.Code, rec.Body)
	}
	a := (*calls)[0].args
	if len(a) != 14 || a[0] != "keydown" || a[1] != "ctrl+l" || a[4] != "keyup" || a[5] != "ctrl+l" || a[6] != "sleep" || a[8] != "keydown" || a[9] != "Return" || a[12] != "keyup" || a[13] != "Return" {
		t.Fatalf("keys %q", a)
	}
}

func TestPaceIsChecked(t *testing.T) {
	v, calls := newTestVisual(false)
	for path, body := range map[string]string{
		"/input/click":  `{"x": 1, "y": 1, "pace": "slow"}`,
		"/input/type":   `{"text": "a", "pace": "--help"}`,
		"/input/scroll": `{"x": 1, "y": 1, "dy": 1, "pace": "Human"}`,
		"/input/key":    `{"keys": "a", "pace": 1}`,
		"/input/drag":   `{"from": {"x": 1, "y": 1}, "to": {"x": 2, "y": 2}, "pace": "fast"}`,
	} {
		if rec := serveVisual(v, http.MethodPost, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: %d", path, body, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("bad paces ran xdotool: %v", *calls)
	}
}
