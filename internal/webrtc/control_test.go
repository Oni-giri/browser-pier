package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
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
			va := &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: sock}
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
			b := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: sock})
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
			awaitHuman(t, m, false, 5*time.Second) // the hand-back goes on by itself
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
	a := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
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
	awaitHuman(t, m, false, 5*time.Second) // the hand-back goes on by itself
	if snap, _ := m.Live("google"); snap.State != sessions.Running || snap.HumanControl {
		t.Fatalf("after the failed stop: %s, person in control %v", snap.State, snap.HumanControl)
	}
	// What the person held was let go, and the clipboard emptied before
	// agents had the browser back.
	if got := strings.Join(c.took(), " "); got != "release empty" {
		t.Errorf("the browser got %q, want release, then empty", got)
	}
	if _, done, err := m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
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
	a := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: sock})
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
	a := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: sock})
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	if r := a.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take again, still in control: %v", r)
	}
	m.ReleaseControl(s.ID)
	if err := m.TakeControl(s.ID, "person"); err != nil { // someone else
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
	// No goroutine waits for the channels: the last to open closes
	// opened, and a viewer whose channels never open leaves nothing
	// behind.
	var left atomic.Int32
	left.Store(int32(len(labels)))
	if len(labels) == 0 {
		close(pv.opened)
	}
	for _, label := range labels {
		dc, err := pc.CreateDataChannel(label, nil)
		if err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		dc.OnOpen(func() {
			once.Do(func() {
				if left.Add(-1) == 0 {
					close(pv.opened)
				}
			})
		})
		pv.dcs[label] = dc
	}
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
	pv.connect(t, h, OfferOptions{Control: true, Holder: sessions.AdminHolder})
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
	pv.connect(t, h, OfferOptions{Control: true, Holder: sessions.AdminHolder})
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
	rec := offer(h, OfferOptions{Control: true, Holder: sessions.AdminHolder}, pv.body)
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

// An offer for a session that has since stopped, its profile started
// again, is refused, and never takes the new session: a handoff's link
// is for the session it was opened in (PROJ-196).
func TestOfferForAStoppedSessionTakesNothing(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	if _, err := m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	next, _, err := m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	pv := newPionViewer(t, InputLabel)
	if rec := offer(h, OfferOptions{Control: true, Holder: sessions.AdminHolder, Session: s.ID}, pv.body); rec.Code != http.StatusConflict {
		t.Fatalf("an offer for the stopped session: %d %s", rec.Code, rec.Body)
	}
	if got, _ := m.Live("google"); got.ID != next.ID || got.HumanControl || got.Viewers != 0 {
		t.Fatalf("the new session: %+v", got)
	}
	// The profile page's own live view, connected to the stopped
	// session, can't take the new one either: its viewer names the
	// session it was offered.
	old := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
	if r := old.send(`{"type":"take"}`); len(r) != 1 || r[0]["type"] != "error" {
		t.Errorf("a take from the stopped session's live view: %v", r)
	}
	if got, _ := m.Live("google"); got.HumanControl {
		t.Error("the new session was taken")
	}
}

// A handoff's live view whose handoff has ended takes nothing, at its
// offer or by a later take, though its session is the same and running:
// an agent's long action goes on (PROJ-196).
func TestHandoffViewAfterItsHandoffEndedTakesNothing(t *testing.T) {
	var c fakeClipboard
	h, m, s := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	if _, err := m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	epoch, ok := m.HelpEpoch(s.ID)
	if !ok {
		t.Fatal("no help epoch")
	}
	if _, err := m.SetNeedsHelp(s.ID, false); err != nil { // the handoff ends
		t.Fatal(err)
	}
	ctx, done, err := m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	time.Sleep(3100 * time.Millisecond) // long enough for a take to break it off
	pv := newPionViewer(t, InputLabel)
	if rec := offer(h, OfferOptions{Control: true, Holder: sessions.HandoffHolder("h1"), Session: s.ID, Handoff: true, HelpEpoch: epoch}, pv.body); rec.Code != http.StatusConflict {
		t.Fatalf("the ended handoff's offer: %d %s", rec.Code, rec.Body)
	}
	old := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.HandoffHolder("h1"), sock: m.Layout().ControlSocket("google"), handoff: true, helpEpoch: epoch})
	if r := old.send(`{"type":"take"}`); len(r) != 1 || r[0]["type"] != "error" {
		t.Errorf("the ended handoff's view taking control: %v", r)
	}
	if ctx.Err() != nil {
		t.Error("the agent's action was broken off")
	}
	if got, _ := m.Live("google"); got.HumanControl {
		t.Error("taken")
	}
}

// fencingBrowser is a browser's control socket as far as the fence goes
// (images/chromium/entrypoint/fence.go): a lift naming a fence refuses,
// from then on, the operations of takeovers up to it, checked right
// before each acts. Its clipboard set can be held up after reading its
// request, as a busy browser's might be.
type fencingBrowser struct {
	mu     sync.RWMutex // read across a person's action, written to raise the fence
	fence  uint64
	cmu    sync.Mutex
	calls  []string
	holdUp chan struct{} // if set, a set waits for it once its request is read
	inSet  chan struct{} // gets a value as a set is held up
}

func (b *fencingBrowser) note(what string) {
	b.cmu.Lock()
	b.calls = append(b.calls, what)
	b.cmu.Unlock()
}

func (b *fencingBrowser) took() string {
	b.cmu.Lock()
	defer b.cmu.Unlock()
	out := strings.Join(b.calls, " ")
	b.calls = nil
	return out
}

func (b *fencingBrowser) routes(mux *http.ServeMux) {
	gen := func(r *http.Request) uint64 {
		n, _ := strconv.ParseUint(r.Header.Get(sessions.TakeoverHeader), 10, 64)
		return n
	}
	// act runs a person's action unless their takeover is fenced; an
	// operation naming none (an agent's) always runs.
	act := func(w http.ResponseWriter, r *http.Request, what string) {
		n := gen(r)
		b.mu.RLock()
		defer b.mu.RUnlock()
		if n != 0 && n <= b.fence {
			b.note("refused")
			http.Error(w, "this person's control of the browser has ended", http.StatusConflict)
			return
		}
		b.note(what)
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		if f := r.Header.Get(sessions.FenceHeader); f != "" {
			n, _ := strconv.ParseUint(f, 10, 64)
			b.mu.Lock()
			b.fence = max(b.fence, n)
			b.mu.Unlock()
			b.note("lift")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"fence":%d}`, n)
			return
		}
		act(w, r, "lift")
	})
	mux.HandleFunc("POST /input/clipboard", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if b.holdUp != nil {
			b.inSet <- struct{}{}
			<-b.holdUp
		}
		act(w, r, "set")
	})
	mux.HandleFunc("POST /input/events", func(w http.ResponseWriter, r *http.Request) { act(w, r, "events") })
	mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, _ *http.Request) {
		b.note("empty")
		w.WriteHeader(http.StatusNoContent)
	})
}

// A clipboard set the person's live view sent, held up inside the
// browser after its request was read, across the hand-back (the
// orchestrator's side long since gave up on it), never acts after the
// lift and the emptying: the browser has fenced the takeover by then
// (PROJ-196).
func TestHeldUpSetNeverActsAfterTheHandBack(t *testing.T) {
	b := &fencingBrowser{holdUp: make(chan struct{}), inSet: make(chan struct{}, 1)}
	h, m, s := offerFixtureWith(t, b.routes, slog.New(slog.DiscardHandler))
	k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
	if r := k.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	setDone := make(chan []map[string]any, 1)
	go func() { setDone <- k.send(set(1, "the person's secret", false)) }()
	select {
	case <-b.inSet:
	case <-time.After(5 * time.Second):
		t.Fatal("the set never reached the browser")
	}
	m.ReleaseControl(s.ID) // a handoff ends, say
	if r := <-setDone; len(r) == 0 || r[0]["type"] != "clipboard-error" {
		t.Errorf("the set as the control ended: %v", r)
	}
	close(b.holdUp) // the browser gets round to it
	time.Sleep(100 * time.Millisecond)
	if got := b.took(); got != "lift empty refused" {
		t.Errorf("the browser got %q, want the lift, the emptying, and the held-up set refused", got)
	}
	if _, done, err := m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input after = %v", err)
	} else {
		done()
	}
}

// A handoff's offer whose handoff registration (Opened) stalls still
// loses its control if no input channel opens within inputOpenWait of
// the grant: the clock starts with the grant (PROJ-196).
func TestInputChannelClockStartsAtTheGrant(t *testing.T) {
	was := inputOpenWait
	inputOpenWait = 300 * time.Millisecond
	t.Cleanup(func() { inputOpenWait = was })
	var c fakeClipboard
	h, m, _ := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	pv := newPionViewer(t) // video only
	stall := make(chan struct{})
	defer close(stall)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		offer(h, OfferOptions{Control: true, Holder: sessions.AdminHolder, Opened: func(*Viewer) { <-stall }}, pv.body)
	}()
	awaitHuman(t, m, true, 5*time.Second)
	awaitHuman(t, m, false, 3*time.Second)
	select {
	case <-answered:
		t.Error("the registration didn't stall")
	default:
	}
}

// A live view whose operation the browser refuses because its takeover
// has ended (409) gives its control up at once: it tells its page, and
// sends the browser nothing more, rather than answer every move with an
// error (PROJ-196).
func TestRefusedViewerGivesUpControl(t *testing.T) {
	b := &fencingBrowser{}
	h, m, s := offerFixtureWith(t, b.routes, slog.New(slog.DiscardHandler))
	k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
	if r := k.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	// The browser has fenced this takeover, though BrowserPier still
	// counts it as holding control.
	b.mu.Lock()
	b.fence = k.v.turn
	b.mu.Unlock()
	if r := k.send(`{"type":"input","events":[]}`); len(r) != 1 || r[0]["type"] != "control" || r[0]["on"] != false {
		t.Errorf("input refused by the browser: %v, want control off", r)
	}
	awaitHuman(t, m, false, 5*time.Second)
	b.took()
	for range 5 {
		if r := k.send(`{"type":"input","events":[]}`); len(r) != 1 || r[0]["on"] != false {
			t.Errorf("input after giving control up: %v", r)
		}
	}
	if got := b.took(); got != "" {
		t.Errorf("the viewer kept sending after giving control up: %q", got)
	}
}

// An offer whose control has already been handed back by the time it
// answers (its input channel's time ran out during a slow registration)
// says so: control false (PROJ-196).
func TestOfferAnswersTheControlItHas(t *testing.T) {
	was := inputOpenWait
	inputOpenWait = time.Millisecond
	t.Cleanup(func() { inputOpenWait = was })
	var c fakeClipboard
	h, m, _ := offerFixtureWith(t, c.routes, slog.New(slog.DiscardHandler))
	pv := newPionViewer(t)
	rec := offer(h, OfferOptions{Control: true, Holder: sessions.AdminHolder, Opened: func(*Viewer) { awaitHuman(t, m, false, 5*time.Second) }}, pv.body)
	var answer struct {
		Control bool `json:"control"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &answer) != nil {
		t.Fatalf("offer: %d %s", rec.Code, rec.Body)
	}
	if answer.Control {
		t.Error("the offer answered control: true for control already handed back")
	}
}

// A live view's Hand back answers control:false once the browser is
// handed back (within a bound), so its page, asking the status at once,
// doesn't see a person in control, as if someone else had it (PROJ-196).
func TestHandBackAnswersOnceHandedBack(t *testing.T) {
	slowLift := func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			sessionstest.AnswerLift(w, r)
		})
	}
	h, m, s := offerFixtureWith(t, slowLift, slog.New(slog.DiscardHandler))
	k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
	if r := k.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	if r := k.send(`{"type":"release"}`); len(r) != 1 || r[0]["on"] != false {
		t.Fatalf("release: %v", r)
	}
	if got, _ := m.Live("google"); got.HumanControl {
		t.Error("told control:false while the browser still shows a person in control")
	}
}

// A live view whose operation the browser refuses gives control up at
// once: it lets go of the operation's lease first, so its hand-back,
// which waits for the person's operations under way, doesn't wait on it
// (PROJ-196).
func TestRefusedViewerHandsBackAtOnce(t *testing.T) {
	b := &fencingBrowser{}
	h, m, s := offerFixtureWith(t, b.routes, slog.New(slog.DiscardHandler))
	k := newTalk(t, &viewer{h: h, session: s.ID, holder: sessions.AdminHolder, sock: m.Layout().ControlSocket("google")})
	if r := k.send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	b.mu.Lock()
	b.fence = k.v.turn
	b.mu.Unlock()
	start := time.Now()
	if r := k.send(`{"type":"input","events":[]}`); len(r) != 1 || r[0]["on"] != false {
		t.Errorf("input refused by the browser: %v, want control off", r)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("giving control up took %s: the hand-back waited on the viewer's own operation", took.Round(time.Millisecond))
	}
	if got, _ := m.Live("google"); got.HumanControl {
		t.Error("told control off with the browser not yet handed back")
	}
}
