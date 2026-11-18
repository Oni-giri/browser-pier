package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/godbus/dbus/v5"
)

// TestA11yConfigListensOnAPathOnly: the bus listens on one path socket in
// its private folder, never on TCP or an abstract socket.
func TestA11yConfigListensOnAPathOnly(t *testing.T) {
	dir := "/tmp/browserpier-a11y-1000"
	var conf struct {
		Listen []string `xml:"listen"`
		Auth   []string `xml:"auth"`
		Policy []struct {
			Allow []struct {
				User string `xml:"user,attr"`
			} `xml:"allow"`
		} `xml:"policy"`
	}
	raw := a11yConfig(dir)
	if err := xml.Unmarshal([]byte(raw[strings.Index(raw, "<busconfig>"):]), &conf); err != nil {
		t.Fatal(err)
	}
	if len(conf.Listen) != 1 || conf.Listen[0] != "unix:path="+dir+"/bus" {
		t.Fatalf("listens on %q", conf.Listen)
	}
	for _, l := range conf.Listen {
		if strings.Contains(l, "tcp:") || strings.Contains(l, "abstract=") || strings.Contains(l, "/run/browserpier") || !strings.HasPrefix(l, "unix:path="+dir+"/") {
			t.Errorf("listener %q", l)
		}
	}
	if len(conf.Auth) != 1 || conf.Auth[0] != "EXTERNAL" {
		t.Errorf("auth %q", conf.Auth)
	}
	for _, p := range conf.Policy {
		for _, a := range p.Allow {
			if a.User != "" {
				t.Errorf("the policy lets user %q connect", a.User)
			}
		}
	}
	if !strings.HasPrefix(a11yDir(1000), os.TempDir()) || strings.Contains(a11yDir(1000), "/run/browserpier") {
		t.Errorf("folder %s", a11yDir(1000))
	}
	if args := strings.Join(a11yArgs("/x/bus.conf"), " "); strings.Contains(args, "--address") || strings.Contains(args, "--fork ") || !strings.Contains(args, "--nofork") {
		t.Errorf("dbus-daemon %s", args)
	}
}

func TestPrepareA11yDirWantsAPrivateFolder(t *testing.T) {
	uid := os.Getuid()
	dir := filepath.Join(t.TempDir(), "a11y")
	if err := prepareA11yDir(dir, uid); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	if err := prepareA11yDir(dir, uid); err != nil {
		t.Fatalf("again: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareA11yDir(dir, uid); err == nil {
		t.Fatal("a folder others can enter: want an error")
	}
	if err := prepareA11yDir(dir, uid+1); err == nil {
		t.Fatal("another uid's folder: want an error")
	}
}

// TestStopBrowserClosesChromiumBeforeTheBus: Chromium is asked to close,
// and has exited, before the bus is stopped.
func TestStopBrowserClosesChromiumBeforeTheBus(t *testing.T) {
	chromium, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	busProc, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	bus := &a11yBus{proc: busProc}
	var busUpAtClose atomic.Bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s/devtools/browser/x"}`, srv.Listener.Addr())
		default:
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			if _, msg, err := conn.Read(r.Context()); err == nil && strings.Contains(string(msg), "Browser.close") {
				select {
				case <-busProc.done:
				default:
					busUpAtClose.Store(true)
				}
				// Chromium takes a moment to flush, then exits.
				go func() {
					time.Sleep(200 * time.Millisecond)
					_ = chromium.cmd.Process.Signal(syscall.SIGTERM)
				}()
			}
			_ = conn.CloseNow()
		}
	}))
	defer srv.Close()
	var chromiumGoneFirst atomic.Bool
	go func() {
		<-busProc.done
		select {
		case <-chromium.done:
			chromiumGoneFirst.Store(true)
		default:
		}
	}()
	if err := stopBrowser(chromium, srv.Listener.Addr().String(), 5*time.Second, bus); err != nil {
		t.Fatal(err)
	}
	<-busProc.done
	time.Sleep(10 * time.Millisecond)
	if !busUpAtClose.Load() {
		t.Error("the bus was stopped before Chromium was asked to close")
	}
	if !chromiumGoneFirst.Load() {
		t.Error("the bus was stopped before Chromium had exited")
	}
}

// TestBusExitingDoesNotEndTheBrowser: the bus dying is logged, and
// waitEnd goes on waiting for Chromium or a stop.
func TestBusExitingDoesNotEndTheBrowser(t *testing.T) {
	chromium, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	defer chromium.stop(syscall.SIGKILL, time.Second)
	busProc, err := spawn(exec.Command("true"))
	if err != nil {
		t.Fatal(err)
	}
	bus := &a11yBus{proc: busProc}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if waitEnd(ctx, slog.New(slog.DiscardHandler), chromium, bus) {
		t.Fatal("waitEnd says Chromium exited")
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("waitEnd returned as the bus exited")
	}
	// Chromium exiting still ends it, bus or no bus.
	go func() { _ = chromium.cmd.Process.Kill() }()
	if !waitEnd(context.Background(), slog.New(slog.DiscardHandler), chromium, nil) {
		t.Fatal("waitEnd missed Chromium exiting")
	}
}

// TestA11yBusRuns starts the real dbus-daemon, if this machine has one,
// in a private folder, and connects to it.
func TestA11yBusRuns(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("no dbus-daemon here")
	}
	t.Setenv("TMPDIR", t.TempDir())
	bus, err := startA11yBus(context.Background(), slog.New(slog.DiscardHandler), os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.stop()
	if fi, err := os.Stat(bus.dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder %v %v", fi, err)
	}
	c, err := dialBus(bus.address())
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	body, err := c.call(context.Background(), "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus.ListNames")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := dbus.Store(body, &names); err != nil || len(names) < 2 {
		t.Fatalf("names %v %v", names, err)
	}
	bus.stop()
	bus.stop() // twice is fine
	select {
	case <-bus.done():
	default:
		t.Fatal("still running")
	}
}
