package sessions

import (
	"context"
	"fmt"
	"time"

	"github.com/Oni-giri/browser-pier/internal/scrub"
)

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
//
// The first secret also marks the session in the database (only that it
// had one): if the orchestrator restarts while the browser runs on, the
// values are gone, so Reconcile stops that browser rather than answer for
// it unscrubbed. When that mark can't be stored, the secret is still
// hidden but RegisterSecret fails, so the caller doesn't type it.
func (m *Manager) RegisterSecret(sessionID, value string) error {
	m.markMu.Lock()
	defer m.markMu.Unlock()
	m.mu.Lock()
	l := m.findLocked(sessionID)
	if l == nil {
		m.mu.Unlock()
		return ErrNotRunning
	}
	m.secrets.Add(sessionID, value)
	mark := !l.sess.HadFills && m.secrets.Count(sessionID) > 0
	if mark {
		l.sess.HadFills = true
	}
	m.mu.Unlock()
	if !mark {
		return nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	if err := markHadFills(ctx, m.db, sessionID); err != nil {
		m.mu.Lock()
		l.sess.HadFills = false // the next secret tries again
		m.mu.Unlock()
		return fmt.Errorf("recording that the session has a secret: %w", err)
	}
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
