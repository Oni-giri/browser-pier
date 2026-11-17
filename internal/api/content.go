package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/pagetools"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// contentIn is POST /api/sessions/{id}/content's body. Options that
// don't go with the format are refused, so a mistake isn't silent.
type contentIn struct {
	Format string `json:"format"`
	// markdown and readability
	MaxChars *int `json:"max_chars"`
	Offset   *int `json:"offset"`
	// screenshot
	Width *int `json:"width"`
	// pdf
	Landscape  *bool   `json:"landscape"`
	Background *bool   `json:"background"`
	Pages      *string `json:"pages"`
}

// content serves the content endpoint (PROJ-20): the active tab as
// Markdown or its main text ({"text": "…"}, fenced, as the page tools
// answer), a screenshot (image/png, from the X display in either mode,
// as /screenshot) or a PDF (application/pdf, as an attachment).
func (a *API) content(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	var in contentIn
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body too large")
		return
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	text := in.MaxChars != nil || in.Offset != nil
	pdf := in.Landscape != nil || in.Background != nil || in.Pages != nil
	tok, _ := auth.TokenFromContext(r.Context())
	who := pagetools.Caller{TokenID: tok.ID, Agent: tok.Name}
	switch in.Format {
	case "markdown", "readability":
		if in.Width != nil || pdf {
			writeError(w, http.StatusBadRequest, in.Format+" takes max_chars and offset only")
			return
		}
		out, err := a.tools.Text(r.Context(), s, who, pagetools.TextIn{Format: in.Format, MaxChars: deref(in.MaxChars), Offset: deref(in.Offset)})
		if a.contentError(w, s, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"text": out})
	case "screenshot":
		if text || pdf {
			writeError(w, http.StatusBadRequest, "screenshot takes width only")
			return
		}
		s, ok := a.running(w, r, false)
		if !ok {
			return
		}
		target := "/screenshot"
		if in.Width != nil {
			if *in.Width < 16 {
				writeError(w, http.StatusBadRequest, "width: want a number of pixels, at least 16")
				return
			}
			target += "?width=" + strconv.Itoa(*in.Width)
		}
		a.m.Touch(s.ID)
		if a.sendScreenshot(w, r, s, target) {
			a.m.NoteFor(r.Context(), s.ID, tok.Name, "Took a screenshot")
		}
	case "pdf":
		if text || in.Width != nil {
			writeError(w, http.StatusBadRequest, "pdf takes landscape, background and pages only")
			return
		}
		out, err := a.tools.PDF(r.Context(), s, who, pagetools.PDFIn{Landscape: deref(in.Landscape), Background: deref(in.Background), Pages: deref(in.Pages)})
		if a.contentError(w, s, err) {
			return
		}
		// The PDF is the page's own content: never rendered here.
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="page.pdf"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		_, _ = w.Write(out) //nolint:gosec // a PDF, as an attachment, nosniff and sandboxed: never rendered here
	default:
		writeError(w, http.StatusBadRequest, "format: markdown, readability, screenshot or pdf")
	}
}

// contentError writes a content call's error, reporting whether there
// was one.
func (a *API) contentError(w http.ResponseWriter, s sessions.Session, err error) bool {
	var pe *pagetools.Error
	switch {
	case err == nil:
		return false
	case errors.As(err, &pe):
		writeError(w, pe.Status, pe.Msg)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// the client went away
	default:
		a.pageInternal(w, s, "a content call", err)
	}
	return true
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
