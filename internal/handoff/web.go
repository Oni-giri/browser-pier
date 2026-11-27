package handoff

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/picker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/static"
	"github.com/Oni-giri/browser-pier/internal/theme"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

//go:embed handoff.html
var pageHTML string

//go:embed handoff.js
var pageJS []byte

var pageJSTag = static.Tag(pageJS)

var page = template.Must(template.New("handoff").Funcs(template.FuncMap{"initials": initials, "shorten": shorten}).Parse(pageHTML))

// maxShownUntil is how much of an agent's until_url pattern the page
// shows, in characters: it is the agent's text, and a phone is narrow.
const maxShownUntil = 80

// shorten cuts s to maxShownUntil characters, with an ellipsis when it
// was longer. The template escapes what it returns.
func shorten(s string) string {
	if r := []rune(s); len(r) > maxShownUntil {
		return string(r[:maxShownUntil-1]) + "…"
	}
	return s
}

// initials are up to two letters for the agent's badge: "research-bot" → "RB".
func initials(name string) string {
	out := ""
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '.' }) {
		out += strings.ToUpper(string([]rune(part)[0]))
		if len([]rune(out)) == 2 {
			break
		}
	}
	return out
}

// browserCookie marks the one browser a handoff link was opened in.
const browserCookie = "bp_handoff"

// Web serves handoff links: /h/{token}.
type Web struct {
	svc  *Service
	web  *auth.Web
	live *webrtc.Handler
	m    *sessions.Manager
	log  *slog.Logger
	pin  *auth.Lockout // per client address, on top of the link's own five tries
	// pinProfile caps wrong handoff PINs per profile, across its links and
	// addresses, so someone holding several links can't spread guesses
	// across them; it doesn't block other profiles' handoffs.
	pinProfile *auth.Lockout
	// pinAll caps wrong PINs across every profile: the PIN is shared, so
	// links for many profiles mustn't add up to many guesses at it. Its
	// limit is higher, so one profile's attacker rarely blocks the rest.
	pinAll *auth.Lockout
	// picker answers a page's file chooser for the link's holder
	// (PROJ-181); nil refuses it.
	picker *picker.Picker
}

// SetPicker lets the link's holder answer the page's file choosers.
func (w *Web) SetPicker(p *picker.Picker) { w.picker = p }

func NewWeb(svc *Service, web *auth.Web, live *webrtc.Handler, m *sessions.Manager, log *slog.Logger) *Web {
	return &Web{svc: svc, web: web, live: live, m: m, log: log,
		pin:        auth.NewLockout(10, 15*time.Minute, 15*time.Minute),
		pinProfile: auth.NewLockout(15, 15*time.Minute, 15*time.Minute),
		pinAll:     auth.NewLockout(30, 15*time.Minute, 15*time.Minute)}
}

// Register adds the handoff routes.
func (w *Web) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /h/{token}", w.show)
	mux.HandleFunc("POST /h/{token}/pin", w.enterPIN)
	mux.HandleFunc("GET /h/{token}/ice", w.ice)
	mux.HandleFunc("POST /h/{token}/offer", w.offer)
	mux.HandleFunc("GET /h/{token}/status", w.status)
	mux.HandleFunc("POST /h/{token}/finish", w.finish)
	mux.HandleFunc("GET /h/{token}/chooser", w.chooser(func(p *picker.Picker) chooserRoute { return p.Show }))
	mux.HandleFunc("POST /h/{token}/chooser", w.chooser(func(p *picker.Picker) chooserRoute { return p.Answer }))
	mux.HandleFunc("POST /h/{token}/chooser/upload", w.chooser(func(p *picker.Picker) chooserRoute { return p.Upload }))
	mux.HandleFunc("GET /static/handoff.js", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		static.Header(rw, r, pageJSTag)
		http.ServeContent(rw, r, "handoff.js", time.Time{}, bytes.NewReader(pageJS))
	})
}

type view struct {
	Step      string // pin, control, other, ended
	H         Handoff
	Token     string
	Reason    string
	Error     string
	TriesLeft int
	Ended     string // done, auto, cant, cancelled, expired, missing, insecure
	ExpiresMs int64
	// Width and Height are the browser's screen, which the live view
	// takes the shape of.
	Width, Height int
	Theme         theme.Theme
	// Records: the profile records agents' sessions (PROJ-23); the page
	// says recording is paused while the person has control.
	Records bool
}

func (w *Web) render(rw http.ResponseWriter, r *http.Request, status int, v view) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	// Other sites never see the link. Not no-referrer, under which the PIN
	// form posts "Origin: null", refused over plain HTTP.
	rw.Header().Set("Referrer-Policy", "same-origin")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; media-src 'self' blob:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	rw.WriteHeader(status)
	v.Reason = v.H.Reason.Label()
	v.Theme = theme.Of(r)
	v.ExpiresMs = v.H.ExpiresAt.UnixMilli()
	if err := page.Execute(rw, v); err != nil {
		w.log.Error("rendering the handoff page", "err", err)
	}
}

// load finds the link's handoff, or renders why it doesn't work.
func (w *Web) load(rw http.ResponseWriter, r *http.Request) (Handoff, string, bool) {
	token := r.PathValue("token")
	h, err := w.svc.ByToken(r.Context(), token)
	if errors.Is(err, ErrNotFound) {
		w.render(rw, r, http.StatusNotFound, view{Step: "ended", Ended: "missing"})
		return h, token, false
	}
	if err != nil {
		http.Error(rw, "something went wrong", http.StatusInternalServerError)
		return h, token, false
	}
	if h.Status.Final() {
		w.render(rw, r, http.StatusGone, view{Step: "ended", Ended: ended(h), H: h})
		return h, token, false
	}
	return h, token, true
}

// ended is how the page names the way a handoff ended: auto when the page
// reached the agent's until_url.
func ended(h Handoff) string {
	if h.Status == Done && h.Auto {
		return "auto"
	}
	return string(h.Status)
}

func (w *Web) holder(r *http.Request, h Handoff) bool {
	c, err := r.Cookie(browserCookie)
	return err == nil && w.svc.Unlocked(r.Context(), h, c.Value)
}

// insecure reports whether the link was opened over plain HTTP from
// outside the LAN or Tailscale. Such a request is asked for nothing and
// unlocks nothing: a PIN or password would travel in the clear, and so
// would the cookie that holds the browser.
func (w *Web) insecure(rw http.ResponseWriter, r *http.Request, h Handoff) bool {
	if w.web.IsHTTPS(r) || private(w.web.ClientIP(r)) {
		return false
	}
	w.render(rw, r, http.StatusForbidden, view{Step: "ended", Ended: "insecure", H: h})
	return true
}

func private(ip string) bool {
	a := net.ParseIP(ip)
	if a == nil {
		return false
	}
	if a.IsLoopback() || a.IsPrivate() {
		return true
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10") // Tailscale
	return cgnat.Contains(a)
}

// unlock gives this browser the handoff.
func (w *Web) unlock(rw http.ResponseWriter, r *http.Request, h Handoff, token string) bool {
	secret := randomToken()
	if err := w.svc.Unlock(r.Context(), h, secret); err != nil {
		w.render(rw, r, http.StatusForbidden, view{Step: "other", H: h})
		return false
	}
	//nolint:gosec // Secure as the sign-in cookie is (CookieSecure, HTTPS, an https public URL)
	http.SetCookie(rw, &http.Cookie{Name: browserCookie, Value: secret, Path: "/h/" + token, HttpOnly: true,
		Secure: w.web.SecureCookie(r), SameSite: http.SameSiteStrictMode, Expires: h.ExpiresAt})
	return true
}

func (w *Web) show(rw http.ResponseWriter, r *http.Request) {
	h, token, ok := w.load(rw, r)
	if !ok || w.insecure(rw, r, h) {
		return
	}
	if w.holder(r, h) {
		v := view{Step: "control", H: h, Token: token, Width: 16, Height: 10}
		if p, err := w.m.Profiles().Get(r.Context(), h.Profile); err == nil {
			v.Width, v.Height, v.Records = p.Width, p.Height, p.Record
		}
		w.render(rw, r, http.StatusOK, v)
		return
	}
	if h.unlocked {
		w.render(rw, r, http.StatusForbidden, view{Step: "other", H: h})
		return
	}
	switch h.Policy {
	case profiles.PolicyLink:
		if w.unlock(rw, r, h, token) {
			http.Redirect(rw, r, "/h/"+url.PathEscape(token), http.StatusSeeOther)
		}
	case profiles.PolicySignIn:
		if !w.web.SignedIn(r) {
			http.Redirect(rw, r, "/login?next="+url.QueryEscape("/h/"+url.PathEscape(token)), http.StatusSeeOther)
			return
		}
		if w.unlock(rw, r, h, token) {
			http.Redirect(rw, r, "/h/"+url.PathEscape(token), http.StatusSeeOther)
		}
	default:
		w.render(rw, r, http.StatusOK, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures})
	}
}

func (w *Web) enterPIN(rw http.ResponseWriter, r *http.Request) {
	h, token, ok := w.load(rw, r)
	if !ok || w.insecure(rw, r, h) {
		return
	}
	if h.unlocked {
		w.render(rw, r, http.StatusForbidden, view{Step: "other", H: h})
		return
	}
	// The PIN only opens a link whose policy asks for it: a "full sign-in"
	// link can't be opened with the PIN instead of the admin password.
	if h.Policy != profiles.PolicyPIN {
		http.Redirect(rw, r, "/h/"+url.PathEscape(token), http.StatusSeeOther)
		return
	}
	ip := w.web.ClientIP(r)
	mine, _ := w.pin.Begin(ip)
	if mine == nil {
		w.render(rw, r, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Too many wrong PINs from here. Wait a few minutes."})
		return
	}
	prof, _ := w.pinProfile.Begin(h.Profile)
	if prof == nil {
		mine.Cancel()
		w.log.Warn("handoff PINs locked for a profile: too many wrong PINs across its links", "profile", h.Profile)
		w.render(rw, r, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Too many wrong PINs. Wait a few minutes."})
		return
	}
	glob, _ := w.pinAll.Begin("")
	if glob == nil {
		mine.Cancel()
		prof.Cancel()
		w.log.Warn("handoff PINs locked everywhere: too many wrong PINs across all profiles")
		w.render(rw, r, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Too many wrong PINs. Wait a few minutes."})
		return
	}
	all := attempts{prof, glob}
	// Reserve this link's try before checking, so parallel guesses can't
	// all get in before the count does. A refusal isn't a wrong PIN.
	reserved, err := w.svc.ReservePINTry(r.Context(), h)
	if err != nil || !reserved {
		mine.Cancel()
		all.Cancel()
		if err == nil {
			h, err = w.svc.Get(r.Context(), h.ID)
		}
		switch {
		case err != nil:
			http.Error(rw, "something went wrong", http.StatusInternalServerError)
		case h.Status.Final():
			w.render(rw, r, http.StatusGone, view{Step: "ended", Ended: ended(h), H: h})
		default:
			w.render(rw, r, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Wait for your other attempts to finish."})
		}
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	ok, err = w.web.Admin().CheckPIN(r.Context(), r.PostFormValue("pin"))
	if err != nil {
		mine.Cancel()
		all.Cancel()
		w.svc.PINRight(h) // not a guess: give the try back
		http.Error(rw, "something went wrong", http.StatusInternalServerError)
		return
	}
	if !ok {
		mine.Fail()
		all.Fail()
		left, err := w.svc.PINWrong(r.Context(), h)
		if err != nil {
			http.Error(rw, "something went wrong", http.StatusInternalServerError)
			return
		}
		w.log.Warn("wrong handoff PIN", "handoff", h.ID, "ip", ip, "tries_left", left)
		if left <= 0 {
			w.render(rw, r, http.StatusGone, view{Step: "ended", Ended: string(Cancelled), H: h})
			return
		}
		w.render(rw, r, http.StatusUnauthorized, view{Step: "pin", H: h, Token: token, TriesLeft: left, Error: "Wrong PIN."})
		return
	}
	mine.Succeed()
	all.Cancel() // a right PIN doesn't clear wrong guesses made at other links
	w.svc.PINRight(h)
	if w.unlock(rw, r, h, token) {
		http.Redirect(rw, r, "/h/"+url.PathEscape(token), http.StatusSeeOther)
	}
}

// ice gives the holder the live view's ICE servers, with TURN credentials
// that open no new relays after the link has expired. The handoff is the
// viewer, so asking again and again doesn't get it more of the TURN
// server than its quota.
func (w *Web) ice(rw http.ResponseWriter, r *http.Request) {
	h, err := w.svc.ByToken(r.Context(), r.PathValue("token"))
	if err != nil || h.Status.Final() || !w.holder(r, h) {
		http.Error(rw, "this link isn't open here", http.StatusForbidden)
		return
	}
	w.live.ICE(rw, r, h.ExpiresAt.Add(time.Minute), "handoff "+h.ID)
}

// offer connects the live view; the holder has control from the start.
func (w *Web) offer(rw http.ResponseWriter, r *http.Request) {
	h, err := w.svc.ByToken(r.Context(), r.PathValue("token"))
	if err != nil || h.Status.Final() || !w.holder(r, h) {
		http.Error(rw, "this link isn't open here", http.StatusForbidden)
		return
	}
	// Which time the browser went to Needs help, read while the handoff
	// is open: read first, then the handoff found still open, so it is
	// this handoff's (or a later one's, which only refuses). The take
	// names it, and fails in the same step if the browser no longer
	// waits for that help: a handoff ending while this offer is under
	// way takes nothing back from the agent (PROJ-196).
	epoch, ok := w.m.HelpEpoch(h.SessionID)
	if cur, err := w.svc.Get(r.Context(), h.ID); !ok || err != nil || cur.Status != Pending {
		http.Error(rw, "this link isn't open here", http.StatusForbidden)
		return
	}
	// This browser holds the link, so an earlier view of it is a page
	// since reloaded: replace it rather than refuse "someone else has control".
	w.svc.replaceViewers(r.Context(), h)
	id := h.ID
	w.live.Offer(rw, r, h.Profile, webrtc.OfferOptions{
		Holder:  sessions.HandoffHolder(id),
		Control: true,
		// Only while the handoff is open: hand-back ends this viewer's
		// control. A handoff ending meanwhile (ErrHandingBack once the
		// wait is up) is no longer open.
		Allowed: func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cur, err := w.svc.Get(ctx, id)
			return err == nil && cur.Status == Pending
		},
		Opened: func(v *webrtc.Viewer) { w.svc.track(id, v) },
		// The session this link is for: if it ended and another started
		// on the profile meanwhile, the offer is refused, and that one is
		// never taken.
		Session:   h.SessionID,
		Handoff:   true,
		HelpEpoch: epoch,
	})
}

type chooserRoute = func(http.ResponseWriter, *http.Request, sessions.Session, string)

// chooser serves the handoff page's file chooser routes (PROJ-181): only
// to the browser holding the link, only while it is open, and only for
// the link's own session, so its profile's uploads and nothing else. The
// picker checks that the link holds control of it, not the admin's live
// view (PROJ-184).
func (w *Web) chooser(route func(*picker.Picker) chooserRoute) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		h, err := w.svc.ByToken(r.Context(), r.PathValue("token"))
		if err != nil || h.Status.Final() || !w.holder(r, h) {
			http.Error(rw, "This link isn't open here.", http.StatusForbidden)
			return
		}
		s, ok := w.m.Live(h.Profile)
		if !ok || s.ID != h.SessionID {
			http.Error(rw, "The browser isn't running.", http.StatusConflict)
			return
		}
		route(w.picker)(rw, r, s, sessions.HandoffHolder(h.ID))
	}
}

func (w *Web) status(rw http.ResponseWriter, r *http.Request) {
	h, err := w.svc.ByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		http.Error(rw, "no such link", http.StatusNotFound)
		return
	}
	if !h.Status.Final() && !w.holder(r, h) {
		http.Error(rw, "this link isn't open here", http.StatusForbidden)
		return
	}
	out := map[string]any{"handoff": h.Status, "state": string(sessions.Stopped), "expires_ms": h.ExpiresAt.UnixMilli()}
	if s, ok := w.m.Live(h.Profile); ok {
		out["state"], out["human_control"] = string(s.State), s.HumanControl
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(rw).Encode(out)
}

func (w *Web) finish(rw http.ResponseWriter, r *http.Request) {
	h, _, ok := w.load(rw, r)
	if !ok {
		return
	}
	if !w.holder(r, h) {
		w.render(rw, r, http.StatusForbidden, view{Step: "other", H: h})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 8<<10)
	status := Done
	if r.PostFormValue("status") == string(Cant) {
		status = Cant
	}
	h, err := w.svc.Finish(r.Context(), h, status, r.PostFormValue("note"))
	if errors.Is(err, ErrHandingBack) {
		if r.Context().Err() == nil {
			http.Error(rw, "Handing back: the browser hasn't answered yet. Reload in a moment.", http.StatusServiceUnavailable)
		}
		return
	}
	if err != nil {
		http.Error(rw, "something went wrong", http.StatusInternalServerError)
		return
	}
	w.render(rw, r, http.StatusOK, view{Step: "ended", Ended: ended(h), H: h})
}

// attempts ends several lockout attempts the same way.
type attempts []*auth.Attempt

func (as attempts) Fail() {
	for _, a := range as {
		a.Fail()
	}
}

func (as attempts) Cancel() {
	for _, a := range as {
		a.Cancel()
	}
}
