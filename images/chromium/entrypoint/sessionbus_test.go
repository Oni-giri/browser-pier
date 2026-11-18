package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/godbus/dbus/v5"
)

func TestBusConfigListensOnOnePathSocket(t *testing.T) {
	dir := "/tmp/browserpier-bus-1000"
	conf := busConfig(dir)
	listens := regexp.MustCompile(`<listen>([^<]*)</listen>`).FindAllStringSubmatch(conf, -1)
	if len(listens) != 1 || listens[0][1] != "unix:path="+dir+"/bus" {
		t.Fatalf("listen %q, want one unix:path= in %s", listens, dir)
	}
	if !strings.Contains(conf, `<allow own="org.freedesktop.portal.Desktop"/>`) || strings.Count(conf, "own=") != 1 {
		t.Errorf("the bus lets connections own more than the portal's name:\n%s", conf)
	}
	for _, bad := range []string{"tcp:", "abstract=", "<servicedir", "standard_session_servicedirs", "<include", "<allow user", "eavesdrop=\"true\"", "--fork", "<fork"} {
		if strings.Contains(conf, bad) {
			t.Errorf("the bus config has %s:\n%s", bad, conf)
		}
	}
	if !strings.Contains(conf, "<auth>EXTERNAL</auth>") {
		t.Error("the bus config doesn't name EXTERNAL authentication")
	}
	args := strings.Join(busArgs("/x/bus.conf"), " ")
	for _, bad := range []string{"--fork", "--system", "--session", "--address"} {
		if strings.Contains(strings.ReplaceAll(args, "--nofork", ""), bad) {
			t.Errorf("dbus-daemon args %q have %s", args, bad)
		}
	}
	for _, d := range []string{"/run/browserpier", "/files", "/profile"} {
		if strings.HasPrefix(busDir(1000), d) {
			t.Errorf("the bus folder %s is in %s", busDir(1000), d)
		}
	}
}

func TestBusEnvIsOnlyTheAddress(t *testing.T) {
	var none *sessionBus
	if none.env() != nil || none.address() != "" {
		t.Fatal("no bus has an environment")
	}
	b := &sessionBus{dir: "/tmp/browserpier-bus-1000"}
	if env := b.env(); len(env) != 1 || env[0] != "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/browserpier-bus-1000/bus" {
		t.Fatalf("env %q", env)
	}
	// The entrypoint's own environment, which xdotool and GStreamer
	// inherit, never has it (main.go gives it to Chromium's Env only).
	if _, ok := os.LookupEnv("DBUS_SESSION_BUS_ADDRESS"); ok {
		t.Skip("this test runs on a desktop with a session bus; the check is for the container")
	}
}

func TestChromiumEnvAloneHasTheBus(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/tmp/home"}
	bus := &sessionBus{dir: "/tmp/browserpier-bus-1000"}
	env := chromiumEnv(base, []string{"TZ=Europe/Riga"}, bus)
	if !slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/browserpier-bus-1000/bus") || !slices.Contains(env, "DISPLAY=:99") || !slices.Contains(env, "TZ=Europe/Riga") {
		t.Fatalf("Chromium's env %q", env)
	}
	if len(base) != 2 || slices.ContainsFunc(base, func(e string) bool { return strings.HasPrefix(e, "DBUS_") }) {
		t.Fatalf("the entrypoint's own environment changed: %q", base)
	}
	// Off (no bus): nothing about a bus at all.
	if slices.ContainsFunc(chromiumEnv(base, nil, nil), func(e string) bool { return strings.HasPrefix(e, "DBUS_") }) {
		t.Fatal("a bus address with no bus")
	}
}

func TestChromiumArgsHaveNoPortalSwitch(t *testing.T) {
	for _, a := range chromiumArgs(config{Width: 1280, Height: 800, ProfileDir: "/profile"}, false) {
		if strings.Contains(a, "portal") || strings.Contains(a, "dbus") {
			t.Errorf("chromium arg %s: the portal needs no switch", a)
		}
	}
}

func TestPrepareBusDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bus")
	if err := prepareBusDir(dir, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %s", fi.Mode())
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareBusDir(dir, os.Getuid()); err == nil {
		t.Fatal("a 0755 folder was used")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := prepareBusDir(link, os.Getuid()); err == nil {
		t.Fatal("a link was used as the folder")
	}
}

// TestStopBrowserClosesChromiumBeforeTheBus: Chromium aborts when its
// bus goes, so it must have closed (and flushed its cookies) first.
func TestStopBrowserClosesChromiumBeforeTheBus(t *testing.T) {
	browser, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	busProc, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
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
			_, _, _ = conn.Read(r.Context())
			note("Browser.close")
			// Chromium takes a moment to close; the bus must still be up.
			go func() {
				time.Sleep(200 * time.Millisecond)
				select {
				case <-busProc.done:
					note("bus gone before the browser")
				default:
				}
				_ = browser.cmd.Process.Kill()
				<-browser.done
				note("browser exited")
			}()
			_ = conn.CloseNow()
		}
	}))
	defer srv.Close()
	bus := &sessionBus{dir: t.TempDir(), proc: busProc}
	go func() { <-busProc.done; note("bus exited") }()
	_ = stopBrowser(browser, srv.Listener.Addr().String(), 5*time.Second, nil, bus)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ", ") != "Browser.close, browser exited, bus exited" {
		t.Fatalf("order: %v", order)
	}
}

// TestBusExitingDoesNotEndTheWait: the entrypoint doesn't stop Chromium
// over the bus; it logs it and waits for the browser as before.
func TestBusExitingDoesNotEndTheWait(t *testing.T) {
	browser, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	defer browser.stop(9, time.Second)
	busProc, err := spawn(exec.Command("true"))
	if err != nil {
		t.Fatal(err)
	}
	bus := &sessionBus{proc: busProc}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if stopped := waitEnd(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), browser, bus); !stopped {
		t.Fatal("the wait ended as if Chromium had exited")
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("the bus exiting ended the wait")
	}
	select {
	case <-browser.done:
		t.Fatal("the browser was stopped")
	default:
	}
	if !busGone(bus) {
		t.Fatal("busGone after the bus exited")
	}
}

// TestPortalOnARealBus runs the portal on a real dbus-daemon, when the
// machine has one: the name, the properties, refusals of other
// interfaces and of strangers, and an answer as Chromium receives it.
func TestPortalOnARealBus(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("no dbus-daemon here")
	}
	// A short folder: a socket path must fit in 108 bytes.
	tmp, err := os.MkdirTemp("/tmp", "bpbus")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	t.Setenv("TMPDIR", tmp)
	ctx := context.Background()
	bus, err := startSessionBus(ctx, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.stop()
	if fi, err := os.Stat(busDir(os.Getuid())); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("bus folder: %v %v", fi, err)
	}
	p, err := startPortal(slog.New(slog.NewTextHandler(io.Discard, nil)), bus.address(), time.Minute, noFocus{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	dir := t.TempDir()
	p.chooser.uploads = dir
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := dbus.Connect(bus.address())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	obj := client.Object(portalName, portalPath)

	// Nobody else gets the name.
	if r, err := client.RequestName(portalName, dbus.NameFlagReplaceExisting|dbus.NameFlagDoNotQueue); err != nil || r == dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("another connection took the portal's name: %v %v", r, err)
	}
	var v dbus.Variant
	if err := obj.Call(propertiesIntf+".Get", 0, fileChooserIntf, "version").Store(&v); err != nil || v.Value() != portalVersion {
		t.Fatalf("version: %v %v", v, err)
	}
	var e dbus.Error
	for _, call := range []struct {
		method string
		args   []any
	}{
		{"org.freedesktop.portal.Settings.Read", []any{"org.freedesktop.appearance", "color-scheme"}},
		{"org.freedesktop.host.portal.Registry.Register", []any{"org.chromium.Chromium", map[string]dbus.Variant{}}},
		{propertiesIntf + ".Get", []any{"org.freedesktop.portal.Notification", "version"}},
	} {
		if err := obj.Call(call.method, 0, call.args...).Err; !errors.As(err, &e) || e.Name != "org.freedesktop.DBus.Error.UnknownInterface" {
			t.Errorf("%s: %v, want UnknownInterface", call.method, err)
		}
	}
	if err := obj.Call(fileChooserIntf+".Frobnicate", 0).Err; !errors.As(err, &e) || e.Name != "org.freedesktop.DBus.Error.UnknownMethod" {
		t.Errorf("an unknown method: %v", err)
	}

	open := func() (dbus.ObjectPath, error) {
		var path dbus.ObjectPath
		err := obj.Call(fileChooserIntf+".OpenFile", 0, "x11:0x1", "Open File", map[string]dbus.Variant{"handle_token": dbus.MakeVariant("tok1")}).Store(&path)
		return path, err
	}
	// Before the browser is named, and as a stranger: refused.
	if _, err := open(); err == nil {
		t.Fatal("a chooser was held with no browser")
	}
	other, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.stop(9, time.Second)
	p.setBrowser(other.pid)
	if _, err := open(); err == nil {
		t.Fatal("a process that isn't under the browser was answered")
	}
	// This test's process as the browser: its own connection may ask.
	p.setBrowser(os.Getpid())
	if err := client.AddMatchSignal(dbus.WithMatchInterface(requestIntf)); err != nil {
		t.Fatal(err)
	}
	signals := make(chan *dbus.Signal, 4)
	client.Signal(signals)
	path, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(path), "/tok1") {
		t.Fatalf("request path %s", path)
	}
	list := p.chooser.list()
	if len(list) != 1 {
		t.Fatalf("open choosers %+v", list)
	}
	if err := p.chooser.answer(list[0].ID, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-signals:
		if s.Path != path || s.Name != requestIntf+".Response" || s.Body[0] != uint32(0) {
			t.Fatalf("signal %+v", s)
		}
		res, _ := s.Body[1].(map[string]dbus.Variant)
		uris, _ := res["uris"].Value().([]string)
		if len(uris) != 1 || uris[0] != "file://"+dir+"/a.txt" {
			t.Fatalf("uris %v", uris)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Response")
	}
	// Close from the caller ends a request; the caller leaving the bus too.
	path, err = open()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Object(portalName, path).Call(requestIntf+".Close", 0).Err; err != nil {
		t.Fatal(err)
	}
	if n := len(p.chooser.list()); n != 0 {
		t.Fatalf("%d open after Close", n)
	}
	if _, err := open(); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(p.chooser.list()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a request outlived its caller's connection")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestNoPortalStopsTheBus (PROJ-181): with the bus up but the portal's
// name already taken, startChooser fails, so the browser doesn't start
// with Chromium's own dialog, and the bus it started is stopped.
func TestNoPortalStopsTheBus(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("no dbus-daemon here")
	}
	tmp, err := os.MkdirTemp("/tmp", "bpbus")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	t.Setenv("TMPDIR", tmp)
	var addr string
	var rival *dbus.Conn
	takenFirst := func(log *slog.Logger, a string, timeout time.Duration, focus focuser) (*portal, error) {
		addr = a
		c, err := dbus.Connect(a)
		if err != nil {
			t.Fatal(err)
		}
		rival = c
		if reply, err := c.RequestName(portalName, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
			t.Fatalf("the rival couldn't take the name: %v %v", reply, err)
		}
		return startPortal(log, a, timeout, focus)
	}
	bus, p, err := startChooserWith(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), config{ChooserTimeout: time.Minute, Width: 800, Height: 600}, takenFirst)
	if rival != nil {
		defer func() { _ = rival.Close() }()
	}
	if err == nil || bus != nil || p != nil || !strings.Contains(err.Error(), "the file chooser portal: own "+portalName) {
		t.Fatalf("startChooser = %v, %v, %v; want an error naming the portal's name", bus, p, err)
	}
	// The bus went with it.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		c, err := dbus.Connect(addr)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the session bus still runs after the portal failed")
		}
	}
}
