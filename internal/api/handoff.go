package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// SetHandoffs enables the handoff routes.
func (a *API) SetHandoffs(svc *handoff.Service) { a.handoffs = svc }

func (a *API) registerHandoffs(mux *http.ServeMux) {
	t := func(h http.HandlerFunc) http.Handler { return a.web.RequireToken(h) }
	mux.Handle("POST /api/sessions/{id}/handoff", t(a.createHandoff))
	mux.Handle("GET /api/handoffs/{id}", t(a.getHandoff))
}

type handoffJSON struct {
	ID        string    `json:"id"`
	Session   string    `json:"session_id"`
	Profile   string    `json:"profile"`
	Status    string    `json:"status"` // pending, done, cant, cancelled, expired
	Reason    string    `json:"reason"`
	Policy    string    `json:"policy"`
	Origin    string    `json:"origin,omitempty"`
	URL       string    `json:"url,omitempty"` // only when created
	ExpiresAt time.Time `json:"expires_at"`
	ExpiresIn int       `json:"expires_in,omitempty"` // seconds, only when created
	Note      string    `json:"note,omitempty"`       // the person's
	// Message says what to do next.
	Message string `json:"message"`
}

func handoffOut(h handoff.Handoff) handoffJSON {
	out := handoffJSON{ID: h.ID, Session: h.SessionID, Profile: h.Profile, Status: string(h.Status), Reason: string(h.Reason),
		Policy: string(h.Policy), Origin: h.Origin, ExpiresAt: h.ExpiresAt, Note: h.Note}
	switch h.Status {
	case handoff.Pending:
		out.Message = "Send the URL to the person through your own channel, then wait on GET /api/handoffs/{id}?wait=900. You are paused until they hand back."
	case handoff.Done:
		out.Message = "The person handed back. The page may have changed: re-read it before acting."
	case handoff.Cant:
		out.Message = "The person couldn't do it. Stop this task cleanly and report back. The page may have changed."
	case handoff.Cancelled:
		out.Message = "The handoff was cancelled (" + h.Note + "). You have the browser back; re-read the page before acting."
	case handoff.Expired:
		out.Message = "Nobody took the handoff within 15 minutes. You have the browser back; re-read the page before acting."
	}
	return out
}

type createHandoffJSON struct {
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
	Require string `json:"require"`
}

func (a *API) createHandoff(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	var in createHandoffJSON
	if !readJSON(w, r, &in) {
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	h, token, err := a.handoffs.Create(r.Context(), s.ID, tok.Name, handoff.Reason(in.Reason), in.Detail, profiles.HandoffPolicy(in.Require))
	switch {
	case errors.Is(err, handoff.ErrOpen):
		writeError(w, http.StatusConflict, "a handoff is already open for this browser")
		return
	case errors.Is(err, handoff.ErrNotRunning), errors.Is(err, sessions.ErrNotRunning):
		writeError(w, http.StatusConflict, "the session isn't running")
		return
	case errors.Is(err, handoff.ErrBadReason), errors.Is(err, handoff.ErrBadRequire):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		a.internal(w, "opening a handoff", err)
		return
	}
	out := handoffOut(h)
	out.URL = a.baseURL(r) + "/h/" + token
	out.ExpiresIn = int(handoff.Lifetime / time.Second)
	writeJSON(w, http.StatusCreated, out)
}

// maxWait caps a long-poll.
const maxWait = 900 * time.Second

func (a *API) getHandoff(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.TokenFromContext(r.Context())
	id := r.PathValue("id")
	h, err := a.handoffs.Get(r.Context(), id)
	if errors.Is(err, handoff.ErrNotFound) || (err == nil && !tok.Allows(h.Profile)) {
		writeError(w, http.StatusNotFound, "no such handoff")
		return
	}
	if err != nil {
		a.internal(w, "loading a handoff", err)
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			writeError(w, http.StatusBadRequest, "wait: want seconds")
			return
		}
		wait = min(time.Duration(secs)*time.Second, maxWait)
	}
	h, err = a.handoffs.Wait(r.Context(), id, wait)
	if err != nil {
		if r.Context().Err() != nil {
			return // the agent gave up waiting
		}
		a.internal(w, "waiting on a handoff", err)
		return
	}
	writeJSON(w, http.StatusOK, handoffOut(h))
}
