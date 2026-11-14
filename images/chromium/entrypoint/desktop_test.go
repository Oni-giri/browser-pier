package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// jwmrc is the window manager's configuration, as JWM reads it.
type jwmrc struct {
	Trays []struct {
		X        int    `xml:"x,attr"`
		Y        int    `xml:"y,attr"`
		Height   int    `xml:"height,attr"`
		Autohide string `xml:"autohide,attr"`
	} `xml:"Tray"`
	Groups []struct {
		Class   []string `xml:"Class"`
		Options []string `xml:"Option"`
	} `xml:"Group"`
	StartupCommands []string `xml:"StartupCommand"`
}

// jwm -p, which the image build runs, says nothing of malformed XML or a
// height that isn't a number, so jwmrc is checked here: well formed, the
// taskbar the window is sized around, a ready file for startup to wait
// for, and nothing on the desktop that starts a program or touches the
// browser's window.
func TestJWMConfig(t *testing.T) {
	b, err := os.ReadFile("../jwmrc")
	if err != nil {
		t.Fatal(err)
	}
	// Every token, not just the first element, so trailing junk fails too.
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("jwmrc isn't well-formed XML: %v", err)
		}
		if el, ok := tok.(xml.StartElement); ok {
			switch el.Name.Local {
			// Menus, key and mouse bindings and buttons start programs or
			// act on windows (a window menu has Kill).
			case "RootMenu", "Menu", "Program", "Key", "Mouse", "TrayButton", "Swallow", "Include", "RestartCommand", "ShutdownCommand":
				t.Errorf("jwmrc has <%s>", el.Name.Local)
			}
		}
	}
	var c jwmrc
	if err := xml.Unmarshal(b, &c); err != nil {
		t.Fatalf("jwmrc: %v", err)
	}
	if len(c.Trays) != 1 {
		t.Fatalf("jwmrc has %d trays, want 1", len(c.Trays))
	}
	// At the bottom, so the window stays at 0,0, and always shown, so it
	// reserves its strip.
	if tr := c.Trays[0]; tr.X != 0 || tr.Y != -1 || tr.Height != panelHeight || tr.Autohide != "off" {
		t.Errorf("tray %+v, want x 0, y -1 (the bottom), height %d (panelHeight) and autohide off", tr, panelHeight)
	}
	if len(c.StartupCommands) != 1 || !strings.Contains(c.StartupCommands[0], desktopReady) {
		t.Errorf("startup commands %q, want one that creates %s, which startup waits for", c.StartupCommands, desktopReady)
	}
	chromium := false
	for _, g := range c.Groups {
		if !slices.Contains(g.Class, "Chromium") {
			continue
		}
		chromium = true
		for _, want := range []string{"noborder", "notitle", "nolist", "nomove", "noresize"} {
			if !slices.Contains(g.Options, want) {
				t.Errorf("Chromium's window lacks %s", want)
			}
		}
	}
	if !chromium {
		t.Error("jwmrc has no group for Chromium's window")
	}
}

// A window manager that exits during the session is logged, not fatal;
// one stopped at the end isn't logged.
func TestWatchDesktopWarnsWhenTheWindowManagerExits(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	waitOver := func(over <-chan struct{}) {
		t.Helper()
		select {
		case <-over:
		case <-time.After(5 * time.Second):
			t.Fatal("the watch didn't end")
		}
	}

	crashed := &process{done: make(chan struct{})}
	over := watchDesktop(log, crashed, make(chan struct{}))
	close(crashed.done)
	waitOver(over)
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "window manager exited") {
		t.Errorf("no warning when the window manager exited: %q", buf.String())
	}

	buf.Reset()
	stopped := &process{done: make(chan struct{})}
	stopping := make(chan struct{})
	over = watchDesktop(log, stopped, stopping)
	close(stopping)
	close(stopped.done)
	waitOver(over)
	if buf.Len() != 0 {
		t.Errorf("a warning when the window manager was stopped on purpose: %q", buf.String())
	}
}
