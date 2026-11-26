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
}

// Offer answers a WebRTC offer for profile's live view. The caller has
// already checked who may watch.
func (h *Handler) Offer(w http.ResponseWriter, r *http.Request, profile string, o OfferOptions) {
	s, ok := h.m.Live(profile)
	if !ok || (s.State != sessions.Running && s.State != sessions.NeedsHelp) {
		http.Error(w, "the browser isn't running", http.StatusConflict)
		return
	}
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
	vw := &viewer{h: h, session: s.ID, holder: o.Holder, sock: h.m.Layout().ControlSocket(profile), allowed: o.Allowed}
	// A viewer promised control gets it or isn't connected: a page that
	// thinks it drives while it only watches would send input into nothing.
	if o.Control {
		turn, err := h.m.TakeControlTurnByLink(s.ID, vw.holder)
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
			case errors.Is(err, sessions.ErrNoHolder):
				h.log.Error("a live view offered control with no holder", "profile", profile)
				msg, code = "Couldn't take control: this live view says nobody's taking it.", http.StatusInternalServerError
			}
			http.Error(w, msg, code)
			return
		}
		vw.control, vw.turn = true, turn
	}
	control := vw.control // read before the data channel can change it
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	v, answer, err := h.relay.Connect(ctx, profile, in.SDP, vw.message)
	if err != nil {
		if control {
			h.m.ReleaseTurn(s.ID, vw.turn) // nothing was pressed yet: no need to lift keys
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
		vw.handBack()
		unwatch()
	}()
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

	mu      sync.Mutex
	control bool
	// turn is this viewer's Take control (sessions.TakeControlTurn): its
	// control can end behind its back (a handoff finished, Hand back from
	// another page), and then it releases or lifts nothing of the next
	// person's (PROJ-184).
	turn      uint64
	checkedAt time.Time // when allowed last said yes
}

// holdsLocked reports whether this viewer still has control, and forgets
// control it lost behind its back.
func (v *viewer) holdsLocked() bool {
	if v.control && !v.h.m.HoldsTurn(v.session, v.turn) {
		v.control = false
	}
	return v.control
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
		v.releaseLocked()
		if v.conn != nil {
			go v.conn.Close()
		}
		return
	}
	switch m.Type {
	case "take":
		if v.holdsLocked() {
			say(map[string]any{"type": "control", "on": true})
			return
		}
		turn, err := v.h.m.TakeControlTurn(v.session, v.holder)
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
		case errors.Is(err, sessions.ErrNoHolder):
			v.h.log.Error("a live view took control with no holder", "session", v.session)
			say(map[string]any{"type": "error", "msg": "Couldn't take control: this live view says nobody's taking it."})
		default:
			say(map[string]any{"type": "error", "msg": "The browser isn't running."})
		}
	case "release":
		v.releaseLocked()
		say(map[string]any{"type": "control", "on": false})
	case "lift":
		// As input of this turn's: never lifts the next person's keys.
		if v.holdsLocked() {
			if done, err := v.h.m.HumanInputTurn(v.session, v.holder, v.turn); err == nil {
				_ = v.post("/input/release", nil)
				done()
			}
		}
	case "input", "text", "clipboard", "clipboard-set":
		var done func()
		var err error
		if v.holdsLocked() {
			// Held until the input has reached the browser: a hand-back
			// empties the clipboard only after it. Only this view's turn:
			// another admin view may hold control now under the same
			// holder.
			done, err = v.h.m.HumanInputTurn(v.session, v.holder, v.turn)
		}
		if !v.control || err != nil {
			v.control = false
			if m.Type == "clipboard" || m.Type == "clipboard-set" {
				say(map[string]any{"type": "clipboard-error", "id": m.ID, "msg": "You don't have control."})
			}
			say(map[string]any{"type": "control", "on": false})
			return
		}
		defer done()
		switch m.Type {
		case "clipboard":
			text, err := v.do(http.MethodGet, "/input/clipboard", nil, http.StatusOK, controlWait)
			if err != nil {
				say(map[string]any{"type": "clipboard-error", "id": m.ID, "msg": "Couldn't read the browser's clipboard: " + err.Error()})
				return
			}
			say(map[string]any{"type": "clipboard", "id": m.ID, "text": string(text)})
		case "clipboard-set":
			v.setClipboard(m.ID, m.Text, m.Paste, say)
		default:
			path, body := "/input/events", []byte(m.Events)
			if m.Type == "text" {
				path = "/input/type"
				// A person typing: at once, not at an agent's human pace.
				body, _ = json.Marshal(map[string]string{"text": m.Text, "pace": "instant"})
			}
			if err := v.post(path, body); err != nil {
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
func (v *viewer) setClipboard(id int, text string, paste bool, say func(any)) {
	fail := func(msg string) { say(map[string]any{"type": "clipboard-error", "id": id, "msg": msg}) }
	n := utf8.RuneCountInString(text)
	switch {
	case n == 0:
		fail("There's no text to send.")
		return
	case n > maxClipboardSet:
		fail(fmt.Sprintf("That's %d characters: at most %d can be sent at once.", n, maxClipboardSet))
		return
	}
	path, what := "/input/clipboard", "Clipboard set"
	if paste {
		path, what = "/input/paste", "Pasted from the clipboard box"
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	if err := v.post(path, body); err != nil {
		fail("Couldn't send it to the browser: " + err.Error())
		return
	}
	v.h.m.NoteControl(v.session, what)
	say(map[string]any{"type": "clipboard-set", "id": id})
}

// handBack runs when the viewer goes: control returns to agents with no
// key or button left held.
func (v *viewer) handBack() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.releaseLocked()
}

// releaseLocked lets go of this viewer's control, if it still has it:
// one that lost it behind its back releases nothing of the next person's.
// The hand-back itself lets go of held keys and buttons (ReleaseTurn).
func (v *viewer) releaseLocked() {
	if !v.holdsLocked() {
		return
	}
	v.control = false
	v.h.m.ReleaseTurn(v.session, v.turn)
}

func (v *viewer) post(path string, body []byte) error {
	_, err := v.do(http.MethodPost, path, body, http.StatusNoContent, controlWait)
	return err
}

// controlWait bounds a call to the container's control socket.
const controlWait = 10 * time.Second

// maxClipboard caps what a clipboard read brings back; the container caps
// it too.
const maxClipboard = 32 << 10

// do calls the container's control socket, wanting status want, within
// wait, and returns the answer's body.
func (v *viewer) do(method, path string, body []byte, want int, wait time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
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
	resp, err := c.Do(req) //nolint:gosec // as above
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, errors.New(string(bytes.TrimSpace(b)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxClipboard+1))
	if err == nil && len(b) > maxClipboard {
		err = fmt.Errorf("over %d kB", maxClipboard>>10)
	}
	return b, err
}
