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
