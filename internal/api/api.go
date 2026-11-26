// Package api serves the agent HTTP API, the token-checked CDP proxy and
// the Visual-mode endpoints. Every route needs an agent token, and a token
// only sees the profiles it is scoped to: others look like they don't exist.
package api

import (
	"bytes"
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
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/pagetools"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/settings"
)

// API is the agent-facing HTTP API.
type API struct {
	m        *sessions.Manager
	web      *auth.Web
	files    *files.Store
	handoffs *handoff.Service
	settings *settings.Store // the admin's defaults for new profiles, if set
	log      *slog.Logger
	public   string // BROWSERPIER_PUBLIC_URL, if set
	tools    *pagetools.Tools
}

func New(m *sessions.Manager, web *auth.Web, fs *files.Store, log *slog.Logger, publicURL string) *API {
	tools := pagetools.New(m, log)
	tools.SetFiles(fs)
	if fs != nil {
		m.OnEnd(fs.Forget) // what download notices keep about a session
	}
	return &API{m: m, web: web, files: fs, log: log, public: strings.TrimRight(publicURL, "/"), tools: tools}
}

// SetSettings makes profiles created through the API start from the
// admin's defaults instead of the built-in ones.
func (a *API) SetSettings(s *settings.Store) { a.settings = s }

// Register adds the API's routes to mux. Each is an agent route (scrub.go)
// but the CDP proxy's.
func (a *API) Register(mux Router) {
	t, c := a.agent, a.agentCall
	mux.Handle("GET /api/profiles", t(a.listProfiles))
	mux.Handle("POST /api/profiles", t(a.createProfile))
	mux.Handle("POST /api/sessions", t(a.startSession))
	mux.Handle("GET /api/sessions", t(a.listSessions))
	mux.Handle("GET /api/sessions/{id}", t(a.getSession))
	mux.Handle("DELETE /api/sessions/{id}", t(a.stopSession))
	mux.Handle("GET /api/sessions/{id}/screenshot", t(a.screenshot))
	mux.Handle("GET /api/sessions/{id}/calls/{call_id}", t(a.callStatus))
	// The actions that change the page take a call id (PROJ-156).
	mux.Handle("POST /api/sessions/{id}/click", c("click", a.input("/input/click")))
	mux.Handle("POST /api/sessions/{id}/type", c("type", a.input("/input/type")))
	mux.Handle("POST /api/sessions/{id}/computer", c("computer", a.computer))
	mux.Handle("POST /api/sessions/{id}/files", t(a.uploadFile))
	mux.Handle("GET /api/profiles/{name}/files", t(a.listFiles))
	mux.Handle("GET /api/profiles/{name}/files/{file}", t(a.getFile))
	mux.Handle("DELETE /api/profiles/{name}/files/{file}", t(a.deleteFile))
	mux.Handle("POST /api/sessions/{id}/cdp-ticket", t(a.cdpTicket))
	a.registerPageTools(mux)
	if a.handoffs != nil {
		a.registerHandoffs(mux)
	}
	// Many CDP clients can't set headers, so the CDP proxy also takes a
	// ticket as ?ticket=; never the token, which a proxy in front could log.
	// It isn't scrubbed: raw CDP is full control of the browser, which can
	// read any field's value (SPEC §11).
	proxied := func(h http.HandlerFunc) http.Handler { return a.web.RequireTokenOrCDPTicket(h) }
	mux.Handle("GET /cdp/{profile}", proxied(a.cdp))
	mux.Handle("GET /cdp/{profile}/{path...}", proxied(a.cdp))
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
	LocalNetwork    bool       `json:"local_network"`       // pages may reach private addresses
	BlockAds        bool       `json:"block_ads"`           // the browser blocks ad and tracker domains
	// Site is the main site as set, whose favicon is the profile's icon in
	// the web UI. The one BrowserPier infers from the history isn't shown:
	// it is read from pages.
	Site      string `json:"site,omitempty"`
	IconGlyph string `json:"icon_glyph,omitempty"` // the fallback icon; "" for the default
	IconColor string `json:"icon_color,omitempty"`
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
		LocalNetwork:    p.LocalNetwork,
		BlockAds:        p.BlockAds,
		Site:            p.Site,
		IconGlyph:       p.IconGlyph,
		IconColor:       p.IconColor,
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
	LocalNetwork    *bool       `json:"local_network"`
	BlockAds        *bool       `json:"block_ads"`
	Site            *string     `json:"site"`
	IconGlyph       *string     `json:"icon_glyph"`
	IconColor       *string     `json:"icon_color"`
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
	if in.LocalNetwork != nil && *in.LocalNetwork {
		// Nor to open the LAN to the pages it visits.
		writeError(w, http.StatusBadRequest, "let a profile reach the local network in the web UI, not through the API")
		return
	}
	// The admin's default proxy applies: the agent didn't choose it.
	p := profiles.New(in.Name)
	if a.settings != nil {
		set, err := a.settings.Get(r.Context())
		if err != nil {
			a.internal(w, "loading the default settings", err)
			return
		}
		p = set.NewProfile(in.Name)
	}
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
		// Agents can make handoffs stricter than the default, never
		// looser: a looser policy is ignored, as the spec says.
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
	if in.BlockAds != nil {
		// Like the handoff policy: an agent can ask for more blocking,
		// never less. Turning it off is for one session (POST
		// /api/sessions with "block_ads": false), or the admin's call.
		if !*in.BlockAds {
			writeError(w, http.StatusBadRequest, `block_ads: an agent can't turn a profile's ad and tracker blocking off; start the session with "block_ads": false instead, or ask the admin`)
			return
		}
		p.BlockAds = true
	}
	if in.Site != nil {
		p.Site = profiles.NormalizeSite(*in.Site)
	}
	if in.IconGlyph != nil {
		p.IconGlyph = *in.IconGlyph
	}
	if in.IconColor != nil {
		p.IconColor = *in.IconColor
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
	ID          string   `json:"id"`
	Profile     string   `json:"profile"`
	Mode        string   `json:"mode"`
	State       string   `json:"state"`
	Attached    bool     `json:"attached"`
	CDPURL      string   `json:"cdp_url,omitempty"`
	LiveViewURL string   `json:"live_view_url"`
	InUseBy     []string `json:"in_use_by,omitempty"`
	Error       string   `json:"error,omitempty"`
	LastLog     string   `json:"last_log,omitempty"`
	BlockAds    bool     `json:"block_ads"`
	// AdListMissing: the session was to block, but no list had been
	// downloaded yet when it started, so block_ads is false.
	AdListMissing bool      `json:"ad_list_missing,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func (a *API) sessionOut(r *http.Request, s sessions.Session, attached bool) sessionJSON {
	base := a.baseURL(r)
	out := sessionJSON{
		ID: s.ID, Profile: s.Profile, Mode: string(s.Mode), State: string(s.State), Attached: attached,
		LiveViewURL: base + "/profiles/" + url.PathEscape(s.Profile),
		InUseBy:     s.InUseBy, Error: s.Error, CreatedAt: s.CreatedAt, BlockAds: s.BlockAds, AdListMissing: s.AdListMissing,
	}
	// The browser container's words, which a compromised page could have
	// written: fenced like page text.
	if why := sessions.CrashReason(s.LastLog); why != "" {
		out.Error += ": " + fence.Wrap("browser crash output", why)
	}
	if s.LastLog != "" {
		out.LastLog = fence.Wrap("browser container log", s.LastLog)
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
	// BlockAds false turns the profile's ad and tracker blocking off for
	// this session, a running one included; true or nothing leaves it as
	// it is. It can't turn blocking on.
	BlockAds *bool `json:"block_ads"`
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
	allowAds := in.BlockAds != nil && !*in.BlockAds
	before, wasLive := a.m.Live(in.Profile)
	s, attached, err := a.m.StartWith(r.Context(), in.Profile, sessions.StartOptions{Mode: profiles.Mode(in.Mode), AllowAds: allowAds})
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
	case errors.Is(err, sessions.ErrProfileHeld):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return // the client went away
	case err != nil:
		a.internal(w, "starting a session", err)
		return
	}
	a.m.Touch(s.ID)
	if allowAds && wasLive && before.ID == s.ID && before.BlockAds && !s.BlockAds {
		a.m.NoteFor(r.Context(), s.ID, tok.Name, "Turned ad and tracker blocking off for this session")
	}
	status := http.StatusCreated
	if attached {
		status = http.StatusOK
		a.m.NoteFor(r.Context(), s.ID, tok.Name, "Attached")
	} else {
		what := "Started the browser"
		if allowAds {
			what += ", without ad and tracker blocking"
		}
		a.m.NoteFor(r.Context(), s.ID, tok.Name, what)
		a.log.Info("session started", "session", s.ID, "profile", s.Profile, "mode", s.Mode, "block_ads", s.BlockAds, "token", tok.Name)
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
	msg := full.Error()
	if len(full.Running) == 0 {
		return msg
	}
	msg += ". Stop a browser you don't need (DELETE /api/sessions/{id})"
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

// getSession answers with the session and the token's own calls on it:
// those under way and its recent activity entries (PROJ-156).
func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.session(w, r); ok {
		writeJSON(w, http.StatusOK, a.sessionStatus(r, s))
	}
}

func (a *API) stopSession(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	// Noted first: the log goes with the session once it has stopped.
	a.m.NoteFor(r.Context(), s.ID, tok.Name, "Stopping the browser")
	stopped, err := a.m.Stop(r.Context(), s.ID)
	if err != nil {
		a.m.NoteFor(r.Context(), s.ID, tok.Name, "Couldn't stop the browser") // still live, so still logged
		if errors.Is(err, sessions.ErrClosed) {
			writeError(w, http.StatusServiceUnavailable, "BrowserPier is shutting down")
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
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
	if a.sendScreenshot(w, r, s, target) {
		tok, _ := auth.TokenFromContext(r.Context())
		a.m.NoteFor(r.Context(), s.ID, tok.Name, "Took a screenshot")
	}
}

// sendScreenshot relays a screenshot from the control socket's target,
// reporting whether it was one.
func (a *API) sendScreenshot(w http.ResponseWriter, r *http.Request, s sessions.Session, target string) bool {
	resp, err := a.control(r.Context(), s.Profile, http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "screenshot failed: "+err.Error())
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	relay(w, resp, "Content-Type", "X-Screen-Width", "X-Screen-Height")
	return resp.StatusCode == http.StatusOK
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
		if !json.Valid(body) {
			// Refused here, before the input's turn, rather than by the
			// browser: a call id is then given back (PROJ-156).
			writeError(w, http.StatusBadRequest, "bad JSON")
			return
		}
		var mark *files.Mark
		var wait time.Duration
		var ms *int
		if path == "/input/click" {
			// A click may start a download (PROJ-116); the browser
			// doesn't take download_wait_ms, which is BrowserPier's.
			// When it is set, the click expects one (files.Mark.Expect).
			if body, ms, err = takeDownloadWait(body); err == nil {
				wait, err = files.DownloadWait(ms)
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if err := checkInput(path, body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if path == "/input/click" {
			mark = a.markDownloads(s)
			if ms != nil {
				mark.Expect(wait)
			}
		}
		if !a.act(w, r, s, path, body) {
			return
		}
		if mark != nil {
			mark.Ended()
			a.reportDownloads(w, r, s, mark, wait)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkInput refuses a /click or /type body the browser would refuse for
// its shape, before the input's turn, so a call id is given back
// (PROJ-156): fields of the wrong type or unknown, more after the JSON, a
// pace or button it doesn't know, no text to type. The browser checks the
// same again, and what only it knows (the screen's size, the text's
// length at the pace asked).
func checkInput(path string, body []byte) error {
	var pace string
	switch path {
	case "/input/click":
		// As the browser's clickRequest: no tags, so names match as
		// encoding/json matches them, in any case.
		var c struct {
			X, Y   int
			Button string
			Double bool
			Pace   string
		}
		if err := decodeOne(bytes.NewReader(body), &c); err != nil {
			return errors.New("bad JSON: " + err.Error())
		}
		switch c.Button {
		case "", "left", "middle", "right":
		default:
			return fmt.Errorf("button %q: want left, middle or right", c.Button)
		}
		pace = c.Pace
	case "/input/type":
		var t struct {
			Text string
			Pace string
		}
		if err := decodeOne(bytes.NewReader(body), &t); err != nil {
			return errors.New("bad JSON: " + err.Error())
		}
		if t.Text == "" || !utf8.ValidString(t.Text) {
			return errors.New("text: want non-empty UTF-8")
		}
		pace = t.Pace
	default:
		return nil
	}
	switch pace {
	case "", "human", "instant":
		return nil
	}
	return fmt.Errorf("pace %q: want human or instant", pace)
}

// takeDownloadWait takes download_wait_ms out of a click's JSON body,
// leaving the rest as it was. A body that isn't a JSON object goes on
// unchanged, for the browser to refuse.
func takeDownloadWait(body []byte) ([]byte, *int, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body, nil, nil
	}
	raw, ok := fields["download_wait_ms"]
	if !ok {
		return body, nil, nil
	}
	var ms int
	if err := json.Unmarshal(raw, &ms); err != nil {
		return nil, nil, errors.New("download_wait_ms: want a number of milliseconds")
	}
	delete(fields, "download_wait_ms")
	out, err := json.Marshal(fields)
	return out, &ms, err
}

// act runs one input action through the control socket and logs it, or
// writes why it didn't run.
func (a *API) act(w http.ResponseWriter, r *http.Request, s sessions.Session, path string, body []byte) bool {
	ctx, done, err := a.m.AgentInput(r.Context(), s.ID)
	if errors.Is(err, sessions.ErrInputBusy) {
		writeError(w, http.StatusConflict, "another input is still running in this browser; wait for it to finish, then send this one")
		return false
	}
	if err != nil {
		writeError(w, http.StatusConflict, "a person has this browser (live view or a handoff); wait until they hand it back")
		return false
	}
	// Until this returns: a handoff, the live view's Take control or a
	// stop cancels ctx, which aborts the request to the browser and waits
	// for done. The browser then breaks the action off and lets go of any
	// key or button it held, which can still take it some tens of
	// milliseconds after the person has the browser.
	defer done()
	tok, _ := auth.TokenFromContext(r.Context())
	resp, err := a.control(ctx, s.Profile, http.MethodPost, path, body)
	if err != nil {
		if ctx.Err() != nil && r.Context().Err() == nil {
			// Cut short: how far it got isn't known, but the log says
			// it began.
			what := "Input interrupted"
			if path == "/input/type" {
				what = "Typing interrupted"
			}
			a.m.NoteFor(r.Context(), s.ID, tok.Name, what)
			writeError(w, http.StatusConflict, "input broke off: "+context.Cause(ctx).Error())
			return false
		}
		writeError(w, http.StatusBadGateway, "input failed: "+err.Error())
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		writeError(w, resp.StatusCode, strings.TrimSpace(string(msg)))
		return false
	}
	a.m.NoteFor(r.Context(), s.ID, tok.Name, describeInput(path, body))
	return true
}

// describeInput puts an input action in words for the activity log.
// Typed text is never included, only its length: it may be a password or
// a one-time code.
func describeInput(path string, body []byte) string {
	switch path {
	case "/input/scroll":
		var c struct{ X, Y int }
		_ = json.Unmarshal(body, &c)
		return fmt.Sprintf("Scrolled at %d, %d", c.X, c.Y)
	case "/input/key":
		var k struct{ Keys json.RawMessage }
		_ = json.Unmarshal(body, &k)
		return describeKeys(k.Keys)
	case "/input/drag":
		var d struct{ From, To struct{ X, Y int } }
		_ = json.Unmarshal(body, &d)
		return fmt.Sprintf("Dragged from %d, %d to %d, %d", d.From.X, d.From.Y, d.To.X, d.To.Y)
	}
	if path == "/input/type" {
		var t struct{ Text string }
		_ = json.Unmarshal(body, &t)
		if n := utf8.RuneCountInString(t.Text); n != 1 {
			return fmt.Sprintf("Typed %d characters", n)
		}
		return "Typed 1 character"
	}
	var c struct {
		X, Y   int
		Button string
		Double bool
	}
	_ = json.Unmarshal(body, &c)
	what := "Clicked"
	switch {
	case c.Double:
		what = "Double-clicked"
	case c.Button == "right":
		what = "Right-clicked"
	case c.Button == "middle":
		what = "Middle-clicked"
	}
	return fmt.Sprintf("%s at %d, %d", what, c.X, c.Y)
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
	if err := decodeOne(http.MaxBytesReader(w, r.Body, 64<<10), v); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

// decodeOne reads exactly one JSON value into v, refusing unknown fields
// and anything after the value but white space: a valid action followed
// by a stray "}" is refused, not run (PROJ-156: running it would take its
// call id, and the corrected retry would then be another call).
func decodeOne(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more after the JSON value")
	}
	return nil
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

// pageInternal is internal for a call on a session's page: the error may
// quote the page, so it is logged with the session's filled secrets
// hidden (PROJ-125).
func (a *API) pageInternal(w http.ResponseWriter, s sessions.Session, what string, err error) {
	sc := a.m.Scrubber(func(p string) bool { return p == s.Profile })
	a.log.Error(what, "session", s.ID, "err", sc.String(err.Error()))
	writeError(w, http.StatusInternalServerError, what+" failed")
}
