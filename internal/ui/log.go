package ui

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Oni-giri/browser-pier/internal/recording"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// The activity log page (PROJ-158): a profile's current run and the runs
// kept after their browser stopped. It sits outside the side column's
// polled fragment, so open runs don't collapse; nothing on it is polled.

// keptSentence says how long runs are kept, from sessions' bounds.
var keptSentence = fmt.Sprintf("Kept in memory for %d hours, the last %d runs. A BrowserPier restart or update clears it.",
	int(sessions.KeptFor.Hours()), sessions.KeptRuns)

type logPageData struct {
	profileView
	// Current is the live run's log, all of it, newest first.
	Current []sessions.Activity
	Runs    []runView
	Kept    string
	// liveRec is the live session's recording so far, for LogLink.
	liveRec *recording.Index
}

// LogLink is the player at the moment of a current run's entry, as on the
// profile page (PROJ-23). Kept runs have none.
func (d logPageData) LogLink(at time.Time) string { return recordingAt(d.Name, d.liveRec, at) }

// logLine is a log entry as the log-entry template shows it, with the
// player at its moment, if any.
type logLine struct {
	sessions.Activity
	Link string
}

// runView is a kept run as the log page shows it.
type runView struct{ sessions.Run }

// Starter says who started the run.
func (r runView) Starter() string {
	switch {
	case r.FromApp:
		return "Started from the app"
	case r.StartedBy != "":
		return "Started by " + r.StartedBy
	}
	return "Picked up after a BrowserPier restart"
}

// ModeLabel is the run's mode as shown.
func (r runView) ModeLabel() string { return modeLabel(r.Mode) }

// Failed is set when the run ended in a failure.
func (r runView) Failed() bool { return r.End.Failed() }

func (u *UI) profileLog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, ok := u.profileData(r.Context(), r, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	out := logPageData{profileView: d.profileView, Kept: keptSentence, liveRec: d.liveRec}
	if d.Live() {
		out.Current = u.m.Activity(d.Session.ID)
	}
	for _, run := range u.m.Runs(name) {
		out.Runs = append(out.Runs, runView{run})
	}
	u.render(w, r, http.StatusOK, "profile-log", name+" · Activity log", "profiles", out)
}

// dayName names the day of t as people say it: Today, Yesterday or the
// date.
func dayName(t time.Time) string {
	t = t.Local()
	y, m, dd := time.Now().Date()
	today := time.Date(y, m, dd, 0, 0, 0, 0, time.Local)
	switch {
	case !t.Before(today):
		return "Today"
	case !t.Before(today.AddDate(0, 0, -1)):
		return "Yesterday"
	}
	return t.Format("Mon 2 Jan")
}
