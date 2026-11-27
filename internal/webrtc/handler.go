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
	h.Offer(w, r, r.PathValue("profile"), OfferOptions{})
}

// OfferOptions shape a live view.
type OfferOptions struct {
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
	// (TakeControlInHandoff). A handoff's view never falls back to the
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
	// From here on, everything names s.ID: Watch and TakeControlGen find
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
	vw := &viewer{h: h, session: s.ID, sock: h.m.Layout().ControlSocket(profile), allowed: o.Allowed, handoff: o.Handoff, helpEpoch: o.HelpEpoch}
	// opened closes once the page's input channel is open; gone once the
	// viewer has gone, or never connected.
	opened, gone := make(chan struct{}), make(chan struct{})
	var openOnce, goneOnce sync.Once
	markGone := func() { goneOnce.Do(func() { close(gone) }) }
	// A viewer promised control gets it or isn't connected: a page that
	// thinks it drives while it only watches would send input into nothing.
	// control and gen are read here, before anything else (the timer
	// below, the data channel) can change the viewer's.
	var control bool
	var gen uint64
	if o.Control {
		g, err := vw.take()
		if err != nil {
			unwatch()
			msg := "The browser isn't running."
			switch {
			case errors.Is(err, sessions.ErrHumanDriving):
				msg = "Someone else has control."
			case errors.Is(err, sessions.ErrAgentDriving):
				msg = "The agent is driving."
			case errors.Is(err, sessions.ErrNotInControl):
				msg = "The browser was handed back before you got control."
			case errors.Is(err, sessions.ErrHandoffEnded):
				msg = "This request for help has ended."
			}
			http.Error(w, msg, http.StatusConflict)
			return
		}
		vw.control, vw.gen = true, g
		control, gen = true, g
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
			_, _ = h.m.ReleaseControlGen(r.Context(), s.ID, gen)
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
		control = vw.control && h.m.InControl(s.ID, gen)
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
	sock    string

	allowed func() bool // nil: always
	conn    *Viewer
	// handoff marks a handoff's viewer, and helpEpoch is its handoff's:
	// it takes control only while the session still waits for that help.
	handoff   bool
	helpEpoch uint64

	mu sync.Mutex
	// control is set once this viewer took control, as generation gen
	// (TakeControlGen). Its control can also end without it (a handoff
	// ends, the profile page's Hand back), and someone else take the
	// browser: whatever it sends then names gen, which the manager no
	// longer honours, so it never uses or ends that takeover (PROJ-196).
	control   bool
	gen       uint64
	checkedAt time.Time // when allowed last said yes
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
			if v.h.m.InControl(v.session, v.gen) {
				say(map[string]any{"type": "control", "on": true})
				return
			}
			// Its control ended elsewhere: this is a take afresh.
			_ = v.releaseLocked(viewerHandBackWait)
		}
		gen, err := v.take()
		switch {
		case errors.Is(err, sessions.ErrHumanDriving):
			say(map[string]any{"type": "error", "msg": "Someone else has control."})
		case err == nil:
			v.control, v.gen = true, gen
			say(map[string]any{"type": "control", "on": true})
		case errors.Is(err, sessions.ErrAgentDriving):
			say(map[string]any{"type": "error", "msg": "The agent is driving. Try again in a few seconds, or when it asks for help."})
		case errors.Is(err, sessions.ErrNotInControl):
			say(map[string]any{"type": "error", "msg": "The browser was handed back before you got control. Try again."})
		case errors.Is(err, sessions.ErrHandoffEnded):
			say(map[string]any{"type": "error", "msg": "This request for help has ended."})
		default:
			say(map[string]any{"type": "error", "msg": "The browser isn't running."})
		}
	case "release":
		if err := v.releaseLocked(viewerHandBackWait); errors.Is(err, sessions.ErrHandingBack) {
			say(map[string]any{"type": "error", "msg": "The browser is still being handed back."})
		}
		say(map[string]any{"type": "control", "on": false})
	case "lift", "input", "text", "clipboard", "clipboard-set":
		var ctx context.Context
		var done func()
		var err error
		if v.control {
			// Held until the input has reached the browser: a hand-back
			// empties the clipboard only after it. Only for this viewer's
			// own takeover: once its control has ended, whoever drives now
			// (a person, or an agent mid-drag) gets nothing from it, not
			// even a lift.
			// ctx ends with the session, which waits for done before its
			// profile can have a new browser on the same socket.
			ctx, done, err = v.h.m.HumanInput(v.session, v.gen)
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
			var se *statusError
			refused := errors.As(err, &se) && se.code == http.StatusConflict
			if ctx.Err() == nil && !refused {
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
			path, body := "/input/events", []byte(m.Events)
			if m.Type == "text" {
				path = "/input/type"
				// A person typing: at once, not at an agent's human pace.
				body, _ = json.Marshal(map[string]string{"text": m.Text, "pace": "instant"})
			}
			if err := v.post(ctx, path, body); err != nil && !ended(err) {
				say(map[string]any{"type": "error", "msg": err.Error()})
			}
		}
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
	v.h.m.Note(v.session, "", what)
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

// take takes control for this viewer: for a handoff's, only while its
// handoff's Needs help lasts, checked by the manager in the same step.
func (v *viewer) take() (uint64, error) {
	if v.handoff {
		return v.h.m.TakeControlInHandoff(v.session, v.helpEpoch)
	}
	return v.h.m.TakeControlGen(v.session)
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
	_, err := v.h.m.ReleaseControlGen(ctx, v.session, v.gen)
	return err
}

// viewerHandBackWait bounds how long a live view's message waits for its
// hand-back (a browser that doesn't answer it).
const viewerHandBackWait = 30 * time.Second

func (v *viewer) post(ctx context.Context, path string, body []byte) error {
	_, err := v.do(ctx, http.MethodPost, path, body, http.StatusNoContent, controlWait)
	return err
}

// controlWait bounds a call to the container's control socket.
const controlWait = 10 * time.Second

// maxClipboard caps what a clipboard read brings back; the container caps
// it too.
const maxClipboard = 32 << 10

// do calls the container's control socket for the person, wanting status
// want, within wait and while ctx lasts (HumanInput's: the takeover's),
// and returns the answer's body. v.mu must be held.
func (v *viewer) do(ctx context.Context, method, path string, body []byte, want int, wait time.Duration) ([]byte, error) {
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
	req.Header.Set(sessions.TakeoverHeader, strconv.FormatUint(v.gen, 10))
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
