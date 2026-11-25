package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

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

// pionViewer is a live view's peer connection, with a data channel for
// each label (live.js opens one, "input"), and the offer to send.
type pionViewer struct {
	pc  *pion.PeerConnection
	dcs map[string]*pion.DataChannel
	// opened is closed once every data channel is open.
	opened chan struct{}
	body   string
}

func newPionViewer(t *testing.T, labels ...string) *pionViewer {
	t.Helper()
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	pv := &pionViewer{pc: pc, dcs: map[string]*pion.DataChannel{}, opened: make(chan struct{})}
	var wg sync.WaitGroup
	for _, label := range labels {
		dc, err := pc.CreateDataChannel(label, nil)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		var once sync.Once
		dc.OnOpen(func() { once.Do(wg.Done) })
		pv.dcs[label] = dc
	}
	go func() { wg.Wait(); close(pv.opened) }()
	sdp, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(sdp); err != nil {
		t.Fatal(err)
	}
	<-gathered
	b, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP})
	pv.body = string(b)
	return pv
}

// connect sends the offer with o and completes the connection.
func (pv *pionViewer) connect(t *testing.T, h *Handler, o OfferOptions) {
	t.Helper()
	rec := offer(h, o, pv.body)
	var answer struct {
		SDP     string `json:"sdp"`
		Control bool   `json:"control"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &answer) != nil || answer.Control != o.Control {
		t.Fatalf("offer: %d %s", rec.Code, rec.Body)
	}
	if err := pv.pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pv.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the data channels never opened")
	}
}

// awaitHuman waits for the profile's person in control to be want.
func awaitHuman(t *testing.T, m *sessions.Manager, want bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got, _ := m.Live("google")
		if got.HumanControl == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("person in control %v after %s, want %v", got.HumanControl, within, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Closing the live view's input channel ends its control, though the
// video stays connected: no message can come any more to drive the
// browser or hand it back (PROJ-196).
func TestClosingTheInputChannelHandsBack(t *testing.T) {
	var c fakeClipboard
	h, m, _ := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	pv := newPionViewer(t, InputLabel)
	pv.connect(t, h, OfferOptions{Control: true})
	awaitHuman(t, m, true, time.Second)
	if err := pv.dcs[InputLabel].Close(); err != nil {
		t.Fatal(err)
	}
	awaitHuman(t, m, false, 5*time.Second)
	if got, _ := m.Live("google"); got.Viewers != 1 {
		t.Errorf("viewers %d: the video should still be connected", got.Viewers)
	}
	if pv.pc.ConnectionState() != pion.PeerConnectionStateConnected {
		t.Errorf("the connection is %s", pv.pc.ConnectionState())
	}
}

// Only the input channel drives or hands back: another data channel the
// page opens is closed unheard, and its closing leaves control with the
// input channel (PROJ-196).
func TestOnlyTheInputChannelCounts(t *testing.T) {
	var c fakeClipboard
	h, m, _ := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	pv := newPionViewer(t, InputLabel, "aux")
	aux := pv.dcs["aux"]
	auxClosed := make(chan struct{})
	aux.OnClose(func() { close(auxClosed) })
	pv.connect(t, h, OfferOptions{Control: true})
	awaitHuman(t, m, true, time.Second)
	// What it sends isn't heard, and the server closes it; its closing
	// (from either side) hands nothing back.
	_ = aux.SendText(`{"type":"release"}`)
	select {
	case <-auxClosed:
	case <-time.After(2 * time.Second):
		t.Error("the other channel was left open")
		_ = aux.Close()
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := m.Live("google"); !got.HumanControl {
		t.Fatal("the other channel handed back the input channel's control")
	}
	// The input channel still drives.
	c.took()
	if err := pv.dcs[InputLabel].SendText(`{"type":"input","events":[]}`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for strings.Join(c.took(), " ") != "events" {
		if time.Now().After(deadline) {
			t.Fatal("the input channel's input never reached the browser")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := pv.dcs[InputLabel].Close(); err != nil {
		t.Fatal(err)
	}
	awaitHuman(t, m, false, 5*time.Second)
}

// A live view given control at its offer that never opens its input
// channel (a video-only offer) loses that control after a while: it
// could neither use nor hand it back (PROJ-196).
func TestControlEndsIfNoInputChannelOpens(t *testing.T) {
	was := inputOpenWait
	inputOpenWait = 300 * time.Millisecond
	t.Cleanup(func() { inputOpenWait = was })
	var c fakeClipboard
	h, m, _ := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	pv := newPionViewer(t) // video only
	connected := make(chan struct{})
	var once sync.Once
	pv.pc.OnConnectionStateChange(func(s pion.PeerConnectionState) {
		if s == pion.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	rec := offer(h, OfferOptions{Control: true}, pv.body)
	var answer struct {
		SDP string `json:"sdp"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &answer) != nil {
		t.Fatalf("offer: %d %s", rec.Code, rec.Body)
	}
	if err := pv.pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("never connected")
	}
	awaitHuman(t, m, false, 5*time.Second)
	if got, _ := m.Live("google"); got.Viewers != 1 {
		t.Errorf("viewers %d: the video should still be connected", got.Viewers)
	}
}
