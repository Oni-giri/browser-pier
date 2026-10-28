package handoff

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

//go:embed handoff.html
var pageHTML string

//go:embed handoff.js
var pageJS []byte

var page = template.Must(template.New("handoff").Parse(pageHTML))

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
}

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
	mux.HandleFunc("POST /h/{token}/offer", w.offer)
	mux.HandleFunc("GET /h/{token}/status", w.status)
	mux.HandleFunc("POST /h/{token}/finish", w.finish)
	mux.HandleFunc("GET /static/handoff.js", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-cache")
		_, _ = rw.Write(pageJS)
	})
}

type view struct {
	Step      string // pin, control, other, ended
	H         Handoff
	Token     string
	Reason    string
	Error     string
	TriesLeft int
	Ended     string // done, cant, cancelled, expired, missing, insecure
	ExpiresMs int64
}

func (w *Web) render(rw http.ResponseWriter, status int, v view) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; media-src 'self' blob:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	rw.WriteHeader(status)
	v.Reason = v.H.Reason.Label()
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
		w.render(rw, http.StatusNotFound, view{Step: "ended", Ended: "missing"})
		return h, token, false
	}
	if err != nil {
		http.Error(rw, "something went wrong", http.StatusInternalServerError)
		return h, token, false
	}
	if h.Status.Final() {
		w.render(rw, http.StatusGone, view{Step: "ended", Ended: string(h.Status), H: h})
		return h, token, false
	}
	return h, token, true
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
	w.render(rw, http.StatusForbidden, view{Step: "ended", Ended: "insecure", H: h})
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
		w.render(rw, http.StatusForbidden, view{Step: "other", H: h})
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
		w.render(rw, http.StatusOK, view{Step: "control", H: h, Token: token})
		return
	}
	if h.unlocked {
		w.render(rw, http.StatusForbidden, view{Step: "other", H: h})
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
		w.render(rw, http.StatusOK, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures})
	}
}

func (w *Web) enterPIN(rw http.ResponseWriter, r *http.Request) {
	h, token, ok := w.load(rw, r)
	if !ok || w.insecure(rw, r, h) {
		return
	}
	if h.unlocked {
		w.render(rw, http.StatusForbidden, view{Step: "other", H: h})
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
		w.render(rw, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Too many wrong PINs from here. Wait a few minutes."})
		return
	}
	prof, _ := w.pinProfile.Begin(h.Profile)
	if prof == nil {
		mine.Cancel()
		w.log.Warn("handoff PINs locked for a profile: too many wrong PINs across its links", "profile", h.Profile)
		w.render(rw, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Too many wrong PINs. Wait a few minutes."})
		return
	}
	glob, _ := w.pinAll.Begin("")
	if glob == nil {
		mine.Cancel()
		prof.Cancel()
		w.log.Warn("handoff PINs locked everywhere: too many wrong PINs across all profiles")
		w.render(rw, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: MaxPINFailures - h.PINFailures, Error: "Too many wrong PINs. Wait a few minutes."})
		return
	}
	all := attempts{prof, glob}
	// Reserve this link's try before checking, so parallel guesses can't
	// all get in before the count does.
	reserved, err := w.svc.ReservePINTry(r.Context(), h)
	if err != nil || !reserved {
		mine.Cancel()
		all.Cancel()
		if err != nil {
			http.Error(rw, "something went wrong", http.StatusInternalServerError)
			return
		}
		left, _ := w.svc.PINWrong(r.Context(), h)
		if left <= 0 {
			w.render(rw, http.StatusGone, view{Step: "ended", Ended: string(Cancelled), H: h})
		} else {
			w.render(rw, http.StatusTooManyRequests, view{Step: "pin", H: h, Token: token, TriesLeft: left, Error: "Wait for your other attempts to finish."})
		}
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	ok, err = w.web.Admin().CheckPIN(r.Context(), r.PostFormValue("pin"))
	if err != nil {
		mine.Cancel()
		all.Cancel()
		_ = w.svc.PINRight(r.Context(), h) // not a guess: give the try back
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
			w.render(rw, http.StatusGone, view{Step: "ended", Ended: string(Cancelled), H: h})
			return
		}
		w.render(rw, http.StatusUnauthorized, view{Step: "pin", H: h, Token: token, TriesLeft: left, Error: "Wrong PIN."})
		return
	}
	mine.Succeed()
	all.Cancel() // a right PIN doesn't clear wrong guesses made at other links
	_ = w.svc.PINRight(r.Context(), h)
	if w.unlock(rw, r, h, token) {
		http.Redirect(rw, r, "/h/"+url.PathEscape(token), http.StatusSeeOther)
	}
}

// offer connects the live view; the holder has control from the start.
func (w *Web) offer(rw http.ResponseWriter, r *http.Request) {
	h, err := w.svc.ByToken(r.Context(), r.PathValue("token"))
	if err != nil || h.Status.Final() || !w.holder(r, h) {
		http.Error(rw, "this link isn't open here", http.StatusForbidden)
		return
	}
	id := h.ID
	w.live.Offer(rw, r, h.Profile, webrtc.OfferOptions{
		Control: true,
		// Only while the handoff is open: hand-back ends this viewer's control.
		Allowed: func() bool {
			cur, err := w.svc.Get(context.Background(), id)
			return err == nil && cur.Status == Pending
		},
		Opened: func(v *webrtc.Viewer) { w.svc.track(id, v) },
	})
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
		w.render(rw, http.StatusForbidden, view{Step: "other", H: h})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 8<<10)
	status := Done
	if r.PostFormValue("status") == string(Cant) {
		status = Cant
	}
	h, err := w.svc.Finish(r.Context(), h, status, r.PostFormValue("note"))
	if err != nil {
		http.Error(rw, "something went wrong", http.StatusInternalServerError)
		return
	}
	w.render(rw, http.StatusOK, view{Step: "ended", Ended: string(h.Status), H: h})
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
