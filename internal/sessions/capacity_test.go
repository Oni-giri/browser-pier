package sessions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// clock lets a test move the manager's time.
func (f *fixture) clock() func(time.Duration) {
	now := time.Unix(1_800_000_000, 0)
	f.m.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

func (f *fixture) profile(name string, autoStop time.Duration) {
	f.t.Helper()
	p := profiles.New(name)
	p.AutoStop = autoStop
	if _, err := f.m.Profiles().Create(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) startProfile(name string) Session {
	f.t.Helper()
	s, _, err := f.m.Start(context.Background(), name, "")
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestIdleBrowsersStop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	advance := f.clock()
	f.profile("idle", 5*time.Minute)
	f.profile("busy", 5*time.Minute)
	f.profile("never", 0)
	f.profile("handoff", 5*time.Minute)
	idle, busy, never, handoff := f.startProfile("idle"), f.startProfile("busy"), f.startProfile("never"), f.startProfile("handoff")
	_, release, err := f.m.Attach(ctx, busy.ID, "t1", "claude")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := f.m.SetNeedsHelp(handoff.ID, true); err != nil {
		t.Fatal(err)
	}

	advance(4 * time.Minute)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("idle"); !ok {
		t.Fatal("stopped before its auto-stop")
	}
	f.m.Touch(idle.ID) // used again: the clock restarts
	advance(4 * time.Minute)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("idle"); !ok {
		t.Fatal("Touch didn't count as use")
	}
	advance(2 * time.Minute)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("idle"); ok {
		t.Fatal("an idle browser past its auto-stop is still running")
	}
	if got := f.stored(idle.ID); got.State != Stopped {
		t.Fatalf("idle stop ended %s, want a clean stop", got.State)
	}
	for _, s := range []Session{busy, never, handoff} {
		if _, ok := f.m.Live(s.Profile); !ok {
			t.Errorf("%s was stopped (agent connected, never auto-stops, or a person has it)", s.Profile)
		}
	}
}

func TestMemoryLimitStopsTheLongestIdle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	advance := f.clock()
	const gib = 1 << 30
	f.rt.Memory = gib
	f.m.SetMemoryLimit(2*gib+gib/2, gib) // room for two
	for _, n := range []string{"a", "b", "c", "d"} {
		f.profile(n, 0)
	}
	a := f.startProfile("a")
	advance(time.Minute)
	b := f.startProfile("b")
	advance(time.Minute)

	// c doesn't fit: a has been idle longest, so it goes.
	c := f.startProfile("c")
	if _, ok := f.m.Live("a"); ok {
		t.Fatal("the longest-idle browser wasn't stopped to make room")
	}
	if got := f.stored(a.ID); got.State != Stopped {
		t.Fatalf("a ended %s, want a clean stop", got.State)
	}
	if _, ok := f.m.Live("b"); !ok {
		t.Fatal("stopped a newer browser than needed")
	}

	// With every browser in use, a new one is refused with what's running.
	for _, s := range []Session{b, c} {
		_, release, err := f.m.Attach(ctx, s.ID, "t-"+s.Profile, "agent-"+s.Profile)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	_, _, err := f.m.Start(ctx, "d", "")
	var full *NoRoomError
	if !errors.As(err, &full) || !errors.Is(err, ErrNoRoom) {
		t.Fatalf("Start with no room = %v", err)
	}
	if len(full.Running) != 2 || full.Running[0].Profile != "b" || !strings.Contains(full.Running[0].Why, "agent-b") {
		t.Fatalf("running = %+v", full.Running)
	}
	if strings.Contains(err.Error(), "agent-b") || strings.Contains(err.Error(), "c (") {
		t.Fatalf("the error message names profiles: %v", err)
	}

	// An unknown profile is refused before anyone is stopped for it.
	if _, _, err := f.m.Start(ctx, "nope", ""); !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("unknown profile = %v", err)
	}
	if len(f.m.List()) != 2 {
		t.Fatal("a browser was stopped for a profile that doesn't exist")
	}
	// Attaching to a running one needs no room.
	if _, attached, err := f.m.Start(ctx, "b", ""); err != nil || !attached {
		t.Fatalf("attach with no room: %t, %v", attached, err)
	}
}
