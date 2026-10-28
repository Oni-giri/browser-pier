package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	rec := serveVisual(v, http.MethodPost, "/input/click", `{"x": 40, "y": 50}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("click: %d %s", rec.Code, rec.Body)
	}
	if c := (*calls)[0]; c.name != "xdotool" || strings.Join(c.args, " ") != "mousemove --sync 40 50 click 1" {
		t.Fatalf("call %+v", c)
	}
	serveVisual(v, http.MethodPost, "/input/click", `{"x": 1, "y": 2, "button": "right", "double": true}`)
	if got := strings.Join((*calls)[1].args, " "); got != "mousemove --sync 1 2 click --repeat 2 --delay 80 3" {
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
	if rec := serveVisual(v, http.MethodPost, "/input/type", `{"text": "--version hello\n"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("type: %d %s", rec.Code, rec.Body)
	}
	args := (*calls)[0].args
	if args[len(args)-2] != "--" || args[len(args)-1] != "--version hello\n" {
		t.Fatalf("text not passed after --: %q", args)
	}
	long := `{"text": "` + strings.Repeat("é", maxTypeLen+1) + `"}`
	for _, body := range []string{`{"text": ""}`, long} {
		if rec := serveVisual(v, http.MethodPost, "/input/type", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%.30s: %d", body, rec.Code)
		}
	}
}

func TestScroll(t *testing.T) {
	v, calls := newTestVisual(false)
	for body, want := range map[string]string{
		`{"x": 640, "y": 400, "dy": 3}`:         "mousemove --sync 640 400 click --repeat 3 --delay 30 5",
		`{"x": 1, "y": 2, "dx": -2, "dy": -1}`:  "mousemove --sync 1 2 click --repeat 1 --delay 30 4 click --repeat 2 --delay 30 6",
		`{"x": 1, "y": 2, "dx": 99}`:            "mousemove --sync 1 2 click --repeat 20 --delay 30 7",
		`{"x": 1279, "y": 799, "dy": -1000000}`: "mousemove --sync 1279 799 click --repeat 20 --delay 30 4",
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
		if rec := serveVisual(v, http.MethodPost, "/input/key", body); rec.Code != http.StatusNoContent {
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
	if rec := serveVisual(v, http.MethodPost, "/input/drag", `{"from": {"x": 100, "y": 200}, "to": {"x": 180, "y": 120}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("drag: %d %s", rec.Code, rec.Body)
	}
	got := strings.Join((*calls)[0].args, " ")
	want := "mousemove --sync 100 200 sleep 0.05 mousedown 1" +
		" sleep 0.02 mousemove --sync 110 190 sleep 0.02 mousemove --sync 120 180 sleep 0.02 mousemove --sync 130 170" +
		" sleep 0.02 mousemove --sync 140 160 sleep 0.02 mousemove --sync 150 150 sleep 0.02 mousemove --sync 160 140" +
		" sleep 0.02 mousemove --sync 170 130 sleep 0.02 mousemove --sync 180 120 sleep 0.05 mouseup 1"
	if (*calls)[0].name != "xdotool" || got != want {
		t.Fatalf("args\n%q\nwant\n%q", got, want)
	}
	serveVisual(v, http.MethodPost, "/input/drag", `{"from": {"x": 0, "y": 0}, "to": {"x": 1279, "y": 799}, "button": "right"}`)
	if got := strings.Join((*calls)[1].args, " "); !strings.Contains(got, "mousedown 3") || !strings.HasSuffix(got, "mousemove --sync 1279 799 sleep 0.05 mouseup 3") {
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
