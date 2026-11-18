// Package recording keeps what an agent did on a profile's screen, so the
// admin can replay it (PROJ-23). The browser image's GET /record streams
// fragmented MP4 from the X display; the Recorder cuts it into files that
// play alone, under <data dir>/recordings/<profile>/<session>/, never
// under profiles/ (which is mounted into the browser container).
//
// It records only while Gate.Decide says so: the profile's setting is on,
// the session is running, no person has control, no handoff is open, and
// an agent is acting or acted in the last Tail. The session manager feeds
// the gate and tells the Recorder at every change, under its own lock, so
// a person's input can never reach a page while a recording runs.
//
// The stream comes from a container a page may have taken over: the
// Recorder reads box sizes and nothing more, caps every box, segment and
// minute, and never decodes video.
package recording

import "time"

// Tail is how long after an agent's last action recording goes on.
const Tail = 30 * time.Second

// Reason says why a stretch of a session wasn't recorded.
type Reason string

const (
	// Person: a person had control in the live view.
	Person Reason = "person"
	// Handoff: a handoff was open (Needs help).
	Handoff Reason = "handoff"
	// Idle: no agent had acted for Tail.
	Idle Reason = "idle"
	// Still: nothing changed on screen.
	Still Reason = "still"
	// Stopped: the browser was starting, stopping or stopped.
	Stopped Reason = "stopped"
	// Lost: the stream broke or went over a cap, or there was no room.
	Lost Reason = "lost"
	// Off: the profile's Record sessions setting was off.
	Off Reason = "off"
	// Paused: BrowserPier paused recording itself (Manager.PauseRecording).
	Paused Reason = "paused"
)

// Gate is everything that decides whether a session's screen may be
// recorded now.
type Gate struct {
	// Enabled is the profile's Record sessions setting.
	Enabled bool
	// Running: the session is Running, not starting, stopping or with a
	// person for a handoff.
	Running bool
	// NeedsHelp: a handoff is open.
	NeedsHelp bool
	// Human: a person has control in the live view.
	Human bool
	// Paused: something holds recording off (Manager.PauseRecording).
	Paused bool
	// AgentActive: an agent is connected over CDP, has a page call or an
	// input action under way, or ended one less than Tail ago.
	AgentActive bool
}

// Decide is the one place that says whether to record now, and if not,
// why not. Anything new that gives a person the browser must reach the
// recorder through it.
func (g Gate) Decide() (bool, Reason) {
	switch {
	case !g.Enabled:
		return false, Off
	case g.NeedsHelp: // the person driving during a handoff is the handoff's
		return false, Handoff
	case g.Human:
		return false, Person
	case !g.Running:
		return false, Stopped
	case g.Paused:
		return false, Paused
	case !g.AgentActive:
		return false, Idle
	}
	return true, ""
}
