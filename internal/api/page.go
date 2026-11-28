package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/pagetools"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// The API-mode page tools (internal/pagetools), one endpoint each. They
// take the same JSON as the MCP tools, and answer {"text": "..."}, the
// same text the MCP tools return, with page content fenced.
func (a *API) registerPageTools(mux Router) {
	t, c := a.agent, a.agentCall
	mux.Handle("POST /api/sessions/{id}/snapshot", t(pageTool(a, (*pagetools.Tools).Snapshot)))
	mux.Handle("POST /api/sessions/{id}/find", t(pageTool(a, (*pagetools.Tools).Find)))
	mux.Handle("POST /api/sessions/{id}/navigate", c("navigate", pageTool(a, (*pagetools.Tools).Navigate)))
	mux.Handle("POST /api/sessions/{id}/act", c("act", pageToolNotice(a, (*pagetools.Tools).Act)))
	mux.Handle("POST /api/sessions/{id}/wait", t(pageTool(a, (*pagetools.Tools).WaitFor)))
	mux.Handle("GET /api/sessions/{id}/tabs", t(pageTool(a, (*pagetools.Tools).Tabs)))
	mux.Handle("POST /api/sessions/{id}/tabs", c("tabs", pageTool(a, (*pagetools.Tools).Tabs)))
	mux.Handle("POST /api/sessions/{id}/content", t(a.content))
}

// PageTools is the page tools the API runs, for tests.
func (a *API) PageTools() *pagetools.Tools { return a.tools }

type pageFunc[In any] func(*pagetools.Tools, context.Context, sessions.Session, pagetools.Caller, In) (string, error)

// pageTool serves one page tool: the session as the token may see it
// (404 otherwise), the input as JSON (an empty body is the defaults), and
// the tool's text or its error.
func pageTool[In any](a *API, f pageFunc[In]) http.HandlerFunc { return pageToolWith(a, f, false) }

// pageToolNotice is pageTool for a tool that acts (act): its text ends
// with the file chooser notice when a chooser is open after it.
func pageToolNotice[In any](a *API, f pageFunc[In]) http.HandlerFunc {
	return pageToolWith(a, f, true)
}

func pageToolWith[In any](a *API, f pageFunc[In], notice bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.session(w, r)
		if !ok {
			return
		}
		var in In
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body too large")
			return
		}
		if strings.TrimSpace(string(body)) != "" {
			if err := decodeOne(bytes.NewReader(body), &in); err != nil {
				writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
				return
			}
		}
		tok, _ := auth.TokenFromContext(r.Context())
		out, err := f(a.tools, r.Context(), s, pagetools.Caller{TokenID: tok.ID, Agent: tok.Name}, in)
		var pe *pagetools.Error
		switch {
		case errors.As(err, &pe):
			msg := pe.Msg
			// Not for a call that never ran, nor for a token revoked
			// under it, which is told nothing more (PROJ-206).
			if notice && !pe.NothingRan && pe.Status != http.StatusUnauthorized {
				// A step that failed after one that opened a chooser.
				if line := a.chooserNotice(r.Context(), s); line != "" {
					msg += "\n" + line
				}
			}
			writeError(w, pe.Status, msg)
			return
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return // the client went away
		case err != nil:
			a.pageInternal(w, s, "a page tool", err)
			return
		}
		if notice {
			if line := a.chooserNotice(r.Context(), s); line != "" {
				out += "\n" + line
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"text": out})
	}
}
