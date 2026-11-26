package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFocusX answers xdotool's calls as a screen with a 1280x800 display:
// the browser's window 11 (stale if stale is set, found again as 12),
// the taskbar 7.
type fakeFocusX struct {
	mu      sync.Mutex
	focus   string
	stale   bool
	delay   time.Duration
	actions []string
}

func (x *fakeFocusX) output(_ context.Context, _ string, args ...string) ([]byte, error) {
	time.Sleep(x.delay)
	x.mu.Lock()
	defer x.mu.Unlock()
	switch strings.Join(args, " ") {
	case "getwindowfocus":
		return []byte(x.focus + "\n"), nil
	case "getwindowname 11", "getwindowname 12":
		return []byte("Page - Chromium\n"), nil
	case "search --onlyvisible --classname ":
		return []byte("7\n11\n"), nil
	case "getwindowgeometry --shell 7":
		return []byte("WINDOW=7\nX=0\nY=768\nWIDTH=1280\nHEIGHT=32\nSCREEN=0\n"), nil
	case "getwindowgeometry --shell 11":
		return []byte("WINDOW=11\nX=0\nY=0\nWIDTH=1280\nHEIGHT=768\nSCREEN=0\n"), nil
	case "windowfocus 7":
		x.focus = "7"
		x.actions = append(x.actions, "away")
		return nil, nil
	case "windowactivate 11":
		if x.stale {
			return nil, errors.New("BadWindow")
		}
		x.focus = "11"
		x.actions = append(x.actions, "back")
		return nil, nil
	case "search --onlyvisible --name Chromium$":
		return []byte("12\n"), nil
	case "windowactivate 12":
		x.focus = "12"
		x.actions = append(x.actions, "back afresh")
		return nil, nil
	}
	return nil, errors.New("unexpected xdotool " + strings.Join(args, " "))
}

func (x *fakeFocusX) state() (string, []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.focus, append([]string{}, x.actions...)
}

func testFocus(x *fakeFocusX) *xFocus {
	f := &xFocus{log: slog.New(slog.NewTextHandler(io.Discard, nil)), width: 1280, height: 800, output: x.output, wake: make(chan struct{}, 1)}
	go f.work()
	return f
}

func waitFocus(t *testing.T, x *fakeFocusX, want string) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if got, actions := x.state(); got == want {
			return actions
		}
		if time.Now().After(deadline) {
			got, actions := x.state()
			t.Fatalf("focus %s, want %s (did %q)", got, want, actions)
		}
	}
}

func TestFocusGoesAwayAndBack(t *testing.T) {
	x := &fakeFocusX{focus: "11"}
	f := testFocus(x)
	f.away()
	waitFocus(t, x, "7")
	f.back()
	if actions := waitFocus(t, x, "11"); strings.Join(actions, ",") != "away,back" {
		t.Fatalf("did %q", actions)
	}
}

func TestFocusComesBackToAStaleWindow(t *testing.T) {
	x := &fakeFocusX{focus: "11"}
	f := testFocus(x)
	f.away()
	waitFocus(t, x, "7")
	x.mu.Lock()
	x.stale = true
	x.mu.Unlock()
	f.back()
	waitFocus(t, x, "12")
}

func TestFocusBurstEndsBack(t *testing.T) {
	x := &fakeFocusX{focus: "11", delay: 5 * time.Millisecond}
	f := testFocus(x)
	for range 200 {
		f.away()
		f.back()
	}
	f.away()
	waitFocus(t, x, "7")
	f.back()
	waitFocus(t, x, "11")
	time.Sleep(100 * time.Millisecond)
	if got, _ := x.state(); got != "11" {
		t.Fatalf("focus %s after the burst", got)
	}
}

func TestFocusLeavesAnotherWindowAlone(t *testing.T) {
	x := &fakeFocusX{focus: "99"}
	f := testFocus(x)
	f.away()
	f.back()
	time.Sleep(100 * time.Millisecond)
	if got, actions := x.state(); got != "99" || len(actions) != 0 {
		t.Fatalf("focus %s, did %q", got, actions)
	}
}
