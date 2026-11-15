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
	// ErrHumanDriving: an agent can't act while a person has control.
	ErrHumanDriving = errors.New("a person is driving this browser")
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
	// this session (StartOptions.AllowAds).
	BlockAds  bool
	CreatedAt time.Time
	UpdatedAt time.Time
	// InUseBy names the agents connected to the browser right now, by
	// token name. Only live sessions have any; it isn't stored.
	InUseBy []string
	// HumanControl is set while a person drives the browser from the live
	// view; agents can only watch meanwhile. Not stored.
	HumanControl bool
	// Viewers counts open live views. Not stored.
	Viewers int
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const sessionColumns = `id, profile, state, mode, container_id, last_log, error, created_at, updated_at, block_ads`

func insertSession(ctx context.Context, db *sql.DB, s Session) error {
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Profile, s.State, s.Mode, s.ContainerID, s.LastLog, s.Error, s.CreatedAt.UnixMilli(), s.UpdatedAt.UnixMilli(), s.BlockAds)
	return err
}

func updateSession(ctx context.Context, db *sql.DB, s Session) error {
	_, err := db.ExecContext(ctx, `UPDATE sessions SET state = ?, container_id = ?, last_log = ?, error = ?, updated_at = ?, block_ads = ? WHERE id = ?`,
		s.State, s.ContainerID, s.LastLog, s.Error, s.UpdatedAt.UnixMilli(), s.BlockAds, s.ID)
	return err
}

func getSession(ctx context.Context, db *sql.DB, id string) (Session, error) {
	s, err := scanSession(db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

func liveSessions(ctx context.Context, db *sql.DB) ([]Session, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions
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
	err := row.Scan(&s.ID, &s.Profile, &s.State, &s.Mode, &s.ContainerID, &s.LastLog, &s.Error, &created, &updated, &s.BlockAds)
	s.CreatedAt, s.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return s, err
}
