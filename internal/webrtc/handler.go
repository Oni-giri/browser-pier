package webrtc

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

//go:embed live.html
var liveHTML string

//go:embed live.js
var liveJS []byte

var livePage = template.Must(template.New("live").Parse(liveHTML))

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

// Register adds /live/{profile}, its status and offer, and the client script.
func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.web.RequireAdmin(f) }
	mux.Handle("GET /live/{profile}", admin(h.page))
	mux.Handle("GET /live/{profile}/status", admin(h.status))
	mux.Handle("POST /live/{profile}/offer", admin(h.offer))
	mux.HandleFunc("GET /static/live.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(liveJS)
	})
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	profile := r.PathValue("profile")
	if _, err := h.m.Profiles().Get(r.Context(), profile); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'")
	_ = livePage.Execute(w, map[string]string{"Profile": profile})
}

type statusJSON struct {
	State        string   `json:"state"`
	InUseBy      []string `json:"in_use_by,omitempty"`
	HumanControl bool     `json:"human_control"`
	Viewers      int      `json:"viewers"`
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	out := statusJSON{State: string(sessions.Stopped)}
	if s, ok := h.m.Live(r.PathValue("profile")); ok {
		out = statusJSON{State: string(s.State), InUseBy: s.InUseBy, HumanControl: s.HumanControl, Viewers: s.Viewers}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
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
	vw := &viewer{h: h, session: s.ID, sock: h.m.Layout().ControlSocket(profile), allowed: o.Allowed}
	// A viewer promised control gets it or isn't connected: a page that
	// thinks it drives while it only watches would send input into nothing.
	if o.Control {
		if err := h.m.TakeControl(s.ID); err != nil {
			unwatch()
			msg := "The browser isn't running."
			switch {
			case errors.Is(err, sessions.ErrHumanDriving):
				msg = "Someone else has control."
			case errors.Is(err, sessions.ErrAgentDriving):
				msg = "The agent is driving."
			}
			http.Error(w, msg, http.StatusConflict)
			return
		}
		vw.control = true
	}
	control := vw.control // read before the data channel can change it
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	v, answer, err := h.relay.Connect(ctx, profile, in.SDP, vw.message)
	if err != nil {
		if control {
			h.m.ReleaseControl(s.ID) // nothing was pressed yet: no need to lift keys
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
	sock    string

	allowed func() bool // nil: always
	conn    *Viewer

	mu        sync.Mutex
	control   bool
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
func (v *viewer) message(msg []byte, reply func(string)) {
	var m struct {
		Type   string          `json:"type"`
		Events json.RawMessage `json:"events"`
		Text   string          `json:"text"`
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
		if v.control {
			say(map[string]any{"type": "control", "on": true})
			return
		}
		err := v.h.m.TakeControl(v.session)
		switch {
		case errors.Is(err, sessions.ErrHumanDriving):
			say(map[string]any{"type": "error", "msg": "Someone else has control."})
		case err == nil:
			v.control = true
			say(map[string]any{"type": "control", "on": true})
		case errors.Is(err, sessions.ErrAgentDriving):
			say(map[string]any{"type": "error", "msg": "The agent is driving. You can take control when it pauses or asks for help."})
		default:
			say(map[string]any{"type": "error", "msg": "The browser isn't running."})
		}
	case "release":
		v.releaseLocked()
		say(map[string]any{"type": "control", "on": false})
	case "lift":
		if v.control {
			_ = v.post("/input/release", nil)
		}
	case "input", "text":
		if !v.control || v.h.m.HumanInput(v.session) != nil {
			v.control = false
			say(map[string]any{"type": "control", "on": false})
			return
		}
		path, body := "/input/events", []byte(m.Events)
		if m.Type == "text" {
			path = "/input/type"
			body, _ = json.Marshal(map[string]string{"text": m.Text})
		}
		if err := v.post(path, body); err != nil {
			say(map[string]any{"type": "error", "msg": err.Error()})
		}
	}
}

// handBack runs when the viewer goes: control returns to agents with no
// key or button left held.
func (v *viewer) handBack() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.releaseLocked()
}

func (v *viewer) releaseLocked() {
	if !v.control {
		return
	}
	v.control = false
	_ = v.post("/input/release", nil)
	v.h.m.ReleaseControl(v.session)
}

func (v *viewer) post(path string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", v.sock)
	}}}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://control"+path, bytes.NewReader(body)) //nolint:gosec // a Unix socket and fixed paths
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req) //nolint:gosec // as above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return errors.New(string(bytes.TrimSpace(b)))
	}
	return nil
}
