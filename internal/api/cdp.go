package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// cdpTicket mints a ticket for clients that can't set headers: they
// connect to the cdp_url it returns, which carries the ticket instead of
// the token.
func (a *API) cdpTicket(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	if s.Mode != profiles.ModeAPI {
		writeError(w, http.StatusConflict, "the session is in Visual mode, where nothing may attach to the page over CDP; use the visual endpoints, or restart it with mode api")
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	ticket, expires, err := a.web.MintCDPTicket(tok, s.Profile)
	if errors.Is(err, auth.ErrTooManyTickets) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't make a ticket")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ticket":     ticket,
		"expires_at": expires.UTC().Format(time.RFC3339),
		"cdp_url":    "ws" + strings.TrimPrefix(a.baseURL(r), "http") + "/cdp/" + s.Profile + "?ticket=" + url.QueryEscape(ticket),
	})
}

// cdp proxies Chrome DevTools Protocol to a profile's browser:
//
//	/cdp/{profile}                   the browser's WebSocket (or its version info, without an upgrade)
//	/cdp/{profile}/json/version      version info, URLs pointing back here
//	/cdp/{profile}/json/list         targets, likewise
//	/cdp/{profile}/devtools/...      a target's WebSocket
//
// It needs a running API-mode session: in Visual mode no CDP client may
// attach to the page, so the proxy refuses.
func (a *API) cdp(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.TokenFromContext(r.Context())
	profile := r.PathValue("profile")
	if !tok.Allows(profile) {
		writeError(w, http.StatusNotFound, "no profile named "+profile)
		return
	}
	s, ok := a.m.Live(profile)
	switch {
	case !ok:
		writeError(w, http.StatusConflict, "no browser is running for "+profile+"; start one with POST /api/sessions")
		return
	case s.Mode != profiles.ModeAPI:
		writeError(w, http.StatusConflict, "the session is in Visual mode, where nothing may attach to the page over CDP; use the visual endpoints, or restart it with mode api")
		return
	case s.State == sessions.NeedsHelp:
		writeError(w, http.StatusConflict, "a person has control of this browser; wait for the handoff to finish")
		return
	case s.HumanControl:
		writeError(w, http.StatusConflict, "a person is driving this browser from the live view; wait until they hand it back")
		return
	case s.State != sessions.Running:
		writeError(w, http.StatusConflict, "the session is "+string(s.State)+", not running")
		return
	}

	sock := a.m.Layout().ControlSocket(profile)
	path := strings.Trim(r.PathValue("path"), "/")
	upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	var target string
	switch {
	case path == "" && upgrade:
		browser, err := a.browserTarget(r.Context(), profile)
		if err != nil {
			writeError(w, http.StatusBadGateway, "can't reach the browser: "+err.Error())
			return
		}
		target = browser
	case path == "" || path == "json/version" || path == "json/list" || path == "json":
		a.cdpJSON(w, r, profile, path)
		return
	case strings.HasPrefix(path, "devtools/") && upgrade:
		target = "/" + path
	default:
		writeError(w, http.StatusNotFound, "not a CDP endpoint BrowserPier relays")
		return
	}

	// Attach checks the session is still running and gives a context that
	// ends if a person takes over, the session ends or the token is
	// revoked; the proxy then closes the connection.
	ctx, release, err := a.m.Attach(r.Context(), s.ID, tok.ID, tok.Name)
	if errors.Is(err, sessions.ErrHumanDriving) {
		writeError(w, http.StatusConflict, "a person is driving this browser from the live view; wait until they hand it back")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, "the session isn't running (it may have stopped, or a person has it)")
		return
	}
	defer release()
	// The CDP proxy isn't an agent route (scrub.go), so it says itself
	// whose entries these are.
	byTok := sessions.WithCall(r.Context(), a.m.NewCall(tok.ID, tok.Name, "cdp"))
	a.m.NoteFor(byTok, s.ID, tok.Name, "Connected over CDP")
	defer a.m.NoteFor(byTok, s.ID, tok.Name, "Disconnected")
	proxy := &httputil.ReverseProxy{
		Transport: controlTransport(sock),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: "control", Path: "/cdp" + target}
			pr.Out.Host = "control"
			// Nothing of the agent's credentials goes to the browser.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "can't reach the browser: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// WatchRevocations closes the CDP connections, and breaks off the page
// calls and input actions under way, of tokens that have been revoked,
// checking every interval until ctx ends. Revoking in the UI does so at
// once; this also catches the token CLI, which runs in another process.
func (a *API) WatchRevocations(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkRevocations(ctx)
		}
	}
}

func (a *API) checkRevocations(ctx context.Context) {
	for _, id := range a.m.ConnectedTokens() {
		if _, err := a.web.Tokens().Get(ctx, id); errors.Is(err, auth.ErrTokenNotFound) {
			a.log.Info("closing connections of a revoked token", "token_id", id)
			a.m.DisconnectToken(id)
		}
	}
}

// browserTarget returns the browser-level DevTools path, /devtools/browser/<id>.
func (a *API) browserTarget(ctx context.Context, profile string) (string, error) {
	resp, err := a.control(ctx, profile, http.MethodGet, "/cdp/json/version", nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return "", err
	}
	u, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil || !strings.HasPrefix(u.Path, "/devtools/browser/") {
		return "", errors.New("unexpected browser endpoint")
	}
	return u.Path, nil
}

// cdpJSON serves Chrome's /json endpoints with their WebSocket URLs
// pointing back through this proxy.
func (a *API) cdpJSON(w http.ResponseWriter, r *http.Request, profile, path string) {
	if path == "" {
		path = "json/version"
	}
	if path == "json" {
		path = "json/list"
	}
	resp, err := a.control(r.Context(), profile, http.MethodGet, "/cdp/"+path, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "can't reach the browser: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var body any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadGateway, "unexpected answer from the browser")
		return
	}
	base := "ws" + strings.TrimPrefix(a.baseURL(r), "http") + "/cdp/" + profile
	// A caller that came with a ticket used it up, so each URL it may
	// follow carries a fresh one.
	byTicket := auth.ByCDPTicket(r.Context())
	tok, _ := auth.TokenFromContext(r.Context())
	var mintErr error
	rewrite := func(m map[string]any) {
		if v, ok := m["webSocketDebuggerUrl"].(string); ok {
			if u, err := url.Parse(v); err == nil {
				suffix := ""
				if byTicket {
					ticket, _, err := a.web.MintCDPTicket(tok, profile)
					if err != nil {
						mintErr = err
					}
					suffix = "?ticket=" + url.QueryEscape(ticket)
				}
				m["webSocketDebuggerUrl"] = base + u.Path + suffix
			}
		}
		// The DevTools front end isn't served through BrowserPier.
		delete(m, "devtoolsFrontendUrl")
		delete(m, "devtoolsFrontendUrlCompat")
	}
	switch v := body.(type) {
	case map[string]any:
		rewrite(v)
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				rewrite(m)
			}
		}
	}
	if errors.Is(mintErr, auth.ErrTooManyTickets) {
		writeError(w, http.StatusTooManyRequests, mintErr.Error())
		return
	}
	if mintErr != nil {
		writeError(w, http.StatusInternalServerError, "couldn't make a ticket")
		return
	}
	writeJSON(w, http.StatusOK, body)
}
