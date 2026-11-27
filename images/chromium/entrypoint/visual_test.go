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
	v.keymap = newFakeKeyboard(&calls).open
	v.bindSettle = 0
	v.chromium = addressBarChromium
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
		`{"keys": "ctrl+l"}`:       "ctrl+l",
		`{"keys": "Ctrl+Shift+T"}`: "ctrl+shift+T",
		// After a browser-bound combination, the rest waits shortcutGap.
		`{"keys": ["Enter", "esc", "PageDown", "cmd+a", "Tab"]}`: "Return Escape Next super+a sleep 0.150 key --clearmodifiers --delay 50 -- Tab",
		`{"keys": "ctrl + alt + Delete"}`:                        "ctrl+alt+Delete",
		`{"keys": ["F5", "U00E9", "BackSpace"]}`:                 "F5 sleep 0.150 key --clearmodifiers --delay 50 -- U00E9 BackSpace",
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

// clockedVisual is a test visual on a fake clock: each xdotool run takes
// 30 ms, sleeps move the clock, and every call notes when it started.
type clockedVisual struct {
	*visual
	t     time.Time
	slept []time.Duration
	calls []timedCall
}

type timedCall struct {
	at   time.Duration // since the clock's start
	args []string
}

const xdotoolTakes = 30 * time.Millisecond

var clockStart = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

func newClockedVisual() *clockedVisual {
	v, _ := newTestVisual(false)
	c := &clockedVisual{visual: v, t: clockStart}
	v.now = func() time.Time { return c.t }
	v.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.slept = append(c.slept, d)
		c.t = c.t.Add(d)
		return nil
	}
	v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "xdotool" {
			c.calls = append(c.calls, timedCall{c.t.Sub(clockStart), args})
		}
		c.t = c.t.Add(xdotoolTakes)
		return []byte("\x89PNG fake"), nil
	}
	return c
}

// do serves one request and checks it worked; it returns when the
// request's first xdotool run started, or -1 if none did.
func (c *clockedVisual) do(t *testing.T, method, path, body string) time.Duration {
	t.Helper()
	n := len(c.calls)
	rec := serveVisual(c.visual, method, path, body)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", path, body, rec.Code, rec.Body)
	}
	if len(c.calls) == n {
		return -1
	}
	return c.calls[n].at
}

func TestBrowserBound(t *testing.T) {
	for _, c := range []string{"ctrl+l", "alt+d", "super+l", "ctrl+shift+t", "F6", "Control+l", "CTRL+L", "cmd+a", "option+d",
		"Control_L+l", "Alt_R+d", "F1", "F12", "shift+F6", "f6",
		// A modifier after an ordinary key is pressed with it all the same.
		"a+Control_L", "l+ctrl", "d+alt", "a+shift+super", "F6+ctrl"} {
		if !browserBound(c) {
			t.Errorf("%s doesn't arm the gap", c)
		}
	}
	for _, c := range []string{"a", "shift+a", "Return", "Tab", "Escape", "BackSpace", "Left", "space", "shift+Tab", "F13",
		"F24", "Delete", "Next", "U00E9", "Shift_L+a", "a+F6", "F6+a", "a+shift"} {
		if browserBound(c) {
			t.Errorf("%s arms the gap", c)
		}
	}
}

func TestShortcutGapIsCapped(t *testing.T) {
	if shortcutGap <= 0 || shortcutGap > 250*time.Millisecond {
		t.Fatalf("shortcutGap is %v: a wait holds v.mu, and a handoff with it, so it is at most 250 ms", shortcutGap)
	}
}

func TestTextRightAfterAShortcutWaitsForTheGap(t *testing.T) {
	for _, pace := range []string{"instant", "human"} {
		c := newClockedVisual()
		key := c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "`+pace+`"}`)
		ended := key + xdotoolTakes
		typed := c.do(t, http.MethodPost, "/input/type", `{"text": "example.com", "pace": "`+pace+`"}`)
		if typed < ended+shortcutGap {
			t.Errorf("%s: the text started %v after the key call ended, want at least %v", pace, typed-ended, shortcutGap)
		}
		if len(c.slept) != 1 || c.slept[0] != shortcutGap {
			t.Errorf("%s: slept %v, want one %v", pace, c.slept, shortcutGap)
		}
		// Return after the text isn't browser-bound's follower: no wait.
		c.slept = nil
		c.do(t, http.MethodPost, "/input/key", `{"keys": "Return", "pace": "`+pace+`"}`)
		if len(c.slept) != 0 {
			t.Errorf("%s: Return after the text waited %v", pace, c.slept)
		}
	}
}

func TestACallAfterTheGapDoesntWait(t *testing.T) {
	c := newClockedVisual()
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	c.t = c.t.Add(shortcutGap + time.Millisecond)
	c.do(t, http.MethodPost, "/input/type", `{"text": "example.com", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Fatalf("waited %v after more than the gap", c.slept)
	}
	// Part of the gap gone: only the rest is waited.
	c.do(t, http.MethodPost, "/input/key", `{"keys": "alt+d", "pace": "instant"}`)
	c.t = c.t.Add(100 * time.Millisecond)
	c.do(t, http.MethodPost, "/input/type", `{"text": "x", "pace": "instant"}`)
	if len(c.slept) != 1 || c.slept[0] != shortcutGap-100*time.Millisecond {
		t.Fatalf("slept %v, want the rest of the gap", c.slept)
	}
}

func TestPlainKeysAndTypingDontWait(t *testing.T) {
	c := newClockedVisual()
	for _, step := range [][2]string{
		{"/input/key", `{"keys": "a", "pace": "instant"}`},
		{"/input/type", `{"text": "hello", "pace": "instant"}`},
		{"/input/type", `{"text": "world", "pace": "instant"}`},
		{"/input/key", `{"keys": ["shift+a", "Tab", "Return"], "pace": "instant"}`},
		{"/input/type", `{"text": "pw", "pace": "human"}`},
		{"/input/key", `{"keys": "BackSpace"}`},
		{"/input/type", `{"text": "x"}`},
		// A list whose bound combination isn't the last had its gap inside.
		{"/input/key", `{"keys": ["ctrl+l", "a"], "pace": "instant"}`},
		{"/input/type", `{"text": "x", "pace": "instant"}`},
	} {
		c.do(t, http.MethodPost, step[0], step[1])
	}
	if len(c.slept) != 0 {
		t.Fatalf("plain keys and typing waited %v", c.slept)
	}
}

func TestAShortcutAfterAShortcutWaitsAndArms(t *testing.T) {
	c := newClockedVisual()
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	second := c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+v", "pace": "instant"}`)
	if second != xdotoolTakes+shortcutGap {
		t.Fatalf("ctrl+v started at %v, want %v", second, xdotoolTakes+shortcutGap)
	}
	third := c.do(t, http.MethodPost, "/input/key", `{"keys": "Return", "pace": "instant"}`)
	if third != second+xdotoolTakes+shortcutGap {
		t.Fatalf("Return started at %v, want %v: ctrl+v arms the gap again", third, second+xdotoolTakes+shortcutGap)
	}
}

func TestAShortcutListWaitsInside(t *testing.T) {
	c := newClockedVisual()
	c.do(t, http.MethodPost, "/input/key", `{"keys": ["ctrl+l", "a", "Return"], "pace": "instant"}`)
	args := c.calls[0].args
	want := "key --clearmodifiers --delay 50 -- ctrl+l sleep 0.150 key --clearmodifiers --delay 50 -- a Return"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("args %q\nwant %q", got, want)
	}
	// Each key is still a keysym and no command; every sleep is the constant.
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "key":
			i += 4 // --clearmodifiers --delay 50 --
		case "sleep":
			i++
			if args[i] != secs(shortcutGap) {
				t.Errorf("sleep %s, want %s", args[i], secs(shortcutGap))
			}
		default:
			for _, k := range strings.Split(args[i], "+") {
				if !keysymRE.MatchString(k) || xdotoolCommand(k) {
					t.Errorf("%q in the arguments isn't a key", k)
				}
			}
		}
	}
	// The list ended on Return: the next call doesn't wait.
	c.do(t, http.MethodPost, "/input/type", `{"text": "x", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Fatalf("a call after a list ending on Return waited %v", c.slept)
	}
	// At human pace the pause after a bound combination is the gap at least.
	c.calls = nil
	c.do(t, http.MethodPost, "/input/key", `{"keys": ["ctrl+l", "a", "ctrl+c", "b"]}`)
	pauses := 0
	h := c.calls[0].args
	for i := 0; i+1 < len(h); i++ {
		if h[i] == "keyup" && i+3 < len(h) && h[i+2] == "sleep" {
			d, err := strconv.ParseFloat(h[i+3], 64)
			if err != nil {
				t.Fatal(err)
			}
			if bound := browserBound(h[i+1]); bound && time.Duration(d*float64(time.Second)) < shortcutGap {
				t.Errorf("after %s the pause is %vs", h[i+1], d)
			}
			pauses++
		}
	}
	if pauses != 3 {
		t.Fatalf("%d pauses in %q", pauses, h)
	}
}

func TestPointerActionsDontWaitForTheGap(t *testing.T) {
	for _, step := range [][3]string{
		{http.MethodPost, "/input/click", `{"x": 10, "y": 10, "pace": "instant"}`},
		{http.MethodPost, "/input/click", `{"x": 10, "y": 10}`},
		{http.MethodPost, "/input/scroll", `{"x": 10, "y": 10, "dy": 1, "pace": "instant"}`},
		{http.MethodPost, "/input/scroll", `{"x": 10, "y": 10, "dy": 1}`},
		{http.MethodPost, "/input/drag", `{"from": {"x": 1, "y": 1}, "to": {"x": 20, "y": 20}, "pace": "instant"}`},
		{http.MethodPost, "/input/drag", `{"from": {"x": 1, "y": 1}, "to": {"x": 20, "y": 20}}`},
		{http.MethodGet, "/screenshot", ""},
	} {
		c := newClockedVisual()
		c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
		c.do(t, step[0], step[1], step[2])
		if len(c.slept) != 0 {
			t.Errorf("%s %s right after ctrl+l waited %v", step[1], step[2], c.slept)
		}
	}
}

func TestAWaitEndsWithItsRequest(t *testing.T) {
	c := newClockedVisual()
	c.do(t, http.MethodPost, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	waiting := make(chan struct{})
	c.sleep = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		mux := http.NewServeMux()
		c.register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/input/type", strings.NewReader(`{"text": "secret", "pace": "instant"}`)))
		done <- rec
	}()
	<-waiting
	cancel()
	select {
	case rec := <-done:
		if rec.Code == http.StatusNoContent {
			t.Errorf("a cancelled wait answered %d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler didn't return when its request ended")
	}
	if len(c.calls) != 1 {
		t.Fatalf("xdotool ran after the request ended: %v", c.calls[1:])
	}
	if !c.mu.TryLock() {
		t.Fatal("the mutex is still held")
	}
	c.mu.Unlock()
}

// An instant key run cut short lets go of its keys, as a human-paced one
// does: a list's pauses after shortcuts make it longer than it was.
func TestInstantKeysReleaseWhenCutShort(t *testing.T) {
	v, calls := newTestVisual(true)
	if rec := serveVisual(v, http.MethodPost, "/input/key", `{"keys": ["ctrl+l", "a"], "pace": "instant"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("key: %d %s", rec.Code, rec.Body)
	}
	if len(*calls) != 2 || strings.Join((*calls)[1].args, " ") != "keyup ctrl+l a" {
		t.Fatalf("calls %v", *calls)
	}
}

// A key list with a browser-bound combination before its last had its gap
// inside, but only if it ran to the end: cut short, or failing, after the
// shortcut may have reached the browser, it arms the gap for the next call.
func TestACutShortListWithAShortcutArmsTheGap(t *testing.T) {
	for _, pace := range []string{"instant", "human"} {
		c := newClockedVisual()
		run := c.run
		c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := run(ctx, name, args...)
			if name == "xdotool" && len(args) > 0 && args[0] != "keyup" && strings.Contains(strings.Join(args, " "), "ctrl+l") {
				return nil, context.Canceled // the request ended while the list ran
			}
			return out, err
		}
		if rec := serveVisual(c.visual, http.MethodPost, "/input/key", `{"keys": ["ctrl+l", "a"], "pace": "`+pace+`"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: key: %d %s", pace, rec.Code, rec.Body)
		}
		if last := c.calls[len(c.calls)-1].args; last[0] != "keyup" {
			t.Fatalf("%s: the keys weren't let go of: %v", pace, c.calls)
		}
		c.run = run
		c.do(t, http.MethodPost, "/input/type", `{"text": "example.com", "pace": "instant"}`)
		if len(c.slept) != 1 || c.slept[0] != shortcutGap {
			t.Errorf("%s: the retry after a cut-short list slept %v, want one %v", pace, c.slept, shortcutGap)
		}
	}
	// A list with no browser-bound combination doesn't arm it, cut short or not.
	c := newClockedVisual()
	run := c.run
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		_, _ = run(ctx, name, args...)
		return nil, context.Canceled
	}
	serveVisual(c.visual, http.MethodPost, "/input/key", `{"keys": ["a", "Return"], "pace": "instant"}`)
	c.run = run
	c.do(t, http.MethodPost, "/input/type", `{"text": "x", "pace": "instant"}`)
	if len(c.slept) != 0 {
		t.Errorf("a cut-short list of plain keys made the next call wait %v", c.slept)
	}
}

// An instant ASCII type killed part way (a Type a hand-back cuts short,
// an agent's type a Take control breaks off) lets go of every key it may
// hold, and Shift, which it holds for a shifted character (PROJ-196).
func TestInstantTypeCutShortLetsGoOfItsKeys(t *testing.T) {
	for target, body := range map[string]string{"/input/type": `{"text": "Hi! Hi", "pace": "instant"}`, "/input/live-type": `{"text": "Hi! Hi"}`} {
		v, calls := newTestVisual(false)
		v.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
			*calls = append(*calls, call{name, args})
			if len(args) > 0 && args[0] == "type" {
				return nil, errors.New("signal: killed")
			}
			return nil, nil
		}
		rec := serveVisual(v, http.MethodPost, target, body)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s killed: %d %s", target, rec.Code, rec.Body)
		}
		last := (*calls)[len(*calls)-1]
		if got := strings.Join(last.args, " "); got != "keyup H i exclam space Shift_L" {
			t.Fatalf("%s: the release after the kill is %q (calls %v)", target, got, *calls)
		}
	}
}

// The person's Type, ASCII included, drains the live view first: what it
// queued has run and a key it holds is let go, before the text is typed,
// so neither mixes with it (PROJ-196).
func TestPersonTypeDrainsTheLiveView(t *testing.T) {
	v, calls := newTestVisual(false)
	x := &fakeXdotool{}
	v.live.start = func() (*xdo, error) { return x.proc(), nil }
	if rec := post(v.live.events, `[{"t":"keydown","k":"Shift_L"}]`); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if rec := serveVisual(v, http.MethodPost, "/input/live-type", `{"text": "ab"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	if len(*calls) < 2 || strings.Join((*calls)[0].args, " ") != "keyup Shift_L" || (*calls)[1].args[0] != "type" {
		t.Fatalf("calls %v, want Shift let go before the type", *calls)
	}
	if len(v.live.held) != 0 || v.live.x != nil {
		t.Fatalf("held %v, live xdotool still running %v", v.live.held, v.live.x != nil)
	}
}
