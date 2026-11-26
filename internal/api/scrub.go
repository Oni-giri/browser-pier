package api

import (
	"bytes"
	"mime"
	"net/http"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// Router is where the API registers its routes: an *http.ServeMux, or a
// test's recorder.
type Router interface {
	Handle(pattern string, handler http.Handler)
}

// agentRoute is one of the agent API's routes: the token check, then the
// handler, with everything it writes scrubbed of the secrets BrowserPier
// typed into the token's browsers (PROJ-125) as the last step before it
// leaves. Every route but the CDP proxy is one, and only agent and
// agentCall make them; TestEveryRouteIsScrubbed checks the routes.
type agentRoute struct {
	h http.Handler
	// tool names a route that takes a call id (agentCall, PROJ-156);
	// "" for one that refuses it.
	tool string
}

func (r agentRoute) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.h.ServeHTTP(w, req) }

// agent makes an agent route of h, which takes no call id.
func (a *API) agent(h http.HandlerFunc) http.Handler {
	return agentRoute{h: a.web.RequireToken(a.scrubbing(a.refusingCallID(h)))}
}

// scrubbing runs h with a writer that scrubs its answer. The secrets are
// those of every live session of a profile the token allows, as they are
// both when the request comes in and once it is answered: a secret filled
// during the call counts, and so does one whose session ended meanwhile.
func (a *API) scrubbing(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := auth.TokenFromContext(r.Context())
		before := a.m.Scrubber(tok.Allows)
		sw := &scrubWriter{w: w, scrubber: func() *scrub.Scrubber { return scrub.Union(before, a.m.Scrubber(tok.Allows)) }}
		h.ServeHTTP(sw, r)
		sw.finish()
	})
}

// scrubWriter holds back a text answer until the handler has finished,
// then writes it scrubbed: a JSON answer string by string, anything else
// as text. Headers are scrubbed too (X-Downloads holds file names). A
// screenshot, a PDF or a downloaded file goes through as it is, unread:
// no secret can be taken out of their bytes, as SPEC says.
type scrubWriter struct {
	w        http.ResponseWriter
	scrubber func() *scrub.Scrubber

	sc      *scrub.Scrubber
	started bool // WriteHeader was called
	binary  bool // passing the body through
	code    int
	buf     bytes.Buffer
}

func (s *scrubWriter) Header() http.Header { return s.w.Header() }

func (s *scrubWriter) WriteHeader(code int) {
	if s.started {
		return
	}
	s.started, s.code = true, code
	if binaryType(s.w.Header().Get("Content-Type")) {
		s.binary = true
		s.scrubHeaders()
		s.w.WriteHeader(code)
	}
}

func (s *scrubWriter) Write(b []byte) (int, error) {
	if !s.started {
		if s.w.Header().Get("Content-Type") == "" {
			s.w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		s.WriteHeader(http.StatusOK)
	}
	if s.binary {
		return s.w.Write(b)
	}
	return s.buf.Write(b)
}

// finish writes what was held back.
func (s *scrubWriter) finish() {
	if s.binary {
		return
	}
	s.scrubHeaders()
	if !s.started {
		return // nothing written: net/http answers 200, headers scrubbed
	}
	s.w.Header().Del("Content-Length") // the body may change
	body := s.buf.Bytes()
	if s.sc != nil {
		if jsonType(s.w.Header().Get("Content-Type")) {
			body = s.sc.JSON(body)
		} else if text := string(body); s.sc.String(text) != text {
			body = []byte(s.sc.String(text))
		}
	}
	s.w.WriteHeader(s.code)
	_, _ = s.w.Write(body)
}

// scrubHeaders resolves the scrubber, once the handler has done its work,
// and scrubs every header value.
func (s *scrubWriter) scrubHeaders() {
	s.sc = s.scrubber()
	if s.sc == nil {
		return
	}
	for _, vs := range s.w.Header() {
		for i, v := range vs {
			vs[i] = s.sc.String(v)
		}
	}
}

// binaryType reports whether a Content-Type is one whose bytes go through
// unscrubbed. Anything else, a missing type included, is scrubbed as text.
func binaryType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return strings.HasPrefix(mt, "image/") || mt == "application/pdf" || mt == "application/octet-stream"
}

func jsonType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// Scrubber hides the secrets of the live browsers of the profiles allow
// accepts: what the MCP tools' results are scrubbed with.
func (a *API) Scrubber(allow func(profile string) bool) *scrub.Scrubber { return a.m.Scrubber(allow) }
