package sessions

import "github.com/Oni-giri/browser-pier/internal/scrub"

// RegisterSecret records a value BrowserPier itself typed into a live
// session's page, one the agent must never see, so that every answer to
// agents and the activity log hide it (PROJ-125, internal/scrub). PROJ-57's
// vault fills will call it; until then only tests do. There is no REST or
// MCP way to reach it: an agent knows what it typed itself.
//
// The value is kept in memory only, never logged or stored, and goes when
// the session ends. It stays through a handoff or a person taking control:
// the page may still show it then, and hiding it costs nothing. Values
// under scrub.MinRunes characters are ignored. It fails with ErrNotRunning
// when the session has ended or isn't known.
func (m *Manager) RegisterSecret(sessionID, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.findLocked(sessionID) == nil {
		return ErrNotRunning
	}
	m.secrets.Add(sessionID, value)
	return nil
}

// SecretCount is how many secrets a session has registered; 0 once it has
// ended.
func (m *Manager) SecretCount(sessionID string) int { return m.secrets.Count(sessionID) }

// Scrubber hides the secrets of every live session whose profile allow
// accepts (an agent token's Allows), nil when they have none. Scoped to the
// token, it hides only what that agent could otherwise read, never
// confirming another profile's secret.
func (m *Manager) Scrubber(allow func(profile string) bool) *scrub.Scrubber {
	m.mu.Lock()
	var ids []string
	for _, l := range m.live {
		if allow(l.sess.Profile) {
			ids = append(ids, l.sess.ID)
		}
	}
	m.mu.Unlock()
	return m.secrets.For(ids...)
}
