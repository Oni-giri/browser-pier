package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/recording"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

type profilePageData struct {
	profileView
	Log []sessions.Activity
	// StartNow: the page was opened right after creating the profile, to
	// log in; it starts the browser itself.
	StartNow bool
	Mem      int64
	// Blocked is how many connections the running browser refused for
	// the ad and tracker list; BlockedRead whether it could be read.
	Blocked     int64
	BlockedRead bool
	// Recordings are the profile's session recordings, newest first;
	// RetentionDays how long they are kept.
	Recordings    []recordingRow
	RetentionDays int
	// liveRec is the live session's recording so far, for LogLink.
	liveRec *recording.Index
}

// LogLink is the player at the moment of an agent log entry, while the
// live session's recording covers it; "" otherwise. The log goes with the
// session, so after a stop the player stands alone.
func (d profilePageData) LogLink(at time.Time) string {
	ix := d.liveRec
	if ix == nil || at.Before(ix.Start().Add(-time.Second)) || at.After(ix.End()) {
		return ""
	}
	return "/profiles/" + url.PathEscape(d.Name) + "/recordings/" + ix.Session + "?t=" + strconv.FormatInt(at.UnixMilli(), 10)
}

// RecordingLabel says whether the live session is being recorded, for a
// profile that records: "" when it doesn't.
func (d profilePageData) RecordingLabel() string {
	switch {
	case !d.Live() || !d.Session.Record:
		return ""
	case d.Session.Recording:
		return "Recording"
	}
	return "Recording paused"
}

// AdsLabel says what the ad and tracker blocking is doing: the live
// session's count, or the profile's setting.
func (d profilePageData) AdsLabel() string {
	switch {
	case d.Live() && d.Session.AdListMissing:
		return "No list yet"
	case d.Live() && !d.Session.BlockAds && d.BlockAds:
		return "Off this session"
	case d.Live() && d.Session.BlockAds && d.BlockedRead:
		return fmt.Sprintf("%d blocked", d.Blocked)
	case d.Live():
		if d.Session.BlockAds {
			return "On"
		}
		return "Off"
	case d.BlockAds:
		return "On"
	}
	return "Off"
}

// UpFor is how long the browser has been running.
func (d profilePageData) UpFor() string {
	if !d.Live() {
		return "—"
	}
	return minutes(time.Since(d.Session.CreatedAt))
}

func (u *UI) profileData(ctx context.Context, r *http.Request, name string) (profilePageData, bool) {
	p, err := u.m.Profiles().Get(ctx, name)
	if err != nil {
		return profilePageData{}, false
	}
	last, err := u.m.LastSession(ctx, name)
	if err != nil {
		u.log.Warn("reading sessions", "err", err)
	}
	asks := map[string]*handoff.Handoff{}
	if open, err := u.handoffs.Pending(ctx); err == nil {
		for i := range open {
			asks[open[i].SessionID] = &open[i]
		}
	}
	mem := map[string]int64{}
	if last.State.Live() {
		for _, b := range u.memory(ctx).Browsers {
			mem[b.Profile] = b.Bytes
		}
	}
	d := profilePageData{profileView: u.view(r, p, last, asks, mem), Recordings: u.recordings(name)}
	if u.o.Recordings != nil {
		d.RetentionDays = int(u.o.Recordings.Limits().Retention / (24 * time.Hour))
		if d.Live() {
			if ix, err := u.o.Recordings.Get(name, d.Session.ID); err == nil {
				d.liveRec = &ix
			}
		}
	}
	if d.Live() {
		d.Log = u.m.Activity(d.Session.ID)
		if d.Session.BlockAds {
			d.Blocked, d.BlockedRead = u.m.Blocked(ctx, name)
		}
	}
	return d, true
}

func (u *UI) profilePage(w http.ResponseWriter, r *http.Request) {
	d, ok := u.profileData(r.Context(), r, r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.StartNow = r.URL.Query().Get("start") == "1" && !d.Live()
	u.render(w, r, http.StatusOK, "profile", d.Name, "profiles", d)
}

// profileSide is the live view's side column, polled to stay current.
func (u *UI) profileSide(w http.ResponseWriter, r *http.Request) {
	d, ok := u.profileData(r.Context(), r, r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	u.fragment(w, http.StatusOK, "profile", "side", d)
}

// handBack answers the agent's request for help from the app, as the
// phone page does: done or can't, with a note, and the browser goes back
// to the agent.
func (u *UI) handBack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := "/profiles/" + url.PathEscape(name)
	s, ok := u.m.Live(name)
	if !ok {
		done(w, r, back, name+" isn't running", true)
		return
	}
	open, err := u.handoffs.Pending(r.Context())
	if err != nil {
		u.log.Error("listing handoffs", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	status := handoff.Done
	if r.PostFormValue("status") == string(handoff.Cant) {
		status = handoff.Cant
	}
	for _, h := range open {
		if h.SessionID != s.ID {
			continue
		}
		if _, err := u.handoffs.Finish(r.Context(), h, status, r.PostFormValue("note")); err != nil {
			u.log.Error("finishing a handoff from the UI", "handoff", h.ID, "err", err)
			done(w, r, back, "Couldn't hand back: "+err.Error(), true)
			return
		}
		u.log.Info("handoff finished from the UI", "handoff", h.ID, "profile", name, "status", status)
		done(w, r, back, "Handed back to "+h.Agent, false)
		return
	}
	// No request open: just let go of the controls.
	u.m.ReleaseControl(s.ID)
	done(w, r, back, "Handed back", false)
}
