package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// Call ids (PROJ-156): the actions that change the page take an optional
// Idempotency-Key, so that a retried call isn't run twice. The receipts
// and the rule are sessions' (calls.go); this is the HTTP side: which
// routes take the header, and the answers to a repeat.
const (
	// IdempotencyHeader carries a call id: 1 to 64 letters, digits, - or _.
	IdempotencyHeader = "Idempotency-Key"
	// ReplayedHeader marks the answer to a call sent again: it wasn't
	// run again, and says how the first one went.
	ReplayedHeader = "Idempotency-Replayed"
)

// maxRecentCalls is how many of its recent activity entries GET
// /api/sessions/{id} gives a token.
const maxRecentCalls = 20

// agentCall makes an agent route of h that takes a call id, as tool: a
// route that changes the page. TestEveryRouteChoosesOnCallIDs checks
// which routes do.
func (a *API) agentCall(tool string, h http.HandlerFunc) http.Handler {
	return agentRoute{h: a.web.RequireToken(a.scrubbing(a.takingCallID(tool, h))), tool: tool}
}

// refusingCallID runs h as a call without an id, refusing the header: a
// route that doesn't take one says so, so an agent never believes it is
// protected when it isn't. The call, named after the route, shows the
// token's page reads in running_calls and its activity entries in
// recent_calls.
func (a *API) refusingCallID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values(IdempotencyHeader)) > 0 {
			writeError(w, http.StatusBadRequest, "no call id is taken here: "+IdempotencyHeader+" is for the actions that change the page (click, type, computer, act, navigate, POST tabs); send this request without it")
			return
		}
		tok, _ := auth.TokenFromContext(r.Context())
		c := a.m.NewCall(tok.ID, tok.Name, routeTool(r.Pattern))
		h.ServeHTTP(w, r.WithContext(sessions.WithCall(r.Context(), c)))
	})
}

// routeTool names a route by its pattern's last fixed segment:
// "POST /api/sessions/{id}/snapshot" is snapshot.
func routeTool(pattern string) string {
	path := pattern
	if _, p, ok := strings.Cut(pattern, " "); ok {
		path = p
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] != "" && !strings.HasPrefix(segs[i], "{") {
			return segs[i]
		}
	}
	return ""
}

// takingCallID runs h as a call to tool, with the request's call id if it
// has one. The receipt is taken before h runs, so before the call waits
// for its turn: a repeat of a call already taken is answered from its
// receipt and h doesn't run. Whether h's call was admitted is decided by
// its lease (sessions.Call), not by its status: a call refused before it
// gives the id back.
func (a *API) takingCallID(tool string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := auth.TokenFromContext(r.Context())
		ids := r.Header.Values(IdempotencyHeader)
		if len(ids) == 0 {
			c := a.m.NewCall(tok.ID, tok.Name, tool)
			h.ServeHTTP(w, r.WithContext(sessions.WithCall(r.Context(), c)))
			return
		}
		if len(ids) > 1 || !sessions.ValidCallID(ids[0]) {
			writeError(w, http.StatusBadRequest, IdempotencyHeader+" (MCP: call_id): want one call id of 1 to 64 letters, digits, - or _, new for each action; leave it out for none")
			return
		}
		id := ids[0]
		s, ok := a.session(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		c, prev, err := a.m.TakeCall(s.ID, tok.ID, tok.Name, tool, id, sessions.FingerprintOf(tool, body))
		switch {
		case prev != nil:
			a.replay(w, s, *prev)
			return
		case errors.Is(err, sessions.ErrOtherCall):
			writeError(w, http.StatusConflict, err.Error()+"; nothing was run")
			return
		case errors.Is(err, sessions.ErrTooManyCalls):
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		case err != nil:
			// Not live: the route refuses it as it would without an id,
			// and there is nothing to keep a receipt in.
			c = a.m.NewCall(tok.ID, tok.Name, tool)
		}
		sw := &statusWriter{ResponseWriter: w}
		ended := false
		defer func() {
			if !ended {
				c.EndPanicked()
			}
		}()
		h.ServeHTTP(sw, r.WithContext(sessions.WithCall(r.Context(), c)))
		ended = true
		c.End(sw.ok())
	})
}

// statusWriter records the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// ok: the handler answered success. One that wrote nothing (its caller
// went away) didn't.
func (s *statusWriter) ok() bool { return s.code >= 200 && s.code < 300 }

// callJSON is a call's receipt for the agent. Never its arguments or its
// answer: they aren't kept.
type callJSON struct {
	CallID  string     `json:"call_id"`
	Tool    string     `json:"tool"`
	State   string     `json:"state"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
}

func callOut(r sessions.Receipt) callJSON {
	out := callJSON{CallID: r.CallID, Tool: r.Tool, State: string(r.State), Started: r.Started.UTC()}
	if !r.Ended.IsZero() {
		t := r.Ended.UTC()
		out.Ended = &t
	}
	return out
}

// replay answers a call sent again with how the first one went: success
// only if it succeeded. Its result isn't kept, so neither a screenshot,
// a snapshot nor a download notice is given again.
func (a *API) replay(w http.ResponseWriter, s sessions.Session, r sessions.Receipt) {
	w.Header().Set(ReplayedHeader, "true")
	out := map[string]any{"call": callOut(r)}
	status := http.StatusConflict
	what := r.Tool + " call " + r.CallID
	switch r.State {
	case sessions.CallOK:
		status = http.StatusOK
		out["text"] = "This " + what + " ran already and ended ok; it wasn't run again. Its result isn't kept: read the page again (snapshot, or a screenshot in Visual mode) to see where things are."
	case sessions.CallWaiting:
		out["error"] = "This " + what + " is waiting for its turn; it wasn't run again. Ask GET /api/sessions/" + s.ID + "/calls/" + r.CallID + " (MCP: call_status) how it went. A 404 there doesn't prove it never ran: it may have been given back unrun, or have run and had its receipt dropped since (1,000 calls later, or the browser restarted). Read the page before sending it again."
	case sessions.CallRunning:
		out["error"] = "This " + what + " is still running; it wasn't run again. Ask GET /api/sessions/" + s.ID + "/calls/" + r.CallID + " (MCP: call_status) how it ended. A 404 there doesn't prove it never ran: a type that failed before any key was pressed is given back unrun, and a call that ran may have had its receipt dropped since (1,000 calls later, or the browser restarted). Read the page before sending it again."
	case sessions.CallInterrupted:
		out["error"] = "This " + what + " ran already and was interrupted (a person took the browser, it stopped, or its caller went away); it wasn't run again. How far it got isn't known: read the page, then send what is still needed under a new call id."
	default:
		out["error"] = "This " + what + " ran already and ended with an error; it wasn't run again. Some of it may have happened: read the page, then send what is still needed under a new call id."
	}
	writeJSON(w, status, out)
}

// callStatus answers GET /api/sessions/{id}/calls/{call_id}: the token's
// own receipt for a call id, or 404.
func (a *API) callStatus(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	id := r.PathValue("call_id")
	if !sessions.ValidCallID(id) {
		writeError(w, http.StatusBadRequest, "call_id: want 1 to 64 letters, digits, - or _")
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	rec, ok := a.m.CallReceipt(s.ID, tok.ID, id)
	if !ok {
		writeError(w, http.StatusNotFound, "no call with this id in this session: it was never taken, or was given back (as a call refused before it ran is), or its receipt was dropped as the 1000th after it ended, or the browser has restarted since. How it went isn't known: read the page before sending it again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"call": callOut(rec)})
}

// runningCallJSON is one of the token's calls under way.
type runningCallJSON struct {
	Tool    string    `json:"tool"`
	CallID  string    `json:"call_id,omitempty"`
	State   string    `json:"state"`
	Started time.Time `json:"started"`
}

// recentCallJSON is one of the token's recent activity entries.
type recentCallJSON struct {
	What   string    `json:"what"`
	At     time.Time `json:"at"`
	Tool   string    `json:"tool,omitempty"`
	CallID string    `json:"call_id,omitempty"`
	State  string    `json:"state,omitempty"`
}

// sessionStatusJSON is GET /api/sessions/{id}: the session, and the
// token's own calls on it.
type sessionStatusJSON struct {
	sessionJSON
	RunningCalls []runningCallJSON `json:"running_calls"`
	RecentCalls  []recentCallJSON  `json:"recent_calls"`
}

func (a *API) sessionStatus(r *http.Request, s sessions.Session) sessionStatusJSON {
	tok, _ := auth.TokenFromContext(r.Context())
	out := sessionStatusJSON{sessionJSON: a.sessionOut(r, s, false), RunningCalls: []runningCallJSON{}, RecentCalls: []recentCallJSON{}}
	for _, c := range a.m.RunningCalls(s.ID, tok.ID) {
		out.RunningCalls = append(out.RunningCalls, runningCallJSON{Tool: c.Tool, CallID: c.CallID, State: string(c.State), Started: c.Started.UTC()})
	}
	for _, c := range a.m.RecentCalls(s.ID, tok.ID, maxRecentCalls) {
		what := c.What
		if c.FromPage {
			// A downloaded file's name, which the page chose.
			what = fence.Wrap("activity entry naming a download", what)
		}
		out.RecentCalls = append(out.RecentCalls, recentCallJSON{What: what, At: c.At.UTC(), Tool: c.Tool, CallID: c.CallID, State: string(c.State)})
	}
	return out
}
