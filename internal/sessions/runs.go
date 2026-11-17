package sessions

import (
	"fmt"
	"slices"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// Kept runs (PROJ-158). When a session ends, its activity log moves to a
// list kept for its profile, so the admin can read what an agent did
// after it stopped the browser, or auto-stop did. In memory only: an
// orchestrator restart forgets them all, and nothing of them goes to the
// database, a file or the orchestrator's log. No agent reads them.

const (
	// keptRuns is how many runs a profile keeps.
	keptRuns = 5
	// keptFor is how long after it ended a run is kept.
	keptFor = 24 * time.Hour
	// maxKeptBytes caps every profile's kept runs together; the oldest
	// run goes first.
	maxKeptBytes = 8 << 20
)

// EndReason is how a run ended, from a closed set: the kept run and its
// last entry carry it, in BrowserPier's own words (String). A failure
// never quotes the container's log, Session.Error or CrashReason, which
// a page can influence.
type EndReason string

const (
	EndAgent    EndReason = "agent"    // an agent stopped it
	EndAdmin    EndReason = "admin"    // stopped from the app (or deleted)
	EndIdle     EndReason = "idle"     // the profile's auto-stop
	EndRoom     EndReason = "room"     // stopped to make room for another browser
	EndOutdated EndReason = "outdated" // an older BrowserPier started it (Reconcile)
	EndSecrets  EndReason = "secrets"  // it held filled secrets when BrowserPier restarted (Reconcile)
	EndShutdown EndReason = "shutdown" // stopped with BrowserPier (StopAll)
	// EndStopped: a stop begun by the BrowserPier before a restart, whose
	// reason went with it.
	EndStopped EndReason = "stopped"
	// Failures, named as PROJ-157's webhooks name them (sessions.Failure
	// on its branch): one set for both once they meet.
	EndCrashed     EndReason = "crashed"       // the browser exited by itself
	EndUnclean     EndReason = "unclean_close" // it didn't close cleanly when stopped (PROJ-157's name)
	EndLost        EndReason = "lost"          // its container was lost track of
	EndStartFailed EndReason = "start_failed"  // it never got to Running
)

// Failed reports whether the run ended in a failure.
func (r EndReason) Failed() bool {
	switch r {
	case EndCrashed, EndUnclean, EndLost, EndStartFailed:
		return true
	}
	return false
}

// StopReason is why a browser is stopped, or how it ended: an EndReason,
// with the agent's name or the idle time it names.
type StopReason struct {
	end   EndReason
	agent string
	idle  time.Duration
}

// StopByAgent is the reason of a stop an agent asked for.
func StopByAgent(agent string) StopReason { return StopReason{end: EndAgent, agent: agent} }

// StopFromApp is the reason of a stop from the web UI.
var StopFromApp = StopReason{end: EndAdmin}

func stopIdle(after time.Duration) StopReason { return StopReason{end: EndIdle, idle: after} }

var (
	stopRoom       = StopReason{end: EndRoom}
	stopOutdated   = StopReason{end: EndOutdated}
	stopSecrets    = StopReason{end: EndSecrets}
	stopShutdown   = StopReason{end: EndShutdown}
	endCrashed     = StopReason{end: EndCrashed}
	endUnclean     = StopReason{end: EndUnclean}
	endLost        = StopReason{end: EndLost}
	endStartFailed = StopReason{end: EndStartFailed}
	endUnknown     = StopReason{end: EndStopped}
)

// Reason is the EndReason.
func (r StopReason) Reason() EndReason { return r.end }

// String is the reason in BrowserPier's own fixed words.
func (r StopReason) String() string {
	switch r.end {
	case EndAgent:
		return "Stopped by " + r.agent
	case EndAdmin:
		return "Stopped from the app"
	case EndIdle:
		return "Stopped: idle for " + idleFor(r.idle)
	case EndRoom:
		return "Stopped to make room for another browser"
	case EndOutdated:
		return "Stopped: started by an older BrowserPier"
	case EndSecrets:
		return "Stopped: BrowserPier restarted while it held filled secrets"
	case EndShutdown:
		return "Stopped with BrowserPier"
	case EndCrashed:
		return "The browser exited unexpectedly"
	case EndUnclean:
		return "The browser didn't close cleanly: recent logins may be lost"
	case EndLost:
		return "Lost track of the browser container"
	case EndStartFailed:
		return "The browser didn't start"
	}
	return "Stopped"
}

// idleFor words a profile's auto-stop: "30 min", "2 h", "1 h 30 min".
func idleFor(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d%time.Hour < time.Minute:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// Run is a browser run whose session has ended, kept for its profile.
type Run struct {
	SessionID      string
	Mode           profiles.Mode
	Started, Ended time.Time
	// End is how it ended; EndText says it in words, as its last entry does.
	End     EndReason
	EndText string
	// StartedBy is the agent that started it, by token name, or "". FromApp
	// is set when it was started from the web UI. Neither is set for a run
	// BrowserPier picked up after a restart.
	StartedBy string
	FromApp   bool
	// Entries are its activity log, newest first, scrubbed and with titles
	// cut, as Activity shows them.
	Entries []Activity
}

// keptRun is a Run and what it counts for against maxKeptBytes.
type keptRun struct {
	Run
	bytes int
}

// keepRunLocked ends l's log with how the run ended, and moves it to its
// profile's kept runs, scrubbed with sc: the session's final secrets,
// which the caller then forgets. It must run before Forget, or a secret
// registered after an entry was written would stay in it for good.
// m.mu must be held, and l must no longer be live.
func (m *Manager) keepRunLocked(l *live, s Session, why StopReason, inHelp bool, sc *scrub.Scrubber) {
	now := m.now()
	if inHelp {
		// The handoff ends after the session, where nothing can be noted
		// any more: said here, so it is in the kept run.
		l.noteLocked(now, KindHandoff, helpCancelledStopped)
	}
	l.noteLocked(now, KindSession, why.String())
	r := &keptRun{Run: Run{
		SessionID: s.ID, Mode: s.Mode, Started: s.CreatedAt, Ended: now,
		End: why.end, EndText: why.String(), StartedBy: l.startedBy, FromApp: l.fromApp,
		Entries: make([]Activity, len(l.activity)),
	}}
	for i, a := range l.activity {
		a = shown(sc, a)
		a.TokenID, a.Tool, a.CallID = "", "", ""
		r.Entries[len(r.Entries)-1-i] = a
		r.bytes += entrySize(a)
	}
	l.activity, l.activityBytes = nil, 0
	m.keepLocked(s.Profile, r)
}

// keepLocked adds r to profile's kept runs, and applies the bounds: runs
// over keptFor old go, a failed start replaces a failed start that is the
// newest run (so a retry loop can't push out useful runs), a profile
// keeps keptRuns, and all of them together at most maxKeptBytes, the
// oldest run going first. m.mu must be held.
func (m *Manager) keepLocked(profile string, r *keptRun) {
	m.expireLocked(m.now())
	runs := m.kept[profile]
	if n := len(runs); r.End == EndStartFailed && n > 0 && runs[n-1].End == EndStartFailed {
		runs = runs[:n-1]
	}
	runs = append(runs, r)
	if len(runs) > keptRuns {
		runs = slices.Clone(runs[len(runs)-keptRuns:])
	}
	m.kept[profile] = runs
	for {
		total, oldest := 0, ""
		for p, runs := range m.kept {
			for _, r := range runs {
				total += r.bytes
			}
			if oldest == "" || runs[0].Ended.Before(m.kept[oldest][0].Ended) {
				oldest = p
			}
		}
		if total <= maxKeptBytes || oldest == "" {
			return
		}
		m.dropOldestLocked(oldest)
	}
}

// dropOldestLocked drops profile's oldest kept run.
func (m *Manager) dropOldestLocked(profile string) {
	if runs := m.kept[profile]; len(runs) > 1 {
		m.kept[profile] = slices.Delete(runs, 0, 1)
	} else {
		delete(m.kept, profile)
	}
}

// expireLocked drops the runs that ended keptFor ago or more.
func (m *Manager) expireLocked(now time.Time) {
	for p, runs := range m.kept {
		runs = slices.DeleteFunc(runs, func(r *keptRun) bool { return now.Sub(r.Ended) >= keptFor })
		if len(runs) == 0 {
			delete(m.kept, p)
		} else {
			m.kept[p] = runs
		}
	}
}

// Runs returns the profile's kept runs, newest first, as copies: its last
// keptRuns runs that ended within keptFor. For the web UI only: no agent
// route reads them.
func (m *Manager) Runs(profile string) []Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(m.now())
	runs := m.kept[profile]
	out := make([]Run, len(runs))
	for i, r := range runs {
		c := r.Run
		c.Entries = slices.Clone(r.Entries)
		out[len(out)-1-i] = c
	}
	return out
}
