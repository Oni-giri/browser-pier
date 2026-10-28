package sessions

import (
	"slices"
	"time"
)

// maxActivity is how many entries each session's activity log keeps.
const maxActivity = 50

// Activity is one thing an agent did in a session's browser, for the web
// UI. It is kept in memory only, while the session is live.
type Activity struct {
	At    time.Time
	Agent string // the token's name
	What  string // in words, e.g. "Clicked at 412, 220"; never what was typed
	// Attention marks an entry waiting for a person, such as a handoff.
	Attention bool
}

// Note adds to a live session's activity log; it does nothing once the
// session has ended. what must never hold typed text or other page
// content: it is shown as is.
func (m *Manager) Note(sessionID, agent, what string) { m.note(sessionID, agent, what, false) }

// NoteAttention is Note for an entry waiting for a person.
func (m *Manager) NoteAttention(sessionID, agent, what string) { m.note(sessionID, agent, what, true) }

func (m *Manager) note(sessionID, agent, what string, attention bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return
	}
	a := Activity{At: m.now(), Agent: agent, What: what, Attention: attention}
	if len(l.activity) < maxActivity {
		l.activity = append(l.activity, a)
		return
	}
	copy(l.activity, l.activity[1:])
	l.activity[len(l.activity)-1] = a
}

// Activity returns a live session's last few activity entries, newest
// first; none once it has ended.
func (m *Manager) Activity(sessionID string) []Activity {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return nil
	}
	out := slices.Clone(l.activity)
	slices.Reverse(out)
	return out
}
