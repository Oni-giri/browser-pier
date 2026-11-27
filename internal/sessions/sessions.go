// Package sessions manages the session lifecycle (Starting, Running, Needs
// help, Stopping, Stopped, Failed), one running session per profile.
package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// State is where a session is in its lifecycle.
type State string

const (
	Starting  State = "starting"
	Running   State = "running"
	NeedsHelp State = "needs_help"
	Stopping  State = "stopping"
	Stopped   State = "stopped"
	Failed    State = "failed"
)

// Live reports whether a session in this state has a container.
func (s State) Live() bool {
	switch s {
	case Starting, Running, NeedsHelp, Stopping:
		return true
	}
	return false
}

var (
	ErrNotFound    = errors.New("session not found")
	ErrProfileBusy = errors.New("the profile's browser is running; stop it first")
	ErrNotRunning  = errors.New("the session isn't running")
	ErrStartFailed = errors.New("the browser failed to start")
	ErrClosed      = errors.New("the session manager is closed")
	// ErrAgentDriving: a person can't take control while an agent is
	// connected or has just sent input.
	ErrAgentDriving = errors.New("an agent is driving this browser")
	// ErrNoHolder: TakeControl was called without saying who takes it, a
	// wiring mistake (PROJ-184).
	ErrNoHolder = errors.New("sessions: control taken with no holder")
	// ErrControlEnded: control was handed back while Take control waited
	// for an agent's action to end (PROJ-184).
	ErrControlEnded = errors.New("control was handed back before it was taken")
	// ErrHumanDriving: an agent can't act while a person has control.
	ErrHumanDriving = errors.New("a person is driving this browser")
	// ErrInputBusy: an agent's input action is still under way in this
	// browser; one runs at a time.
	ErrInputBusy = errors.New("another input is still running in this browser; wait for it to finish")
)

// Session is one run of a profile's browser.
type Session struct {
	ID          string
	Profile     string
	Mode        profiles.Mode
	State       State
	ContainerID string
	// LastLog is the container's last log line, kept when it stops or fails.
	LastLog string
	// Error says why the session failed, when it did.
	Error string
	// BlockAds is whether the browser blocks ad and tracker domains: the
	// profile's setting when it started, unless an agent turned it off for
	// this session (StartOptions.AllowAds), or no list had been downloaded
	// yet when it started (AdListMissing).
	BlockAds bool
	// AdListMissing is set when the session was to block ads and trackers
	// but started without a list (none downloaded yet), so it blocks
	// nothing. In memory only: a session picked up after an orchestrator
	// restart has BlockAds false without it.
	AdListMissing bool
	// HadFills is set once BrowserPier has typed a secret into the page
	// (RegisterSecret), and never cleared. Stored on its own (markHadFills),
	// never by updateSession, so an older copy can't clear it.
	HadFills bool
	// takeoverGen is the highest takeover generation handed out, saved
	// before it is (saveTakeoverGen); helpEpoch counts the times the
	// session went to Needs help (HelpEpoch). Both outlive a restart.
	takeoverGen, helpEpoch uint64
	CreatedAt              time.Time
	UpdatedAt              time.Time
	// InUseBy names the agents connected to the browser right now, by
	// token name. Only live sessions have any; it isn't stored.
	InUseBy []string
	// HumanControl is set while a person drives the browser from the live
	// view; agents can only watch meanwhile. Not stored.
	HumanControl bool
	// Viewers counts open live views. Not stored.
	Viewers int
	// Record is the profile's Record sessions setting as the session
	// applies it, and Recording whether its screen is being recorded
	// right now (PROJ-23). Admin only: never shown to agents. Not stored.
	Record, Recording bool
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const sessionColumns = `id, profile, state, mode, container_id, last_log, error, created_at, updated_at, block_ads`

// selectColumns are sessionColumns and what is stored on its own.
const selectColumns = sessionColumns + `, had_fills, takeover_gen, help_epoch`

func insertSession(ctx context.Context, db *sql.DB, s Session) error {
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Profile, s.State, s.Mode, s.ContainerID, s.LastLog, s.Error, s.CreatedAt.UnixMilli(), s.UpdatedAt.UnixMilli(), s.BlockAds)
	return err
}

func updateSession(ctx context.Context, db *sql.DB, s Session) error {
	_, err := db.ExecContext(ctx, `UPDATE sessions SET state = ?, container_id = ?, last_log = ?, error = ?, updated_at = ?, block_ads = ?, help_epoch = ? WHERE id = ?`,
		s.State, s.ContainerID, s.LastLog, s.Error, s.UpdatedAt.UnixMilli(), s.BlockAds, s.helpEpoch, s.ID)
	return err
}

// saveTakeoverGen records that session id hands out takeover gen, before
// it does (PROJ-196).
func saveTakeoverGen(ctx context.Context, db *sql.DB, id string, gen uint64) error {
	_, err := db.ExecContext(ctx, `UPDATE sessions SET takeover_gen = max(takeover_gen, ?) WHERE id = ?`, gen, id)
	return err
}

// pendingHandoff reports whether session id has a handoff open (the
// handoffs table, internal/handoff).
func pendingHandoff(ctx context.Context, db *sql.DB, id string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM handoffs WHERE session_id = ? AND status = 'pending'`, id).Scan(&n)
	return n > 0, err
}

// markHadFills records that a secret was typed into session id's page.
func markHadFills(ctx context.Context, db *sql.DB, id string) error {
	_, err := db.ExecContext(ctx, `UPDATE sessions SET had_fills = 1 WHERE id = ?`, id)
	return err
}

func getSession(ctx context.Context, db *sql.DB, id string) (Session, error) {
	s, err := scanSession(db.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

func liveSessions(ctx context.Context, db *sql.DB) ([]Session, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+selectColumns+` FROM sessions
		WHERE state IN ('starting', 'running', 'needs_help', 'stopping') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var created, updated int64
	err := row.Scan(&s.ID, &s.Profile, &s.State, &s.Mode, &s.ContainerID, &s.LastLog, &s.Error, &created, &updated, &s.BlockAds, &s.HadFills, &s.takeoverGen, &s.helpEpoch)
	s.CreatedAt, s.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return s, err
}
