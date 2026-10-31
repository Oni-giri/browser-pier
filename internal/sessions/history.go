package sessions

import (
	"context"
	"database/sql"
	"errors"
)

// LastSessions returns each profile's most recent session, live or past,
// by profile name. Live sessions come from memory, so they show who is
// using them.
func (m *Manager) LastSessions(ctx context.Context) (map[string]Session, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM (
		SELECT *, ROW_NUMBER() OVER (PARTITION BY profile ORDER BY created_at DESC, rowid DESC) AS n FROM sessions
	) WHERE n = 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Session{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out[s.Profile] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, s := range m.List() {
		out[s.Profile] = s
	}
	return out, nil
}

// LastSession returns a profile's most recent session, live or past; the
// zero Session if it never ran.
func (m *Manager) LastSession(ctx context.Context, profile string) (Session, error) {
	if s, ok := m.Live(profile); ok {
		return s, nil
	}
	s, err := scanSession(m.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions
		WHERE profile = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, profile))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, nil
	}
	return s, err
}
