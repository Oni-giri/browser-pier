package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// TestChromiumEnvPointsAccessibilityAtTheSessionBus: in Visual mode
// Chromium's accessibility goes over the session bus, the one the portal
// is on; in API mode Chromium gets no accessibility variable at all.
func TestChromiumEnvPointsAccessibilityAtTheSessionBus(t *testing.T) {
	bus := &sessionBus{dir: "/tmp/browserpier-bus-1000"}
	addr := "unix:path=/tmp/browserpier-bus-1000/bus"
	api := chromiumEnv(nil, nil, bus, false)
	if slices.ContainsFunc(api, func(e string) bool {
		return strings.HasPrefix(e, "AT_SPI_") || strings.HasPrefix(e, "ACCESSIBILITY_")
	}) {
		t.Fatalf("API mode's environment has accessibility: %q", api)
	}
	vis := chromiumEnv(nil, nil, bus, true)
	if !slices.Equal(vis, append(slices.Clone(api), "AT_SPI_BUS_ADDRESS="+addr, "ACCESSIBILITY_ENABLED=1")) {
		t.Fatalf("Visual mode's environment: %q", vis)
	}
	if !slices.Contains(vis, "DBUS_SESSION_BUS_ADDRESS="+addr) {
		t.Fatalf("the accessibility bus isn't the session bus: %q", vis)
	}
	// No bus, no address of one.
	if slices.ContainsFunc(chromiumEnv(nil, nil, nil, true), func(e string) bool { return strings.HasPrefix(e, "AT_SPI_") }) {
		t.Fatal("an accessibility bus address with no bus")
	}
}

// TestTreeReaderSharesTheBusWithThePortal runs the real session bus, when
// the machine has one, with the portal on it and a stand-in for
// Chromium's accessibility bridge, which owns no well-known name: the
// tree reader finds the browser's application among the bus's
// connections (the portal's included) and reads it, under the bus's
// portal-only ownership rule.
func TestTreeReaderSharesTheBusWithThePortal(t *testing.T) {
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
	bus, err := startSessionBus(context.Background(), os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.stop()
	p, err := startPortal(slog.New(slog.NewTextHandler(io.Discard, nil)), bus.address(), time.Minute, noFocus{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()

	app, err := dbus.Connect(bus.address())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	if err := app.Export(appProps{}, appRoot, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	// The bridge's connection can't take a name of its own here: only the
	// portal's may be owned.
	if reply, err := app.RequestName("org.a11y.atspi.Registry", dbus.NameFlagDoNotQueue); err == nil && reply == dbus.RequestNameReplyPrimaryOwner {
		t.Fatal("a connection took a well-known name other than the portal's")
	}

	r := newTreeReader(bus.address(), 1280, 768)
	defer r.drop()
	if err := r.connect(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := r.findApp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != app.Names()[0] {
		t.Fatalf("found %s, want the application %s", got, app.Names()[0])
	}
}

// appProps answers Name on the application object, as Chromium's bridge
// does.
type appProps struct{}

func (appProps) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface == ifAccessible && name == "Name" {
		return dbus.MakeVariant("Chromium"), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(os.ErrNotExist)
}
