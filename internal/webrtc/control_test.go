package webrtc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// A live view whose control ended without it (a handoff ended, the
// profile page's Hand back from another tab) and someone else took the
// browser since: whatever it does next, it never ends or disturbs that
// takeover. No lift, input or clipboard call of its own reaches the
// browser, the clipboard isn't emptied under the new person, and its page
// is told it has no control (PROJ-196).
func TestStaleViewerLeavesTheNextTakeoverAlone(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	sock := m.Layout().ControlSocket("google")
	const bSecret = "b-Secret-77"

	for _, tc := range []struct {
		name string
		// handoff: A is a handoff's viewer, which its handoff's end
		// cuts off (OfferOptions.Allowed).
		handoff bool
		act     func(t *testing.T, a *talk)
	}{
		{"lift then disconnect", false, func(t *testing.T, a *talk) {
			if r := a.send(`{"type":"lift"}`); len(r) != 1 || r[0]["type"] != "control" || r[0]["on"] != false {
				t.Errorf("lift: %v, want control off", r)
			}
			a.v.handBack()
		}},
		{"disconnect", false, func(t *testing.T, a *talk) { a.v.handBack() }},
		{"input", false, func(t *testing.T, a *talk) {
			if r := a.send(`{"type":"input","events":[{"type":"key","key":"a"}]}`); len(r) != 1 || r[0]["type"] != "control" || r[0]["on"] != false {
				t.Errorf("input: %v, want control off", r)
			}
		}},
		{"text", false, func(t *testing.T, a *talk) {
			if r := a.send(`{"type":"text","text":"typed by A"}`); len(r) != 1 || r[0]["on"] != false {
				t.Errorf("text: %v, want control off", r)
			}
		}},
		{"clipboard read", false, func(t *testing.T, a *talk) {
			if r := a.send(`{"type":"clipboard","id":1}`); len(r) != 2 || r[0]["type"] != "clipboard-error" || r[1]["on"] != false {
				t.Errorf("clipboard read: %v, want refused and control off", r)
			}
		}},
		{"clipboard set", false, func(t *testing.T, a *talk) {
			if r := a.send(set(1, "from A", true)); len(r) != 2 || r[0]["type"] != "clipboard-error" || r[1]["on"] != false {
				t.Errorf("clipboard set: %v, want refused and control off", r)
			}
		}},
		{"hand back", false, func(t *testing.T, a *talk) {
			if r := a.send(`{"type":"release"}`); len(r) != 1 || r[0]["on"] != false {
				t.Errorf("release: %v, want control off", r)
			}
		}},
		{"a handoff's viewer after it ended", true, func(t *testing.T, a *talk) {
			a.v.checkedAt = time.Time{} // Allowed is asked at once, not after a second
			a.send(`{"type":"lift"}`)   // cut off: Allowed says no
			a.v.handBack()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open := true
			va := &viewer{h: h, session: s.ID, sock: sock}
			if tc.handoff {
				va.allowed = func() bool { return open }
			}
			a := newTalk(t, va)
			if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
				t.Fatalf("A's take: %v", r)
			}
			// A's control ends from outside its live view.
			m.ReleaseControl(s.ID)
			open = false
			if got := strings.Join(c.took(), " "); got != "release empty" {
				t.Fatalf("as A's control ended, the browser got %q", got)
			}
			// B takes the browser and puts something on its clipboard.
			b := newTalk(t, &viewer{h: h, session: s.ID, sock: sock})
			if r := b.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
				t.Fatalf("B's take: %v", r)
			}
			if r := b.send(set(1, bSecret, false)); len(r) != 1 || r[0]["type"] != "clipboard-set" {
				t.Fatalf("B's set: %v", r)
			}
			c.took()

			tc.act(t, a)

			if got := c.took(); len(got) != 0 {
				t.Errorf("A's stale control reached the browser: %v", got)
			}
			if got, _ := m.Live("google"); !got.HumanControl {
				t.Error("B lost control")
			}
			c.mu.Lock()
			if c.held != bSecret {
				t.Errorf("B's clipboard holds %q", c.held)
			}
			c.mu.Unlock()
			// B still drives, and hands back as usual.
			if r := b.send(`{"type":"input","events":[]}`); len(r) != 0 {
				t.Errorf("B's input: %v", r)
			}
			if r := b.send(`{"type":"release"}`); len(r) != 1 || r[0]["on"] != false {
				t.Errorf("B's release: %v", r)
			}
			if got := strings.Join(c.took(), " "); got != "events release empty" {
				t.Errorf("B's input and hand-back: the browser got %q", got)
			}
			if got, _ := m.Live("google"); got.HumanControl {
				t.Fatal("still under a person's control")
			}
		})
	}
}

// A browser that is stopping refuses the person's input, and their live
// view is told it has no control: their control ends with it, so if the
// stop fails the browser is the agents' again, its clipboard emptied
// first, not left under a control no one holds (PROJ-200).
func TestInputRefusedWhileStoppingEndsControl(t *testing.T) {
	var c fakeClipboard
	h, m, s, rt := offerFixtureRuntime(t, c.routes, slog.New(slog.DiscardHandler))
	a := newTalk(t, &viewer{h: h, session: s.ID, sock: m.Layout().ControlSocket("google")})
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	if r := a.send(set(1, "a secret", false)); len(r) != 1 || r[0]["type"] != "clipboard-set" {
		t.Fatalf("set: %v", r)
	}
	c.took()
	rt.Mu.Lock()
	rt.StopErr = errors.New("the runtime refused to stop it")
	rt.BeforeStop = func(string) {
		if r := a.send(`{"type":"input","events":[]}`); len(r) != 1 || r[0]["type"] != "control" || r[0]["on"] != false {
			t.Errorf("input while stopping: %v, want control off", r)
		}
	}
	rt.Mu.Unlock()
	if _, err := m.Stop(context.Background(), s.ID); err == nil {
		t.Fatal("the stop didn't fail")
	}
	if snap, _ := m.Live("google"); snap.State != sessions.Running || snap.HumanControl {
		t.Fatalf("after the failed stop: %s, person in control %v", snap.State, snap.HumanControl)
	}
	// What the person held was let go, and the clipboard emptied before
	// agents had the browser back.
	if got := strings.Join(c.took(), " "); got != "release empty" {
		t.Errorf("the browser got %q, want release, then empty", got)
	}
	if _, done, err := m.AgentInput(context.Background(), s.ID); err != nil {
		t.Fatalf("agent input after the failed stop = %v", err)
	} else {
		done()
	}
	a.v.handBack() // the live view goes: nothing more to end
	if got := c.took(); len(got) != 0 {
		t.Errorf("the gone viewer reached the browser: %v", got)
	}
}

// A handoff that ends during a drag (it expires, say) lets go of the
// mouse button before anyone else drives: whoever ends the person's
// control lifts what they held, not only their own live view (PROJ-196).
func TestControlEndedElsewhereLetsGoOfHeldButtons(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	sock := m.Layout().ControlSocket("google")
	a := newTalk(t, &viewer{h: h, session: s.ID, sock: sock})
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	if r := a.send(`{"type":"input","events":[{"t":"down","b":1}]}`); len(r) != 0 {
		t.Fatalf("mouse down: %v", r)
	}
	m.ReleaseControl(s.ID) // the handoff expires
	a.v.handBack()         // and its page goes
	if got := strings.Join(c.took(), " "); got != "events release empty" {
		t.Errorf("the browser got %q, want the button let go before the clipboard is emptied", got)
	}
}

// A take from a live view whose control ended elsewhere is a take afresh,
// answered by the manager, not by the view's own memory of having had
// control (PROJ-196).
func TestRepeatedTakeAsksWhoHasControl(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	sock := m.Layout().ControlSocket("google")
	a := newTalk(t, &viewer{h: h, session: s.ID, sock: sock})
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take again, still in control: %v", r)
	}
	m.ReleaseControl(s.ID)
	if err := m.TakeControl(s.ID); err != nil { // someone else
		t.Fatal(err)
	}
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["type"] != "error" {
		t.Errorf("take while someone else has control: %v, want an error", r)
	}
	m.ReleaseControl(s.ID)
	c.took()
	// Free again: A takes it for real, and drives.
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take once free: %v", r)
	}
	if got, _ := m.Live("google"); !got.HumanControl {
		t.Fatal("told it has control, but no one has")
	}
	if r := a.send(`{"type":"input","events":[]}`); len(r) != 0 {
		t.Errorf("input: %v", r)
	}
	if got := strings.Join(c.took(), " "); got != "events" {
		t.Errorf("the browser got %q", got)
	}
}
