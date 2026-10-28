// Package api serves the agent HTTP API, the token-checked CDP proxy and
// the Visual-mode endpoints. Every route needs an agent token, and a token
// only sees the profiles it is scoped to: others look like they don't exist.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// API is the agent-facing HTTP API.
type API struct {
	m      *sessions.Manager
	web    *auth.Web
	files  *files.Store
	log    *slog.Logger
	public string // BROWSERPIER_PUBLIC_URL, if set
}

func New(m *sessions.Manager, web *auth.Web, fs *files.Store, log *slog.Logger, publicURL string) *API {
	return &API{m: m, web: web, files: fs, log: log, public: strings.TrimRight(publicURL, "/")}
}

// Register adds the API's routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	t := func(h http.HandlerFunc) http.Handler { return a.web.RequireToken(h) }
	mux.Handle("GET /api/profiles", t(a.listProfiles))
	mux.Handle("POST /api/profiles", t(a.createProfile))
	mux.Handle("POST /api/sessions", t(a.startSession))
	mux.Handle("GET /api/sessions", t(a.listSessions))
	mux.Handle("GET /api/sessions/{id}", t(a.getSession))
	mux.Handle("DELETE /api/sessions/{id}", t(a.stopSession))
	mux.Handle("GET /api/sessions/{id}/screenshot", t(a.screenshot))
	mux.Handle("POST /api/sessions/{id}/click", t(a.input("/input/click")))
	mux.Handle("POST /api/sessions/{id}/type", t(a.input("/input/type")))
	mux.Handle("POST /api/sessions/{id}/files", t(a.uploadFile))
	mux.Handle("GET /api/profiles/{name}/files", t(a.listFiles))
	mux.Handle("GET /api/profiles/{name}/files/{file}", t(a.getFile))
	mux.Handle("DELETE /api/profiles/{name}/files/{file}", t(a.deleteFile))
	// Many CDP clients can't set headers, so the CDP proxy also takes the
	// token as ?token=.
	mux.Handle("GET /cdp/{profile}", tokenFromQuery(t(a.cdp)))
	mux.Handle("GET /cdp/{profile}/{path...}", tokenFromQuery(t(a.cdp)))
}

type profileJSON struct {
	Name            string     `json:"name"`
	DefaultMode     string     `json:"default_mode"`
	AutoStopMinutes int        `json:"auto_stop_minutes"`
	Screen          screenJSON `json:"screen"`
	Proxy           string     `json:"proxy,omitempty"` // password redacted
	HandoffPolicy   string     `json:"handoff_policy"`
	TimeZone        string     `json:"time_zone,omitempty"` // set by hand
	Locale          string     `json:"locale,omitempty"`    // set by hand
	// Location is where the browser appeared to be when it last started.
	Location  *locationJSON `json:"location,omitempty"`
	Status    string        `json:"status"` // the live session's state, or stopped
	SessionID string        `json:"session_id,omitempty"`
	InUseBy   []string      `json:"in_use_by,omitempty"`
}

type locationJSON struct {
	Country   string    `json:"country"`
	TimeZone  string    `json:"time_zone"`
	IP        string    `json:"ip,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type screenJSON struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (a *API) profileOut(p profiles.Profile) profileJSON {
	out := profileJSON{
		Name:            p.Name,
		DefaultMode:     string(p.DefaultMode),
		AutoStopMinutes: int(p.AutoStop / time.Minute),
		Screen:          screenJSON{p.Width, p.Height},
		Proxy:           redactProxy(p.Proxy),
		HandoffPolicy:   string(p.HandoffPolicy),
		TimeZone:        p.TimeZone,
		Locale:          p.Locale,
		Status:          string(sessions.Stopped),
	}
	if l := p.Location; !l.CheckedAt.IsZero() {
		out.Location = &locationJSON{Country: l.Country, TimeZone: l.TimeZone, IP: l.IP, CheckedAt: l.CheckedAt}
	}
	if s, ok := a.m.Live(p.Name); ok {
		out.Status, out.SessionID, out.InUseBy = string(s.State), s.ID, s.InUseBy
	}
	return out
}

func redactProxy(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(set)"
	}
	return u.Redacted()
}

func (a *API) listProfiles(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.TokenFromContext(r.Context())
	all, err := a.m.Profiles().List(r.Context())
	if err != nil {
		a.internal(w, "listing profiles", err)
		return
	}
	out := []profileJSON{}
	for _, p := range all {
		if tok.Allows(p.Name) {
			out = append(out, a.profileOut(p))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

type createProfileJSON struct {
	Name            string      `json:"name"`
	DefaultMode     *string     `json:"default_mode"`
	AutoStopMinutes *int        `json:"auto_stop_minutes"`
	Screen          *screenJSON `json:"screen"`
	HandoffPolicy   *string     `json:"handoff_policy"`
	Proxy           *string     `json:"proxy"`
}

// policyRank orders handoff policies from least to most strict.
var policyRank = map[profiles.HandoffPolicy]int{profiles.PolicyLink: 0, profiles.PolicyPIN: 1, profiles.PolicySignIn: 2}

func (a *API) createProfile(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.TokenFromContext(r.Context())
	if !tok.AllProfiles {
		writeError(w, http.StatusForbidden, "only a token for all profiles can create one")
		return
	}
	var in createProfileJSON
	if !readJSON(w, r, &in) {
		return
	}
	if in.Proxy != nil {
		// A proxy sees where the browser goes; an agent steered by a page
		// shouldn't be able to choose one.
		writeError(w, http.StatusBadRequest, "set a profile's proxy in the web UI, not through the API")
		return
	}
	p := profiles.New(in.Name)
	if in.DefaultMode != nil {
		p.DefaultMode = profiles.Mode(*in.DefaultMode)
	}
	if in.AutoStopMinutes != nil {
		p.AutoStop = time.Duration(*in.AutoStopMinutes) * time.Minute
	}
	if in.Screen != nil {
		p.Width, p.Height = in.Screen.Width, in.Screen.Height
	}
	if in.HandoffPolicy != nil {
		// Agents can make handoffs stricter, never looser: a looser
		// policy is ignored, as the spec says.
		asked := profiles.HandoffPolicy(*in.HandoffPolicy)
		rank, ok := policyRank[asked]
		if !ok {
			writeError(w, http.StatusBadRequest, "handoff_policy: want link, pin or signin")
			return
		}
		if rank > policyRank[p.HandoffPolicy] {
			p.HandoffPolicy = asked
		}
	}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := a.m.Profiles().Create(r.Context(), p)
	switch {
	case errors.Is(err, profiles.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.internal(w, "creating a profile", err)
		return
	}
	a.log.Info("profile created", "profile", created.Name, "token", tok.Name)
	writeJSON(w, http.StatusCreated, a.profileOut(created))
}

type sessionJSON struct {
	ID          string    `json:"id"`
	Profile     string    `json:"profile"`
	Mode        string    `json:"mode"`
	State       string    `json:"state"`
	Attached    bool      `json:"attached"`
	CDPURL      string    `json:"cdp_url,omitempty"`
	LiveViewURL string    `json:"live_view_url"`
	InUseBy     []string  `json:"in_use_by,omitempty"`
	Error       string    `json:"error,omitempty"`
	LastLog     string    `json:"last_log,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func (a *API) sessionOut(r *http.Request, s sessions.Session, attached bool) sessionJSON {
	base := a.baseURL(r)
	out := sessionJSON{
		ID: s.ID, Profile: s.Profile, Mode: string(s.Mode), State: string(s.State), Attached: attached,
		LiveViewURL: base + "/live/" + s.Profile,
		InUseBy:     s.InUseBy, Error: s.Error, LastLog: s.LastLog, CreatedAt: s.CreatedAt,
	}
	if s.Mode == profiles.ModeAPI && s.State.Live() {
		out.CDPURL = "ws" + strings.TrimPrefix(base, "http") + "/cdp/" + s.Profile
	}
	return out
}

// baseURL is how the caller reaches BrowserPier: the configured public URL,
// or else the request's own scheme and host.
func (a *API) baseURL(r *http.Request) string {
	if a.public != "" {
		return a.public
	}
	scheme := "http"
	if a.web.IsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type startJSON struct {
	Profile string `json:"profile"`
	Mode    string `json:"mode"`
}

func (a *API) startSession(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.TokenFromContext(r.Context())
	var in startJSON
	if !readJSON(w, r, &in) {
		return
	}
	if !tok.Allows(in.Profile) {
		writeError(w, http.StatusNotFound, "no profile named "+strconv.Quote(in.Profile))
		return
	}
	switch profiles.Mode(in.Mode) {
	case "", profiles.ModeAPI, profiles.ModeVisual:
	default:
		writeError(w, http.StatusBadRequest, "mode: want api or visual")
		return
	}
	s, attached, err := a.m.Start(r.Context(), in.Profile, profiles.Mode(in.Mode))
	switch {
	case errors.Is(err, profiles.ErrNotFound):
		writeError(w, http.StatusNotFound, "no profile named "+strconv.Quote(in.Profile))
		return
	case errors.Is(err, sessions.ErrStartFailed):
		writeJSON(w, http.StatusBadGateway, a.sessionOut(r, s, attached))
		return
	case errors.Is(err, sessions.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "BrowserPier is shutting down")
		return
	case errors.Is(err, sessions.ErrNoRoom):
		writeError(w, http.StatusServiceUnavailable, noRoomMessage(err, tok))
		return
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return // the client went away
	case err != nil:
		a.internal(w, "starting a session", err)
		return
	}
	a.m.Touch(s.ID)
	status := http.StatusCreated
	if attached {
		status = http.StatusOK
	} else {
		a.log.Info("session started", "session", s.ID, "profile", s.Profile, "mode", s.Mode, "token", tok.Name)
	}
	writeJSON(w, status, a.sessionOut(r, s, attached))
}

// noRoomMessage explains a refusal for lack of memory, naming only the
// browsers this token may see.
func noRoomMessage(err error, tok auth.Token) string {
	var full *sessions.NoRoomError
	if !errors.As(err, &full) {
		return err.Error()
	}
	var mine []string
	others := 0
	for _, u := range full.Running {
		if tok.Allows(u.Profile) {
			mine = append(mine, u.String())
		} else {
			others++
		}
	}
	msg := full.Error() + ". Stop a browser you don't need (DELETE /api/sessions/{id})"
	if len(mine) > 0 {
		msg += "; yours: " + strings.Join(mine, ", ")
	}
	if others > 0 {
		msg += fmt.Sprintf("; and %d more", others)
	}
	return msg
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	tok, _ := auth.TokenFromContext(r.Context())
	out := []sessionJSON{}
	for _, s := range a.m.List() {
		if tok.Allows(s.Profile) {
			out = append(out, a.sessionOut(r, s, false))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// session loads the {id} session if the token may see it, or answers 404.
func (a *API) session(w http.ResponseWriter, r *http.Request) (sessions.Session, bool) {
	tok, _ := auth.TokenFromContext(r.Context())
	s, err := a.m.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, sessions.ErrNotFound) || (err == nil && !tok.Allows(s.Profile)) {
		writeError(w, http.StatusNotFound, "no such session")
		return sessions.Session{}, false
	}
	if err != nil {
		a.internal(w, "loading a session", err)
		return sessions.Session{}, false
	}
	return s, true
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.session(w, r); ok {
		writeJSON(w, http.StatusOK, a.sessionOut(r, s, false))
	}
}

func (a *API) stopSession(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	stopped, err := a.m.Stop(r.Context(), s.ID)
	if err != nil {
		if errors.Is(err, sessions.ErrClosed) {
			writeError(w, http.StatusServiceUnavailable, "BrowserPier is shutting down")
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	a.log.Info("session stopped by an agent", "session", s.ID, "profile", s.Profile, "token", tok.Name)
	writeJSON(w, http.StatusOK, a.sessionOut(r, stopped, false))
}

// running loads the {id} session and checks an agent may drive it now.
func (a *API) running(w http.ResponseWriter, r *http.Request, driving bool) (sessions.Session, bool) {
	s, ok := a.session(w, r)
	if !ok {
		return s, false
	}
	switch {
	case s.State == sessions.NeedsHelp && driving:
		writeError(w, http.StatusConflict, "a person has control of this browser; wait for the handoff to finish")
		return s, false
	case s.State != sessions.Running && s.State != sessions.NeedsHelp:
		writeError(w, http.StatusConflict, "the session is "+string(s.State)+", not running")
		return s, false
	}
	return s, true
}

func (a *API) screenshot(w http.ResponseWriter, r *http.Request) {
	s, ok := a.running(w, r, false)
	if !ok {
		return
	}
	target := "/screenshot"
	if width := r.URL.Query().Get("width"); width != "" {
		if n, err := strconv.Atoi(width); err != nil || n < 16 {
			writeError(w, http.StatusBadRequest, "width: want a number of pixels, at least 16")
			return
		}
		target += "?width=" + url.QueryEscape(width)
	}
	a.m.Touch(s.ID)
	resp, err := a.control(r.Context(), s.Profile, http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "screenshot failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	relay(w, resp, "Content-Type", "X-Screen-Width", "X-Screen-Height")
}

// input forwards a Visual-mode action to the browser's control socket,
// which checks it and runs it through xdotool.
func (a *API) input(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.running(w, r, true)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body too large")
			return
		}
		a.m.Touch(s.ID)
		resp, err := a.control(r.Context(), s.Profile, http.MethodPost, path, body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "input failed: "+err.Error())
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			writeError(w, resp.StatusCode, strings.TrimSpace(string(msg)))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// control makes one request to a profile's control socket.
func (a *API) control(ctx context.Context, profile, method, target string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	// The host is a Unix socket and target is one of a few fixed paths.
	req, err := http.NewRequestWithContext(ctx, method, "http://control"+target, rd) //nolint:gosec // see above
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return controlClient(a.m.Layout().ControlSocket(profile)).Do(req) //nolint:gosec // see above
}

func controlClient(sock string) *http.Client {
	return &http.Client{Transport: controlTransport(sock)}
}

func controlTransport(sock string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
}

func relay(w http.ResponseWriter, resp *http.Response, headers ...string) {
	for _, h := range headers {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *API) internal(w http.ResponseWriter, what string, err error) {
	a.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, what+" failed")
}
