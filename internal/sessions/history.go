package sessions

import "context"

// LastSessions returns each profile's most recent session, live or past,
// by profile name. Live sessions come from memory, so they show who is
// using them.
func (m *Manager) LastSessions(ctx context.Context) (map[string]Session, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions s
		WHERE created_at = (SELECT MAX(created_at) FROM sessions WHERE profile = s.profile)`)
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
