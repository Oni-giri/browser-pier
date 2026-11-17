package sessions

import (
	"context"
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
	// FromPage: What holds words a page chose, a downloaded file's name;
	// an agent reads the entry fenced (PROJ-156).
	FromPage bool
	// TokenID, Tool and CallID are the agent call that wrote the entry
	// (CallFrom), when known: the token's ID, the route's tool and the
	// call id, if it had one. The web UI doesn't show them; an agent
	// reads its own token's entries (RecentCalls).
	TokenID string
	Tool    string
	CallID  string
}

// Note adds to a live session's activity log; it does nothing once the
// session has ended. what must never hold typed text or other page
// content: it is shown as is.
func (m *Manager) Note(sessionID, agent, what string) {
	m.note(sessionID, Activity{Agent: agent, What: what})
}

// NoteFor is Note for an entry an agent call wrote: the call ctx carries
// (CallFrom), if any, is recorded with it.
func (m *Manager) NoteFor(ctx context.Context, sessionID, agent, what string) {
	m.note(sessionID, byCall(ctx, Activity{Agent: agent, What: what}))
}

// NoteDownloadFor is NoteFor for an entry naming a download, whose file
// name a page chose.
func (m *Manager) NoteDownloadFor(ctx context.Context, sessionID, agent, what string) {
	m.note(sessionID, byCall(ctx, Activity{Agent: agent, What: what, FromPage: true}))
}

// NoteAttention is NoteFor for an entry waiting for a person.
func (m *Manager) NoteAttention(ctx context.Context, sessionID, agent, what string) {
	m.note(sessionID, byCall(ctx, Activity{Agent: agent, What: what, Attention: true}))
}

// byCall records the call ctx carries in a.
func byCall(ctx context.Context, a Activity) Activity {
	if c := CallFrom(ctx); c != nil {
		a.TokenID, a.Tool, a.CallID = c.tokenID, c.tool, c.id
	}
	return a
}

func (m *Manager) note(sessionID string, a Activity) {
	// A file name or an address in an entry may hold a secret the page
	// echoed (PROJ-125).
	sc := m.lockScrubber(sessionID)
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return
	}
	a.What = sc.String(a.What)
	a.At = m.now()
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
