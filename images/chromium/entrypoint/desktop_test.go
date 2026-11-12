package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Chromium starts only once the window manager says it's up, and a window
// manager that dies or never gets there stops the start.
func TestStartDesktopWaitsForTheWindowManager(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	// A ready file left from before doesn't count.
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wm := exec.Command("sh", "-c", `sleep 0.3; touch "$0"; exec sleep 30`, ready)
	start := time.Now()
	p, err := startDesktop(context.Background(), wm, ready, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("went on after %v, before the window manager was ready", d)
	}
	if !p.stop(syscall.SIGTERM, 5*time.Second) {
		t.Error("the window manager didn't stop on SIGTERM")
	}

	if _, err := startDesktop(context.Background(), exec.Command("sh", "-c", "exit 1"), ready, 5*time.Second); err == nil {
		t.Error("a window manager that exited, and startup went on")
	}
	if _, err := startDesktop(context.Background(), exec.Command("sleep", "30"), ready, 200*time.Millisecond); err == nil {
		t.Error("a window manager that never got ready, and startup went on")
	}
}

func TestWindowManagerCommand(t *testing.T) {
	cmd := windowManager([]string{"TZ=Europe/Berlin"})
	if want := []string{"jwm", "-display", display, "-f", desktopConfig}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args %v, want %v", cmd.Args, want)
	}
	// The taskbar's clock shows the time zone pages see.
	if !slices.Contains(cmd.Env, "TZ=Europe/Berlin") || !slices.Contains(cmd.Env, "DISPLAY="+display) {
		t.Errorf("env %v lacks the location's TZ or the display", cmd.Env)
	}
}

// The taskbar's height in jwmrc is the one the window is sized around.
func TestJWMTrayHeightMatchesPanelHeight(t *testing.T) {
	b, err := os.ReadFile("../jwmrc")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`<Tray [^>]*\bheight="(\d+)"`).FindSubmatch(b)
	if m == nil || string(m[1]) != strconv.Itoa(panelHeight) {
		t.Errorf("jwmrc's tray height is %q, want panelHeight, %d", m, panelHeight)
	}
	if !strings.Contains(string(b), desktopReady) {
		t.Errorf("jwmrc doesn't create %s, so startup would wait for nothing", desktopReady)
	}
}
