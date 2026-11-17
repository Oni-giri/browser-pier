package sessions

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/notify"
)

// failures collects the browser.failed events queued for the webhook.
type failures struct {
	mu     sync.Mutex
	bodies []string
}

func (c *failures) add(_ notify.Kind, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, string(b))
}

func (c *failures) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// tellFailures wires m to a notifier with browser.failed on, as main does.
func tellFailures(t *testing.T, m *Manager) *failures {
	t.Helper()
	c := &failures{}
	n, err := notify.New(context.Background(), m.db, slog.New(slog.DiscardHandler), notify.Options{
		Lookup:  func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil },
		OnQueue: c.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Save(context.Background(), notify.Change{URL: "https://hooks.example.com/x", On: map[notify.Kind]bool{notify.BrowserFailed: true}}); err != nil {
		t.Fatal(err)
	}
	m.OnFailed(func(s Session, how Failure) { n.BrowserFailed(s.Profile, string(how)) })
	return c
}

const pwned = `level=ERROR msg=exiting err="PWNED <!channel>"`

// expectFailure checks one browser.failed of the category was queued,
// holding nothing of the session's error or log.
func expectFailure(t *testing.T, c *failures, category string) {
	t.Helper()
	eventually(t, "a browser.failed", func() bool { return len(c.all()) > 0 })
	got := c.all()
	if len(got) != 1 {
		t.Fatalf("queued %d events: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"category":"`+category+`"`) || !strings.Contains(got[0], `"profile":"google"`) {
		t.Errorf("want category %s: %s", category, got[0])
	}
	for _, bad := range []string{"PWNED", "channel", "exit status", "unexpectedly (", "level=", "no such"} {
		if strings.Contains(got[0], bad) {
			t.Errorf("the body holds %q: %s", bad, got[0])
		}
	}
}

func TestAFailedStartTells(t *testing.T) {
	f := newFixture(t)
	c := tellFailures(t, f.m)
	f.rt.StartErrs = []error{errors.New("PWNED <!channel>")}
	f.rt.LastLog = pwned
	if _, _, err := f.m.Start(context.Background(), "google", ""); !errors.Is(err, ErrStartFailed) {
		t.Fatalf("Start: %v", err)
	}
	expectFailure(t, c, "start_failed")
}

func TestACrashTells(t *testing.T) {
	f := newFixture(t)
	c := tellFailures(t, f.m)
	s := f.start(f.m)
	f.rt.LastLog = pwned
	f.rt.Exit(s.ContainerID, 1)
	expectFailure(t, c, "crashed")
}

func TestAnUncleanCloseTells(t *testing.T) {
	f := newFixture(t)
	c := tellFailures(t, f.m)
	s := f.start(f.m)
	f.rt.StopStatus = 1
	f.rt.LastLog = pwned
	if got, _ := f.m.Stop(context.Background(), s.ID); got.State != Failed {
		t.Fatalf("Stop: %s", got.State)
	}
	expectFailure(t, c, "unclean_close")
}

func TestALostContainerTells(t *testing.T) {
	f := newFixture(t)
	c := tellFailures(t, f.m)
	s := f.start(f.m)
	f.m.mu.Lock()
	l := f.m.live["google"]
	f.m.mu.Unlock()
	f.rt.LastLog = pwned
	f.m.finish(l, s.ContainerID, -1, errors.New("no such container: PWNED"))
	expectFailure(t, c, "lost")
}

func TestGoneAtReconcileTells(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.m)
	f.m.Close()
	f.rt.Exit(s.ContainerID, 0)
	if err := f.rt.Remove(ctx, s.ContainerID); err != nil {
		t.Fatal(err)
	}
	m := f.manager()
	c := tellFailures(t, m)
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	expectFailure(t, c, "lost")
}

func TestStopsAndShutdownsTellNothing(t *testing.T) {
	f := newFixture(t)
	c := tellFailures(t, f.m)
	s := f.start(f.m)
	if got, err := f.m.Stop(context.Background(), s.ID); err != nil || got.State != Stopped {
		t.Fatalf("Stop: %+v, %v", got, err)
	}
	// BrowserPier shutting down as a start begins.
	f.m.goMu.Lock()
	f.m.closed = true
	f.m.goMu.Unlock()
	if _, _, err := f.m.Start(context.Background(), "google", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start while closing: %v", err)
	}
	last, err := f.m.LastSession(context.Background(), "google")
	if err != nil {
		t.Fatal(err)
	}
	if last.State != Failed || last.Error != "BrowserPier was shutting down" {
		t.Fatalf("the last session: %+v", last)
	}
	if got := c.all(); len(got) != 0 {
		t.Errorf("queued %v", got)
	}
}
