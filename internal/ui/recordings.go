package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Oni-giri/browser-pier/internal/recording"
)

// Session recordings (PROJ-23), for the signed-in admin only: no agent
// token reaches any of these routes, and no API or MCP route lists them.

// noAgents answers 401 to a request that carries a token (an agent's)
// and no admin session.
func (u *UI) noAgents(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" && !u.web.SignedIn(r) {
			http.Error(w, "recordings are for the admin, signed in", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recordingRow is one recording in the profile page's list.
type recordingRow struct {
	recording.Index
	URL string
}

// recordings lists a profile's recordings, newest first.
func (u *UI) recordings(profile string) []recordingRow {
	if u.o.Recordings == nil {
		return nil
	}
	var out []recordingRow
	for _, ix := range u.o.Recordings.Sessions(profile) {
		out = append(out, recordingRow{Index: ix, URL: "/profiles/" + url.PathEscape(profile) + "/recordings/" + ix.Session})
	}
	return out
}

// reasonLabels say why a stretch wasn't recorded, as the player shows it.
var reasonLabels = map[recording.Reason]string{
	recording.Person:  "A person had control",
	recording.Handoff: "A handoff was open",
	recording.Idle:    "The agent was idle",
	recording.Still:   "Nothing changed on screen",
	recording.Stopped: "The browser was stopping",
	recording.Lost:    "Lost: no room, or the stream broke",
	recording.Off:     "Recording was off",
	recording.Paused:  "Recording was paused",
}

type playerData struct {
	Profile string
	Index   recording.Index
	// JSON is what player.js plays: segment URLs and times, gaps and
	// their reasons, in milliseconds since the epoch.
	JSON string
}

type playerSegment struct {
	URL   string `json:"url"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type playerGap struct {
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Reason string `json:"reason"`
	Label  string `json:"label"`
}

func (u *UI) playerPage(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("name"), r.PathValue("session")
	if u.o.Recordings == nil {
		http.NotFound(w, r)
		return
	}
	ix, err := u.o.Recordings.Get(name, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var out struct {
		Segments []playerSegment `json:"segments"`
		Gaps     []playerGap     `json:"gaps"`
	}
	base := "/profiles/" + url.PathEscape(name) + "/recordings/" + id + "/"
	for _, s := range ix.Segments {
		out.Segments = append(out.Segments, playerSegment{URL: base + s.File, Start: s.Start.UnixMilli(), End: s.End.UnixMilli()})
	}
	for _, g := range ix.Gaps {
		if g.End.Before(ix.Start()) || g.Start.After(ix.End()) {
			continue // before the first segment or after the last: nothing to play around
		}
		out.Gaps = append(out.Gaps, playerGap{Start: g.Start.UnixMilli(), End: g.End.UnixMilli(), Reason: string(g.Reason), Label: reasonLabels[g.Reason]})
	}
	b, _ := json.Marshal(out)
	u.render(w, r, http.StatusOK, "recording", "Recording · "+name, "profiles", playerData{Profile: name, Index: ix, JSON: string(b)})
}

// segment serves one segment file to the signed-in admin; anyone else
// gets a 404, as for a recording that doesn't exist.
func (u *UI) segment(w http.ResponseWriter, r *http.Request) {
	if u.o.Recordings == nil || !u.web.SignedIn(r) {
		http.NotFound(w, r)
		return
	}
	f, seg, err := u.o.Recordings.Open(r.PathValue("name"), r.PathValue("session"), r.PathValue("file"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// ServeContent answers Range requests; the name is ours, never the stream's.
	http.ServeContent(w, r, seg.File, seg.End, f)
}

func (u *UI) deleteRecording(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("name"), r.PathValue("session")
	back := "/profiles/" + url.PathEscape(name)
	if u.o.Recordings == nil {
		http.NotFound(w, r)
		return
	}
	if err := u.o.Recordings.Delete(name, id); err != nil {
		if errors.Is(err, recording.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		u.log.Warn("deleting a recording", "profile", name, "session", id, "err", err)
		done(w, r, back, "Couldn't delete the recording: "+err.Error(), true)
		return
	}
	u.log.Info("recording deleted from the UI", "profile", name, "session", id)
	done(w, r, back, "Recording deleted", false)
}

func (u *UI) deleteRecordings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := "/profiles/" + url.PathEscape(name)
	if u.o.Recordings == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := u.m.Profiles().Get(r.Context(), name); err != nil {
		http.NotFound(w, r)
		return
	}
	n := len(u.o.Recordings.Sessions(name))
	if err := u.o.Recordings.DeleteAll(name); err != nil {
		u.log.Warn("deleting a profile's recordings", "profile", name, "err", err)
		done(w, r, back, "Couldn't delete every recording: "+err.Error(), true)
		return
	}
	u.log.Info("recordings deleted from the UI", "profile", name, "recordings", n)
	done(w, r, back, fmt.Sprintf("%d recordings deleted", n), false)
}

// lengthLabel is "4 min 12 s", "58 s".
func lengthLabel(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// mbLabel is "3.1 MB".
func mbLabel(n int64) string {
	if n < 100_000 {
		return fmt.Sprintf("%d kB", (n+500)/1000)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}
