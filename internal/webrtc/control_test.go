package webrtc

import (
	"log/slog"
	"strings"
	"testing"
	"time"
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
			if got := strings.Join(c.took(), " "); got != "empty" {
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
