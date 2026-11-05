package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// status is one of the design's five words, plus Stopping.
type status struct {
	Key   string // help, running, starting, stopping, stopped, failed: the CSS class
	Label string
}

var (
	stHelp     = status{"help", "Needs you"}
	stRunning  = status{"running", "Running"}
	stStarting = status{"starting", "Starting"}
	stStopping = status{"starting", "Stopping"}
	stStopped  = status{"stopped", "Stopped"}
	stFailed   = status{"failed", "Failed"}
)

// order sorts what needs attention first.
var order = map[string]int{"help": 0, "failed": 1, "running": 2, "starting": 3, "stopped": 4}

// profileView is a profile as the cards, rows and live view show it.
type profileView struct {
	profiles.Profile
	Status   status
	Session  sessions.Session // the live or last session; zero if never run
	Activity string           // "Waiting on 2FA · 3 min", "Idle", "Last used yesterday"
	Agent    string           // who's using it, or "No agent"
	Memory   int64
	Error    string // why it failed
	Ask      *handoff.Handoff
	Emph     bool // the screen's one emphasis card: the first that needs you
	// Preview is set while the browser can show a screenshot.
	Preview  bool
	PreviewV int64 // changes every 10 s, so cached previews refresh
	CDPURL   string
	// IconURL is the site's favicon, versioned; "" for the fallback glyph
	// (.Glyph and .Color, from the profile).
	IconURL string
}

func (p profileView) Live() bool { return p.Session.State.Live() }

// ModeLabel is the profile's mode as shown.
func (p profileView) ModeLabel() string { return modeLabel(p.DefaultMode) }

// PolicyLabel is the handoff policy as shown.
func (p profileView) PolicyLabel() string { return policyLabel(p.HandoffPolicy) }

// AutoStopLabel is "15 min idle" or "Never".
func (p profileView) AutoStopLabel() string { return autoStopLabel(p.AutoStop) }

func modeLabel(m profiles.Mode) string {
	if m == profiles.ModeAPI {
		return "API"
	}
	return "Visual"
}

func policyLabel(p profiles.HandoffPolicy) string {
	switch p {
	case profiles.PolicyLink:
		return "One tap"
	case profiles.PolicySignIn:
		return "Full sign-in"
	}
	return "Link + PIN"
}

func autoStopLabel(d time.Duration) string {
	if d <= 0 {
		return "Never"
	}
	return fmt.Sprintf("%d min idle", int(d.Minutes()))
}

// views builds every profile's view, needs-you first.
func (u *UI) views(ctx context.Context, r *http.Request) ([]profileView, error) {
	all, err := u.m.Profiles().List(ctx)
	if err != nil {
		return nil, err
	}
	last, err := u.m.LastSessions(ctx)
	if err != nil {
		return nil, err
	}
	asks := map[string]*handoff.Handoff{}
	if open, err := u.handoffs.Pending(ctx); err == nil {
		for i := range open {
			asks[open[i].SessionID] = &open[i]
		}
	} else {
		u.log.Warn("listing handoffs", "err", err)
	}
	mem := map[string]int64{}
	if len(u.m.List()) > 0 {
		for _, b := range u.memory(ctx).Browsers {
			mem[b.Profile] = b.Bytes
		}
	}
	out := make([]profileView, 0, len(all))
	for _, p := range all {
		out = append(out, u.view(r, p, last[p.Name], asks, mem))
	}
	slices.SortStableFunc(out, func(a, b profileView) int {
		if d := order[a.Status.Key] - order[b.Status.Key]; d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (u *UI) view(r *http.Request, p profiles.Profile, s sessions.Session, asks map[string]*handoff.Handoff, mem map[string]int64) profileView {
	v := profileView{Profile: p, Session: s, Agent: "No agent", Memory: mem[p.Name], CDPURL: u.cdpURL(r, p.Name), IconURL: u.IconURL(p.Name)}
	if len(s.InUseBy) > 0 {
		v.Agent = strings.Join(s.InUseBy, ", ")
	}
	now := time.Now()
	switch s.State {
	case sessions.NeedsHelp:
		v.Status = stHelp
		v.Ask = asks[s.ID]
		v.Activity = "Waiting for you"
		if v.Ask != nil {
			v.Agent = v.Ask.Agent
			v.Activity = "Waiting on " + strings.ToLower(v.Ask.Reason.Label()) + " · " + minutes(now.Sub(v.Ask.CreatedAt))
		}
	case sessions.Running:
		v.Status = stRunning
		switch {
		case s.HumanControl:
			v.Activity = "You have control"
		case len(s.InUseBy) > 0:
			v.Activity = "In use"
		default:
			v.Activity = "Up for " + minutes(now.Sub(s.CreatedAt))
		}
	case sessions.Starting:
		v.Status, v.Activity = stStarting, "Just requested"
	case sessions.Stopping:
		v.Status, v.Activity = stStopping, "Stopping"
	case sessions.Failed:
		v.Status = stFailed
		v.Activity = "Crashed " + ago(s.UpdatedAt)
		v.Error = s.Error
		if why := sessions.CrashReason(s.LastLog); why != "" {
			v.Error += ": " + why
		}
		if v.Error == "" {
			v.Error = s.LastLog
		}
	case sessions.Stopped:
		v.Status, v.Activity = stStopped, "Last used "+ago(s.UpdatedAt)
	default:
		v.Status, v.Activity = stStopped, "Never started"
	}
	if s.State == sessions.Running || s.State == sessions.NeedsHelp {
		v.Preview = true
		v.PreviewV = now.Unix() / 10
	}
	return v
}

type profilesData struct {
	All, Shown        []profileView
	Show, View, Query string
	Counts            map[string]int
	Mem               memoryView
	Running           int
}

func (u *UI) profilesPage(w http.ResponseWriter, r *http.Request) {
	all, err := u.views(r.Context(), r)
	if err != nil {
		u.log.Error("listing profiles", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	// The header's search jumps straight to a profile it names exactly.
	if q != "" && r.Header.Get("HX-Request") == "" {
		for _, v := range all {
			if v.Name == q {
				http.Redirect(w, r, "/profiles/"+url.PathEscape(q), http.StatusSeeOther)
				return
			}
		}
	}
	d := profilesData{All: all, Show: r.URL.Query().Get("show"), View: r.URL.Query().Get("view"), Query: q, Counts: map[string]int{}}
	if d.View != "table" {
		d.View = "grid"
	}
	for _, v := range all {
		k := group(v.Status.Key)
		d.Counts[k]++
		if k == "running" || v.Status.Key == "help" {
			d.Running++
		}
		if (d.Show == "" || d.Show == k) && (q == "" || strings.Contains(v.Name, q)) {
			v.Emph = v.Status.Key == "help" && !emphasised(d.Shown)
			d.Shown = append(d.Shown, v)
		}
	}
	if d.Running > 0 {
		d.Mem = u.memoryView(r, all)
	}
	if r.Header.Get("HX-Request") == "true" {
		u.fragment(w, http.StatusOK, "profiles", "list-poll", d)
		return
	}
	u.render(w, r, http.StatusOK, "profiles", "Profiles", "profiles", d)
}

// group is the filter a status falls under.
func group(key string) string {
	switch key {
	case "starting":
		return "running"
	case "failed":
		return "stopped"
	}
	return key
}

// preview is a small screenshot of a running browser for its card. It
// doesn't count as use, so a page left open doesn't keep browsers alive.
func (u *UI) preview(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	png, err := u.previewPNG(r.Context(), name)
	if err != nil {
		http.Error(w, "no preview", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// Not kept by the browser: it shows a logged-in page, and the admin
	// may sign out. The server keeps it a few seconds instead.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(png) //nolint:gosec // served as image/png with nosniff
}

// previewPNG is a running browser's screenshot for its card, taken at most
// every few seconds however many pages ask.
func (u *UI) previewPNG(ctx context.Context, profile string) ([]byte, error) {
	u.prevMu.Lock()
	defer u.prevMu.Unlock()
	if p, ok := u.previews[profile]; ok && time.Since(p.at) < 8*time.Second {
		return p.png, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	png, err := u.m.Screenshot(ctx, profile, 640)
	if err != nil {
		delete(u.previews, profile)
		return nil, err
	}
	u.previews[profile] = cachedPreview{png: png, at: time.Now()}
	return png, nil
}

type cachedPreview struct {
	png []byte
	at  time.Time
}

func (u *UI) startProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := backTo(r, "/profiles")
	if _, err := u.m.Profiles().Get(r.Context(), name); err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	_, _, err := u.m.Start(ctx, name, "")
	u.forget()
	var noRoom *sessions.NoRoomError
	switch {
	case errors.As(err, &noRoom):
		list := make([]string, len(noRoom.Running))
		for i, b := range noRoom.Running {
			list[i] = b.String()
		}
		done(w, r, back, "Not enough memory to start "+name+". In use: "+strings.Join(list, "; "), true)
	case err != nil:
		u.log.Warn("starting a browser from the UI", "profile", name, "err", err)
		done(w, r, back, "Couldn't start "+name+": "+err.Error(), true)
	default:
		u.log.Info("browser started from the UI", "profile", name)
		done(w, r, back, name+" started", false)
	}
}

func (u *UI) stopProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := backTo(r, "/profiles")
	s, ok := u.m.Live(name)
	if !ok {
		done(w, r, back, name+" isn't running", false)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	_, err := u.m.Stop(ctx, s.ID)
	u.forget()
	if err != nil {
		u.log.Warn("stopping a browser from the UI", "profile", name, "err", err)
		done(w, r, back, "Couldn't stop "+name+": "+err.Error(), true)
		return
	}
	u.log.Info("browser stopped from the UI", "profile", name)
	done(w, r, back, name+" stopped", false)
}

func (u *UI) deleteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := u.m.Profiles().Get(r.Context(), name); err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	if s, ok := u.m.Live(name); ok {
		if _, err := u.m.Stop(ctx, s.ID); err != nil {
			done(w, r, "/profiles", "Couldn't stop "+name+" to delete it: "+err.Error(), true)
			return
		}
	}
	if err := u.m.DeleteProfile(ctx, name); err != nil {
		u.log.Warn("deleting a profile from the UI", "profile", name, "err", err)
		done(w, r, "/profiles", "Couldn't delete "+name+": "+err.Error(), true)
		return
	}
	u.forget()
	u.log.Info("profile deleted from the UI", "profile", name)
	done(w, r, "/profiles", name+" deleted", false)
}

// backTo is where a form goes after: its "back" field when that is a
// path on this site, or else fallback.
func backTo(r *http.Request, fallback string) string {
	b := r.PostFormValue("back")
	if strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") && !strings.Contains(b, `\`) {
		return b
	}
	return fallback
}

// emphasised reports whether one of vs already has the emphasis card.
func emphasised(vs []profileView) bool {
	return slices.ContainsFunc(vs, func(v profileView) bool { return v.Emph })
}
