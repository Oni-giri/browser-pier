// Package ui serves the web UI: Go templates and htmx, embedded in the binary.
package ui

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/settings"
)

//go:embed templates static
var embedded embed.FS

// Options are what the UI needs from the server's configuration.
type Options struct {
	// PublicURL is how agents reach this server, for the URLs shown to copy.
	// Empty: the address the page was loaded from.
	PublicURL string
	// HostMemory is the host's total memory in bytes; 0 if unknown.
	HostMemory int64
	// RAMLimitFromEnv: BROWSERPIER_RAM_LIMIT_PERCENT is set, so the memory
	// limit can't be changed here.
	RAMLimitFromEnv bool
	// SetRAMLimit applies a new memory limit, as a percent of host memory.
	SetRAMLimit func(percent int)
}

// UI is the web UI for the signed-in admin.
type UI struct {
	m        *sessions.Manager
	web      *auth.Web
	handoffs *handoff.Service
	settings *settings.Store
	log      *slog.Logger
	o        Options
	pages    map[string]*template.Template
	// pinTries limits wrong current PINs and passwords on Settings: the
	// admin is signed in, but a borrowed laptop shouldn't be able to guess.
	pinTries *auth.Lockout

	// prevMu also serialises screenshots: one at a time is plenty.
	prevMu   sync.Mutex
	previews map[string]cachedPreview

	memMu   sync.Mutex
	mem     sessions.MemoryReport
	memAt   time.Time
	memWait chan struct{} // closed when an update in progress finishes
}

func New(m *sessions.Manager, web *auth.Web, handoffs *handoff.Service, st *settings.Store, log *slog.Logger, o Options) *UI {
	u := &UI{m: m, web: web, handoffs: handoffs, settings: st, log: log, o: o, pages: map[string]*template.Template{}, previews: map[string]cachedPreview{},
		pinTries: auth.NewLockout(5, 15*time.Minute, 15*time.Minute)}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(embedded, "templates/layout.html", "templates/parts.html"))
	for _, page := range []string{"overview", "profiles", "profile", "profile-form", "agents", "settings"} {
		t := template.Must(base.Clone())
		u.pages[page] = template.Must(t.ParseFS(embedded, "templates/"+page+".html"))
	}
	return u
}

// Register adds the UI's routes.
func (u *UI) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return u.web.RequireAdmin(f) }
	static, _ := fs.Sub(embedded, "static")
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})))

	mux.Handle("GET /{$}", admin(u.overview))
	mux.Handle("GET /chip", admin(u.chip))
	mux.Handle("GET /profiles", admin(u.profilesPage))
	mux.Handle("GET /profiles/new", admin(u.newProfile))
	mux.Handle("POST /profiles", admin(u.createProfile))
	mux.Handle("GET /profiles/{name}", admin(u.profilePage))
	mux.Handle("GET /profiles/{name}/side", admin(u.profileSide))
	mux.Handle("GET /profiles/{name}/preview.png", admin(u.preview))
	mux.Handle("GET /profiles/{name}/edit", admin(u.editProfile))
	mux.Handle("POST /profiles/{name}/edit", admin(u.saveProfile))
	mux.Handle("POST /profiles/{name}/start", admin(u.startProfile))
	mux.Handle("POST /profiles/{name}/stop", admin(u.stopProfile))
	mux.Handle("POST /profiles/{name}/delete", admin(u.deleteProfile))
	mux.Handle("POST /profiles/{name}/handback", admin(u.handBack))
	mux.Handle("GET /agents", admin(u.agentsPage))
	mux.Handle("POST /agents/tokens", admin(u.createToken))
	mux.Handle("POST /agents/tokens/{id}/revoke", admin(u.revokeToken))
	mux.Handle("GET /settings", admin(u.settingsPage))
	mux.Handle("POST /settings", admin(u.saveSettings))
	mux.Handle("POST /settings/pin", admin(u.savePIN))
	mux.Handle("POST /settings/password", admin(u.changePassword))
	mux.Handle("POST /settings/sign-out-others", admin(u.signOutOthers))
}

// page is what every app page's layout needs.
type page struct {
	Title string
	Nav   string // the nav item to mark current
	Help  helpChip
	Flash flash
	// Profiles lists the profile names, for the header's search.
	Profiles []string
	// Data is the page's own.
	Data any
}

type helpChip struct {
	Count int
	Link  string // the first waiting profile's live view
}

type flash struct {
	Text  string
	Error bool
}

// csp lets pages load only this server's scripts and styles. htmx is
// configured not to eval, so no inline script is needed anywhere.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"media-src 'self' blob:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func headers(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", csp)
}

// render writes a whole page.
func (u *UI) render(w http.ResponseWriter, r *http.Request, status int, name, title, nav string, data any) {
	p := page{Title: title, Nav: nav, Data: data, Flash: takeFlash(w, r)}
	p.Help = u.helpChip()
	if all, err := u.m.Profiles().List(r.Context()); err == nil {
		for _, pr := range all {
			p.Profiles = append(p.Profiles, pr.Name)
		}
	}
	u.execute(w, status, name, "layout", p)
}

// fragment writes one named part of a page, for htmx to swap in.
func (u *UI) fragment(w http.ResponseWriter, status int, name, part string, data any) {
	u.execute(w, status, name, part, data)
}

func (u *UI) execute(w http.ResponseWriter, status int, name, tmpl string, data any) {
	var buf bytes.Buffer
	if err := u.pages[name].ExecuteTemplate(&buf, tmpl, data); err != nil {
		u.log.Error("rendering a page", "page", name, "part", tmpl, "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	headers(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (u *UI) helpChip() helpChip {
	var c helpChip
	for _, s := range u.m.List() {
		if s.State == sessions.NeedsHelp {
			if c.Count == 0 {
				c.Link = "/profiles/" + url.PathEscape(s.Profile)
			}
			c.Count++
		}
	}
	return c
}

// chip is the header's "N needs you", polled by every page.
func (u *UI) chip(w http.ResponseWriter, _ *http.Request) {
	u.fragment(w, http.StatusOK, "overview", "chip", u.helpChip())
}

// The flash is a one-time message shown on the page a form redirects to.
const flashCookie = "bp_flash"

func setFlash(w http.ResponseWriter, text string, isError bool) {
	v := "ok:" + text
	if isError {
		v = "err:" + text
	}
	//nolint:gosec // a message for the next page, no secret; Secure would lose it over plain HTTP
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(v), Path: "/", MaxAge: 30, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func takeFlash(w http.ResponseWriter, r *http.Request) flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return flash{}
	}
	//nolint:gosec // as in setFlash
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return flash{}
	}
	kind, text, _ := strings.Cut(v, ":")
	if len(text) > 300 {
		text = text[:300]
	}
	return flash{Text: text, Error: kind == "err"}
}

// done redirects after a form: to back, with a message. htmx follows
// HX-Redirect; a plain form follows the 303.
func done(w http.ResponseWriter, r *http.Request, back, msg string, isError bool) {
	if msg != "" {
		setFlash(w, msg, isError)
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", back)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther) //nolint:gosec // back is a path on this site (backTo)
}

// memory is the memory report, refreshed at most every few seconds:
// reading it asks Docker for each browser's usage, which takes a moment.
func (u *UI) memory(ctx context.Context) sessions.MemoryReport {
	u.memMu.Lock()
	if time.Since(u.memAt) < 5*time.Second {
		defer u.memMu.Unlock()
		return u.mem
	}
	if wait := u.memWait; wait != nil {
		u.memMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		u.memMu.Lock()
		defer u.memMu.Unlock()
		return u.mem
	}
	wait := make(chan struct{})
	u.memWait = wait
	u.memMu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	rep := u.m.Memory(ctx)
	cancel()

	u.memMu.Lock()
	defer u.memMu.Unlock()
	u.mem, u.memAt, u.memWait = rep, time.Now(), nil
	close(wait)
	return rep
}

// forget drops the cached memory report after something started or stopped.
func (u *UI) forget() {
	u.memMu.Lock()
	u.memAt = time.Time{}
	u.memMu.Unlock()
}

// baseURL is how agents reach this server.
func (u *UI) baseURL(r *http.Request) string {
	if u.o.PublicURL != "" {
		return strings.TrimRight(u.o.PublicURL, "/")
	}
	scheme := "http"
	if u.web.IsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// cdpURL is the WebSocket URL agents connect to for a profile.
func (u *UI) cdpURL(r *http.Request, profile string) string {
	base := u.baseURL(r)
	base = "ws" + strings.TrimPrefix(base, "http")
	return base + "/cdp/" + url.PathEscape(profile)
}

var funcs = template.FuncMap{
	"gb":          formatBytes,
	"ago":         ago,
	"lastUsed":    lastUsed,
	"minutesOf":   func(d time.Duration) int { return int(d / time.Minute) },
	"screenLabel": func(s string) string { return strings.Replace(s, "x", " × ", 1) },
	"menuData":    func(v profileView, up bool) menuView { return menuView{v, up} },
	"span":        func(d overviewData, span string) spanned { return spanned{d, span} },
	"list":        func(s ...string) []string { return s },
	"ramBytes":    func(host int64, percent int) int64 { return host * int64(percent) / 100 },
	"wsURL":       func(base string) string { return "ws" + strings.TrimPrefix(base, "http") },
	"seconds": func(d time.Duration) string {
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		return minutes(d)
	},
	"minutes":  minutes,
	"initials": initials,
	"color":    colorIndex,
	"upper":    strings.ToUpper,
	"join":     strings.Join,
	"clock":    func(t time.Time) string { return t.Local().Format("15:04:05") },
	"hhmm":     func(t time.Time) string { return t.Local().Format("15:04") },
	"pct":      func(part, whole int64) float64 { return percent(part, whole) },
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	},
}

// menuView is a profile's ⋯ menu; Up opens it upwards, for table rows.
type menuView struct {
	profileView
	Up bool
}

func formatBytes(n int64) string {
	switch {
	case n <= 0:
		return "0 MB"
	case n < 1<<30:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}

func percent(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	p := float64(part) * 100 / float64(whole)
	return min(max(p, 0), 100)
}

// ago says how long ago t was, as people say it.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return t.Local().Format("2 Jan")
	}
}

// minutes formats a duration in whole minutes or hours.
func minutes(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
}

// initials are up to two letters for a name's badge: "research-bot" → "RB".
func initials(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '.' })
	out := ""
	for _, p := range parts {
		out += string([]rune(p)[0])
		if len(out) == 2 {
			break
		}
	}
	if out == "" && name != "" {
		out = string([]rune(name)[0])
	}
	return strings.ToUpper(out)
}

// colorIndex picks one of a few badge colours, stable for a name.
func colorIndex(name string) int {
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h % 4
}
