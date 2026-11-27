package webrtc

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/static"
)

//go:embed live.js
var liveJS []byte

var liveTag = static.Tag(liveJS)

// Handler serves the live view to the signed-in admin: the page, its
// status, and the WebRTC offer.
type Handler struct {
	relay *Relay
	m     *sessions.Manager
	web   *auth.Web
	log   *slog.Logger
}

func NewHandler(relay *Relay, m *sessions.Manager, web *auth.Web, log *slog.Logger) *Handler {
	return &Handler{relay: relay, m: m, web: web, log: log}
}

// Register adds /live/{profile}'s status, ICE servers and offer, and the
// client script.
// The page itself is the web UI's /profiles/{profile}.
func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.web.RequireAdmin(f) }
	mux.Handle("GET /live/{profile}", admin(h.page))
	mux.Handle("GET /live/{profile}/status", admin(h.status))
	mux.Handle("GET /live/{profile}/ice", admin(h.ice))
	mux.Handle("POST /live/{profile}/offer", admin(h.offer))
	mux.HandleFunc("GET /static/live.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		static.Header(w, r, liveTag)
		http.ServeContent(w, r, "live.js", time.Time{}, bytes.NewReader(liveJS))
	})
}

// page sends the old live view address to the profile's page in the web UI.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/profiles/"+url.PathEscape(r.PathValue("profile")), http.StatusMovedPermanently)
}

type statusJSON struct {
	State        string   `json:"state"`
	InUseBy      []string `json:"in_use_by,omitempty"`
	HumanControl bool     `json:"human_control"`
	Viewers      int      `json:"viewers"`
	// Record is the profile's Record sessions setting as the session
	// applies it, for the live view's hint (PROJ-23). This status is the
	// admin's alone.
	Record bool `json:"record"`
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	out := statusJSON{State: string(sessions.Stopped)}
	if s, ok := h.m.Live(r.PathValue("profile")); ok {
		out = statusJSON{State: string(s.State), InUseBy: s.InUseBy, HumanControl: s.HumanControl, Viewers: s.Viewers, Record: s.Record}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// LiveTURNLifetime is how long the admin's TURN credentials can open new
// relays. A relay already open outlives them (coturn checks them only for
// new allocations), and a live view can stay open all day: the page gets
// new ones whenever it reconnects.
const LiveTURNLifetime = 12 * time.Hour

func (h *Handler) ice(w http.ResponseWriter, r *http.Request) {
	// One viewer per signed-in browser, for the TURN server's quota.
	s, _ := auth.SessionFromContext(r.Context())
	h.ICE(w, r, time.Now().Add(LiveTURNLifetime), "admin session "+s.ID)
}

// ICE sends the iceServers a viewer connects with, TURN credentials
// included, which open new relays until the given time. viewer names who
// asks, the same on each request (see TURN.Servers). The caller has
// already checked who may watch: they're for that viewer alone.
func (h *Handler) ICE(w http.ResponseWriter, _ *http.Request, until time.Time, viewer string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"iceServers": h.relay.ICEServers(until, viewer)})
}

func (h *Handler) offer(w http.ResponseWriter, r *http.Request) {
	h.Offer(w, r, r.PathValue("profile"), OfferOptions{Holder: sessions.AdminHolder})
}

// OfferOptions shape a live view.
type OfferOptions struct {
	// Holder names who this viewer is when it takes control (see
	// sessions.Manager.TakeControl): the signed-in admin's browser or the
	// handoff link. The file chooser routes answer only them (PROJ-184).
	Holder string
	// Control gives the viewer the mouse and keyboard from the start (a
	// handoff link, once unlocked).
	Control bool
	// Allowed, if set, is checked before acting on the viewer's messages;
	// once it says no, the viewer is cut off (a handoff that has ended).
	Allowed func() bool
	// Opened, if set, gets the viewer once connected, to close it later.
	Opened func(*Viewer)
	// Session, if set, is the session the caller let this viewer watch
	// (a handoff's): the offer is refused unless it is still the
	// profile's live session, and control is only ever taken of it,
	// never of a session that replaced it meanwhile.
	Session string
	// Handoff marks a handoff's live view, and HelpEpoch is its handoff's
	// (sessions.Manager.HelpEpoch): control, at the offer or a later
	// take, is taken only while the session still waits for that help
	// (TakeControlTurnByLink). A handoff's view never falls back to the
	// admin's take, whatever its epoch.
	Handoff   bool
	HelpEpoch uint64
}

// inputOpenWait is how long a viewer given control at its offer has to
// open its input channel: until then it can neither drive nor hand back,
// so its control ends.
var inputOpenWait = 30 * time.Second

// Offer answers a WebRTC offer for profile's live view. The caller has
// already checked who may watch.
func (h *Handler) Offer(w http.ResponseWriter, r *http.Request, profile string, o OfferOptions) {
	s, ok := h.m.Live(profile)
	if !ok || (s.State != sessions.Running && s.State != sessions.NeedsHelp) || (o.Session != "" && s.ID != o.Session) {
		http.Error(w, "the browser isn't running", http.StatusConflict)
		return
	}
	// From here on, everything names s.ID: Watch and TakeControlTurn find
	// that session or none, under the manager's lock, so a session that
	// replaces it is never watched or taken through this offer.
	var in struct {
		SDP string `json:"sdp"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || in.SDP == "" {
		http.Error(w, "send {\"sdp\": offer}", http.StatusBadRequest)
		return
	}
	unwatch, err := h.m.Watch(s.ID)
	if err != nil {
		http.Error(w, "the browser isn't running", http.StatusConflict)
		return
	}
	vw := &viewer{h: h, session: s.ID, holder: o.Holder, sock: h.m.Layout().ControlSocket(profile), allowed: o.Allowed, handoff: o.Handoff, helpEpoch: o.HelpEpoch}
	// opened closes once the page's input channel is open; gone once the
	// viewer has gone, or never connected.
	opened, gone := make(chan struct{}), make(chan struct{})
	var openOnce, goneOnce sync.Once
	markGone := func() { goneOnce.Do(func() { close(gone) }) }
	// A viewer promised control gets it or isn't connected: a page that
	// thinks it drives while it only watches would send input into nothing.
	// control and turn are read here, before anything else (the timer
	// below, the data channel) can change the viewer's.
	var control bool
	var turn uint64
	if o.Control {
		t, err := vw.take()
		if err != nil {
			unwatch()
			msg, code := "The browser isn't running.", http.StatusConflict
			switch {
			case errors.Is(err, sessions.ErrHumanDriving):
				msg = "Someone else has control."
			case errors.Is(err, sessions.ErrAgentDriving):
				msg = "The agent is driving."
			case errors.Is(err, sessions.ErrControlEnded):
				msg = "Control was handed back meanwhile. Try again."
			case errors.Is(err, sessions.ErrHandoffEnded):
				msg = "This request for help has ended."
			case errors.Is(err, sessions.ErrNoHolder):
				h.log.Error("a live view offered control with no holder", "profile", profile)
				msg, code = "Couldn't take control: this live view says nobody's taking it.", http.StatusInternalServerError
			}
			http.Error(w, msg, code)
			return
		}
		vw.control, vw.turn = true, t
		control, turn = true, t
		// Control from now on, but no input channel yet: if none opens
		// within inputOpenWait of now (a video-only offer, one stuck
		// negotiating, or a registration that stalls), the control ends,
		// rather than stay with a page that can't use or hand it back.
		// The timer starts here, with the grant, whatever comes after.
		go func() {
			t := time.NewTimer(inputOpenWait)
			defer t.Stop()
			select {
			case <-opened:
			case <-gone:
			case <-t.C:
				vw.handBack()
			}
		}()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// The page's input channel closing ends its control, as the page
	// itself takes it (live.js): the video may stay connected, but no
	// message can come to drive or hand back the browser.
	v, answer, err := h.relay.Connect(ctx, profile, in.SDP, Input{
		Message: vw.message,
		Opened:  func() { openOnce.Do(func() { close(opened) }) },
		Closed:  vw.handBack,
	})
	if err != nil {
		markGone()
		if control {
			// As long as the request lasts, viewerHandBackWait at most:
			// the hand-back goes on by itself after.
			rctx, rcancel := context.WithTimeout(r.Context(), viewerHandBackWait)
			_, _ = h.m.ReleaseTurn(rctx, s.ID, turn)
			rcancel()
		}
		unwatch()
		http.Error(w, "couldn't connect: "+err.Error(), http.StatusBadRequest)
		return
	}
	vw.conn = v
	if o.Opened != nil {
		o.Opened(v)
	}
	go func() {
		<-v.Done()
		markGone()
		vw.handBack()
		unwatch()
		v.markGone()
	}()
	if control {
		// It may have been handed back meanwhile (its input channel's
		// time ran out, its handoff ended): say how it is now.
		vw.mu.Lock()
		control = vw.control && h.m.HoldsTurn(s.ID, turn)
		vw.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	// control says whether this viewer starts with the mouse and keyboard.
	_ = json.NewEncoder(w).Encode(map[string]any{"sdp": answer, "control": control})
}

// viewer is one live view's input: it only reaches the browser while
// this viewer holds control.
type viewer struct {
	h       *Handler
	session string
	holder  string // who takes control through this viewer
	sock    string

	allowed func() bool // nil: always
	conn    *Viewer
	// handoff marks a handoff's viewer, and helpEpoch is its handoff's:
	// it takes control only while the session still waits for that help.
	handoff   bool
	helpEpoch uint64

	mu sync.Mutex
	// control is set once this viewer took control, as turn (the
	// takeover's generation, sessions.TakeControlTurn). Its control can
	// also end without it (a handoff ends, the profile page's Hand back),
	// and someone else take the browser, even another of the admin's
	// views under the same holder: whatever it sends then names turn,
	// which neither the manager nor the browser honours any more, so it
	// never uses or ends that takeover (PROJ-184, PROJ-196).
	control   bool
	turn      uint64
	checkedAt time.Time // when allowed last said yes
	// typing is the Type under way, if one is (PROJ-195): it runs on its
	// own goroutine, so Hand back and lift are read at once and cancel
	// it; typed is closed once it has ended (tests wait on it).
	typing context.CancelFunc
	typed  chan struct{}
}

// stillAllowed re-checks, at most once a second, that this viewer may act.
func (v *viewer) stillAllowed() bool {
	if v.allowed == nil || time.Since(v.checkedAt) < time.Second {
		return true
	}
	if !v.allowed() {
		return false
	}
	v.checkedAt = time.Now()
	return true
}

// message handles one data channel message from the viewer's page:
//
//	{"type":"take"}                       take control
//	{"type":"release"}                    hand back
//	{"type":"input","events":[...]}       mouse and keys (images/chromium/entrypoint/live.go)
//	{"type":"text","text":"..."}          type a string (the phone's Type button)
//	{"type":"lift"}                       let go of held keys and buttons (the page lost focus)
//
// and the clipboard box's (PROJ-194), each answered with its id, or with
// {"type":"clipboard-error","id":1,"msg":"..."}:
//
//	{"type":"clipboard","id":1}           read the browser's clipboard: {"type":"clipboard","id":1,"text":"..."}
//	{"type":"clipboard-set","id":1,"text":"...","paste":false}
//	                                      put text on it, held until the takeover ends, then
//	                                      with paste press Ctrl+V: {"type":"clipboard-set","id":1}
//
// What is typed, read or set is never logged: it may be a password. The
// clipboard is emptied as the person's control ends (ReleaseControl).
func (v *viewer) message(msg []byte, reply func(string)) {
	var m struct {
		Type   string          `json:"type"`
		Events json.RawMessage `json:"events"`
		Text   string          `json:"text"`
		ID     int             `json:"id"`
		Paste  bool            `json:"paste"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}
	say := func(v any) {
		b, _ := json.Marshal(v)
		reply(string(b))
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.stillAllowed() {
		_ = v.releaseLocked(viewerHandBackWait)
		if v.conn != nil {
			go v.conn.Close()
		}
		return
	}
	switch m.Type {
	case "take":
		if v.control {
			if v.h.m.HoldsTurn(v.session, v.turn) {
				say(map[string]any{"type": "control", "on": true})
				return
			}
			// Its control ended elsewhere: this is a take afresh.
			_ = v.releaseLocked(viewerHandBackWait)
		}
		turn, err := v.take()
		switch {
		case errors.Is(err, sessions.ErrHumanDriving):
			say(map[string]any{"type": "error", "msg": "Someone else has control."})
		case err == nil:
			v.control, v.turn = true, turn
			say(map[string]any{"type": "control", "on": true})
		case errors.Is(err, sessions.ErrAgentDriving):
			say(map[string]any{"type": "error", "msg": "The agent is driving. Try again in a few seconds, or when it asks for help."})
		case errors.Is(err, sessions.ErrControlEnded):
			say(map[string]any{"type": "error", "msg": "Control was handed back meanwhile. Try again."})
		case errors.Is(err, sessions.ErrHandoffEnded):
			say(map[string]any{"type": "error", "msg": "This request for help has ended."})
		case errors.Is(err, sessions.ErrNoHolder):
			v.h.log.Error("a live view took control with no holder", "session", v.session)
			say(map[string]any{"type": "error", "msg": "Couldn't take control: this live view says nobody's taking it."})
		default:
			say(map[string]any{"type": "error", "msg": "The browser isn't running."})
		}
	case "release":
		v.stopTypingLocked()
		if err := v.releaseLocked(viewerHandBackWait); errors.Is(err, sessions.ErrHandingBack) {
			say(map[string]any{"type": "error", "msg": "The browser is still being handed back."})
		}
		say(map[string]any{"type": "control", "on": false})
	case "lift", "input", "text", "clipboard", "clipboard-set":
		if v.typing != nil {
			// A lift never cuts a Type short: the phone's Type prompt
			// itself blurs the page as it closes (Safari, desktop
			// Chromium), so the page's blur lift arrives right behind the
			// Type it just sent. The Type lets go of the live view's keys
			// before it types (the drain) and of its own after; Hand back,
			// the end of control or the view going still stop it.
			if m.Type == "lift" {
				return
			}
			// The person's Type is still going: its keys and this input's
			// would interleave. Hand back is still read, and cancels it.
			msg := "Still typing: wait for it to finish, or hand back to stop it."
			if m.Type == "clipboard" || m.Type == "clipboard-set" {
				say(map[string]any{"type": "clipboard-error", "id": m.ID, "msg": msg})
			} else {
				say(map[string]any{"type": "error", "msg": msg})
			}
			return
		}
		if m.Type == "text" {
			if n := utf8.RuneCountInString(m.Text); n > maxLiveType {
				say(map[string]any{"type": "error", "msg": fmt.Sprintf("That's %d characters: at most %d can be typed at once.", n, maxLiveType)})
				return
			}
		}
		var ctx context.Context
		var done func()
		var err error
		if v.control {
			// Held until the input has reached the browser: a hand-back
			// empties the clipboard only after it. Only for this viewer's
			// own turn: once its control has ended, whoever drives now (a
			// person, another of the admin's views under the same holder,
			// or an agent mid-drag) gets nothing from it, not even a lift.
			// ctx is the takeover's: it ends as its hand-back begins, or
			// the session ends, which waits for done before its profile
			// can have a new browser on the same socket.
			ctx, done, err = v.h.m.HumanInputTurn(v.session, v.holder, v.turn)
		}
		if !v.control || err != nil {
			if m.Type == "lift" && !v.control {
				return // nothing held: the page lost focus without control
			}
			// A browser that is stopping refuses input while this viewer
			// still holds control: its control ends with the refusal, as
			// the page is told, rather than outliving it with no one
			// holding it if the stop fails. For control that has ended,
			// this only drops the flag.
			_ = v.releaseLocked(viewerHandBackWait)
			if m.Type == "clipboard" || m.Type == "clipboard-set" {
				say(map[string]any{"type": "clipboard-error", "id": m.ID, "msg": "You don't have control."})
			}
			say(map[string]any{"type": "control", "on": false})
			return
		}
		if m.Type == "text" {
			// On a goroutine of its own, with this turn's lease (done),
			// which it lets go of when it ends (PROJ-195).
			v.startTypingLocked(ctx, done, m.Text, say)
			return
		}
		// ended: the browser refused the operation (409) because this
		// takeover has ended, or it was cut short as its hand-back began.
		// The viewer then gives control up, tells its page, and sends
		// nothing more, rather than answer every move with an error. It
		// gives it up only once the operation's lease is let go (done):
		// the hand-back waits for the person's operations under way, this
		// one included, so releasing while holding it would wait on itself.
		giveUp := false
		defer func() {
			done()
			if giveUp {
				_ = v.releaseLocked(viewerHandBackWait)
				say(map[string]any{"type": "control", "on": false})
			}
		}()
		ended := func(err error) bool {
			if !refusedOrCut(ctx, err) {
				return false
			}
			giveUp = true
			return true
		}
		switch m.Type {
		case "lift":
			_ = ended(v.post(ctx, "/input/release", nil))
		case "clipboard":
			text, err := v.do(ctx, http.MethodGet, "/input/clipboard", nil, http.StatusOK, controlWait)
			if err != nil {
				say(map[string]any{"type": "clipboard-error", "id": m.ID, "msg": "Couldn't read the browser's clipboard: " + err.Error()})
				ended(err)
				return
			}
			say(map[string]any{"type": "clipboard", "id": m.ID, "text": string(text)})
		case "clipboard-set":
			if err := v.setClipboard(ctx, m.ID, m.Text, m.Paste, say); err != nil {
				ended(err)
			}
		default:
			if err := v.post(ctx, "/input/events", []byte(m.Events)); err != nil && !ended(err) {
				say(map[string]any{"type": "error", "msg": err.Error()})
			}
		}
	}
}

// startTypingLocked types the person's text (the phone's Type button) on
// a goroutine of its own, with v.mu held to start it: at once, not at an
// agent's human pace, and not held back by an agent's shortcut gap
// (PROJ-151); with time for characters the keyboard map lacks
// (PROJ-195). It names this viewer's turn, and holds done (that turn's
// HumanInputTurn lease) until it ends, so a hand-back waits for it before
// emptying the clipboard; it breaks off as soon as the takeover's control
// starts to end (inputCtx, the takeover's context), they hand back
// (stopTypingLocked), or this view goes; a lift doesn't stop it. If the
// browser refuses it (its takeover fenced) or the takeover's end cuts it
// short, the viewer gives control up, as for any input. One at a time:
// the caller refuses another meanwhile.
func (v *viewer) startTypingLocked(inputCtx context.Context, done func(), text string, say func(any)) {
	ctx, cancel := context.WithCancel(inputCtx)
	typed := make(chan struct{})
	v.typing, v.typed = cancel, typed
	turn := v.turn
	var gone <-chan struct{}
	if v.conn != nil {
		gone = v.conn.Done()
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	go func() {
		go func() {
			select {
			case <-gone:
				cancel()
			case <-ctx.Done():
			}
		}()
		_, err := v.doAs(ctx, turn, http.MethodPost, "/input/live-type", body, http.StatusNoContent, typeWait(text))
		cancelled := ctx.Err() != nil
		ended := refusedOrCut(inputCtx, err)
		cancel()
		// The lease first, without v.mu: a hand-back holding v.mu waits
		// for it.
		done()
		v.mu.Lock()
		if v.typed == typed {
			v.typing, v.typed = nil, nil
		}
		if ended && v.control && v.turn == turn {
			_ = v.releaseLocked(viewerHandBackWait)
			say(map[string]any{"type": "control", "on": false})
		}
		v.mu.Unlock()
		close(typed)
		if err != nil && !cancelled && !ended {
			say(map[string]any{"type": "error", "msg": err.Error()})
		}
	}()
}

// refusedOrCut reports whether a person's call failed because their
// takeover has ended: the browser refused it (409, fenced), or the
// takeover's context (ctx) ended as its hand-back began.
func refusedOrCut(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	var se *statusError
	return (errors.As(err, &se) && se.code == http.StatusConflict) || ctx.Err() != nil
}

// stopTypingLocked cancels the person's Type under way, if one is, with
// v.mu held. It doesn't wait: a hand-back waits for its lease anyway.
func (v *viewer) stopTypingLocked() {
	if v.typing != nil {
		v.typing()
	}
}

// maxClipboardSet caps the text the clipboard box sends, in characters;
// the container caps it too.
const maxClipboardSet = 10000

// setClipboard puts the clipboard box's text on the browser's clipboard
// and, with paste, presses Ctrl+V. The log says only that it happened:
// not the text, nor its length, which would tell agents a password's.
func (v *viewer) setClipboard(ctx context.Context, id int, text string, paste bool, say func(any)) error {
	fail := func(msg string) { say(map[string]any{"type": "clipboard-error", "id": id, "msg": msg}) }
	n := utf8.RuneCountInString(text)
	switch {
	case n == 0:
		fail("There's no text to send.")
		return nil
	case n > maxClipboardSet:
		fail(fmt.Sprintf("That's %d characters: at most %d can be sent at once.", n, maxClipboardSet))
		return nil
	}
	path, what := "/input/clipboard", "Clipboard set"
	if paste {
		path, what = "/input/paste", "Pasted from the clipboard box"
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	if err := v.post(ctx, path, body); err != nil {
		fail("Couldn't send it to the browser: " + err.Error())
		return err
	}
	v.h.m.NoteControl(v.session, what)
	say(map[string]any{"type": "clipboard-set", "id": id})
	return nil
}

// statusError is the browser's refusal of a person's call, with its
// status.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// take takes control for this viewer's holder: for a handoff's, only
// while its handoff's Needs help lasts, checked by the manager in the
// same step.
func (v *viewer) take() (uint64, error) {
	if v.handoff {
		return v.h.m.TakeControlTurnByLink(v.session, v.holder, v.helpEpoch)
	}
	return v.h.m.TakeControlTurn(v.session, v.holder)
}

// handBack runs when the viewer goes, or its input channel closes:
// control returns to agents with no key or button left held.
func (v *viewer) handBack() {
	v.mu.Lock()
	defer v.mu.Unlock()
	// No page to answer: it waits the hand-back out, whatever it takes
	// (a stop ends one the browser never answers), so whoever watches for
	// this viewer to be gone (a handoff's reloaded page) finds the browser
	// handed back.
	_ = v.releaseLocked(0)
}

// releaseLocked ends this viewer's control, if it still has it (the
// hand-back lets go of what the person held first). Control that has
// ended without it (and perhaps been taken by someone else since) is
// left alone: the viewer only forgets it had it.
func (v *viewer) releaseLocked(wait time.Duration) error {
	if !v.control {
		return nil
	}
	v.control = false
	// The hand-back is over before this returns (agents have the
	// browser), unless wait (if not 0) passes first: ErrHandingBack, the
	// hand-back going on by itself.
	ctx := context.Background()
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	_, err := v.h.m.ReleaseTurn(ctx, v.session, v.turn)
	return err
}

// viewerHandBackWait bounds how long a live view's message waits for its
// hand-back (a browser that doesn't answer it).
const viewerHandBackWait = sessions.HandBackWait

func (v *viewer) post(ctx context.Context, path string, body []byte) error {
	_, err := v.do(ctx, http.MethodPost, path, body, http.StatusNoContent, controlWait)
	return err
}

// controlWait bounds a call to the container's control socket.
const controlWait = 10 * time.Second

// maxLiveType caps the Type button's text, as the container's maxTypeLen
// does, before typeWait is worked out from it.
const maxLiveType = 2000

// typeWait bounds the Type button's call, which may type up to 2,000
// characters: 12 ms a key at instant pace, and, for characters the
// keyboard map lacks, stretches of as many bound keys as the map has
// spare (13 with the image's layout data) that each take about 0.55 s
// (PROJ-195). 60 ms a character covers text where every character is a
// new one; TestTypeWaitFitsTheText checks it against keyplan.Estimate,
// the browser's own plan.
func typeWait(text string) time.Duration {
	return controlWait + time.Duration(utf8.RuneCountInString(text))*60*time.Millisecond
}

// maxClipboard caps what a clipboard read brings back; the container caps
// it too.
const maxClipboard = 32 << 10

// do calls the container's control socket for the person, wanting status
// want, within wait and while ctx lasts (HumanInput's: the takeover's),
// and returns the answer's body. v.mu must be held: it names this
// viewer's turn (doAs).
func (v *viewer) do(ctx context.Context, method, path string, body []byte, want int, wait time.Duration) ([]byte, error) {
	return v.doAs(ctx, v.turn, method, path, body, want, wait)
}

// doAs is do for takeover turn, without v.mu: the Type's goroutine
// (startTypingLocked) names the turn it was started in.
func (v *viewer) doAs(ctx context.Context, turn uint64, method, path string, body []byte, want int, wait time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", v.sock)
	}}}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://control"+path, bytes.NewReader(body)) //nolint:gosec // a Unix socket and fixed paths
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Every call here is the person's, for the takeover this viewer took:
	// once its hand-back has fenced it, the browser refuses the call,
	// however long it was held up (PROJ-196).
	req.Header.Set(sessions.TakeoverHeader, strconv.FormatUint(turn, 10))
	resp, err := c.Do(req) //nolint:gosec // as above
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, &statusError{resp.StatusCode, string(bytes.TrimSpace(b))}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxClipboard+1))
	if err == nil && len(b) > maxClipboard {
		err = fmt.Errorf("over %d kB", maxClipboard>>10)
	}
	return b, err
}
