package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// the page may still show it then, and hiding it costs nothing. Since the
// browser reopens its tabs as it starts (RestoreOnStartup, PROJ-112), a tab
// could show the value again after a stop and a start, with nothing left
// to hide it: so the next start of the profile removes Chromium's record
// of the tabs first (forgetTabs), and that start opens none. Cookies and
// the rest of the profile are kept. Values
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

// lockScrubber locks m.mu and returns the session's Scrubber as its
// secrets are now. The Scrubber is built before the lock is taken, since
// building one can take a while, and again if a secret came in meanwhile
// (secrets only grow until the session ends).
func (m *Manager) lockScrubber(sessionID string) *scrub.Scrubber {
	for {
		n := m.secrets.Count(sessionID)
		sc := m.secrets.For(sessionID)
		m.mu.Lock()
		if m.secrets.Count(sessionID) == n {
			return sc
		}
		m.mu.Unlock()
	}
}

// sessionRestore is what Chromium keeps the tabs it reopens in, under the
// user data dir: Default/Sessions (its Session_<time> and Tabs_<time>
// files) since Chromium 100, and the four files of older versions, kept
// here in case a profile comes from one. Nothing else: the cookies,
// session-only ones included, and sessionStorage stay where they are.
var sessionRestore = []string{
	filepath.Join("Default", "Sessions"),
	filepath.Join("Default", "Current Session"), filepath.Join("Default", "Current Tabs"),
	filepath.Join("Default", "Last Session"), filepath.Join("Default", "Last Tabs"),
}

// forgetTabsIfFilled removes the profile's record of its tabs before
// session id's browser starts when the profile's last browser to run had
// a secret typed into it (Session.HadFills): a restored tab could show the
// value again (a GET form's URL, a page that echoes it), and the new
// session's scrubber doesn't know it. The last browser to run is the
// latest other session with a container; one that failed before creating
// one never ran Chromium, so it doesn't hide an earlier fill.
func (m *Manager) forgetTabsIfFilled(ctx context.Context, profile, id string) error {
	var had bool
	err := m.db.QueryRowContext(ctx, `SELECT had_fills FROM sessions WHERE profile = ? AND id != ? AND container_id != ''
		ORDER BY created_at DESC, rowid DESC LIMIT 1`, profile, id).Scan(&had)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !had) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := forgetTabs(m.layout.dir(profile, "chrome")); err != nil {
		return err
	}
	m.log.Info("not reopening the tabs of the profile's last session: a secret was typed into it", "profile", profile, "session", id)
	return nil
}

// forgetTabs removes Chromium's record of the tabs it reopens from a user
// data dir. The browser can write anything there, symlinks included, so
// it goes through os.Root, which won't follow a link out of the folder.
func forgetTabs(chromeDir string) error {
	root, err := os.OpenRoot(chromeDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, name := range sessionRestore {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}
