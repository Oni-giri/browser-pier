// Package db opens the orchestrator's SQLite database and keeps its schema
// up to date.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; pure Go, so builds stay CGO-free
)

// migrations are applied in order, each once. Append new ones; never edit
// one that has shipped.
var migrations = []string{
	`CREATE TABLE profiles (
		name              TEXT PRIMARY KEY,
		default_mode      TEXT NOT NULL,
		auto_stop_minutes INTEGER NOT NULL,
		screen_width      INTEGER NOT NULL,
		screen_height     INTEGER NOT NULL,
		proxy             TEXT NOT NULL DEFAULT '',
		handoff_policy    TEXT NOT NULL,
		created_at        INTEGER NOT NULL,
		updated_at        INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		id           TEXT PRIMARY KEY,
		profile      TEXT NOT NULL REFERENCES profiles(name) ON DELETE CASCADE,
		state        TEXT NOT NULL,
		mode         TEXT NOT NULL,
		container_id TEXT NOT NULL DEFAULT '',
		last_log     TEXT NOT NULL DEFAULT '',
		error        TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL
	);
	-- One live session per profile, enforced by the database as well as the code.
	CREATE UNIQUE INDEX sessions_one_live ON sessions(profile)
		WHERE state IN ('starting', 'running', 'needs_help', 'stopping');`,

	`CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	-- Agent tokens are stored as the SHA-256 of the token, never the token.
	CREATE TABLE agent_tokens (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		hash         TEXT NOT NULL UNIQUE,
		hint         TEXT NOT NULL,
		all_profiles INTEGER NOT NULL,
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0
	);
	-- A token scoped to a list loses a profile when the profile is deleted, so
	-- a new profile with the same name isn't reachable by an old token.
	CREATE TABLE agent_token_profiles (
		token_id TEXT NOT NULL REFERENCES agent_tokens(id) ON DELETE CASCADE,
		profile  TEXT NOT NULL REFERENCES profiles(name) ON DELETE CASCADE,
		PRIMARY KEY (token_id, profile)
	);
	-- Signed-in browsers, by the SHA-256 of their cookie.
	CREATE TABLE admin_sessions (
		hash         TEXT PRIMARY KEY,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		remember     INTEGER NOT NULL,
		user_agent   TEXT NOT NULL DEFAULT ''
	);`,

	// A profile's manual time zone and language, and the exit location it
	// last ran with.
	`ALTER TABLE profiles ADD COLUMN tz_override TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN locale_override TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN geo_country TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN geo_time_zone TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN geo_ip TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN geo_checked_at INTEGER NOT NULL DEFAULT 0;`,

	// Handoffs: an agent asking a person to take over. The link's token and
	// the browser it was opened in are stored as SHA-256 hashes.
	`CREATE TABLE handoffs (
		id           TEXT PRIMARY KEY,
		session_id   TEXT NOT NULL,
		profile      TEXT NOT NULL REFERENCES profiles(name) ON DELETE CASCADE,
		agent        TEXT NOT NULL,
		reason       TEXT NOT NULL,
		detail       TEXT NOT NULL,
		origin       TEXT NOT NULL,
		policy       TEXT NOT NULL,
		status       TEXT NOT NULL,
		note         TEXT NOT NULL DEFAULT '',
		token_hash   TEXT NOT NULL UNIQUE,
		browser_hash TEXT NOT NULL DEFAULT '',
		pin_failures INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		finished_at  INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX handoffs_session ON handoffs(session_id);`,

	// One open handoff per session, even when two agents ask at once. Any
	// duplicates left by that race are cancelled first, keeping the oldest.
	`UPDATE handoffs SET status = 'cancelled', note = 'another handoff was open'
		WHERE status = 'pending' AND rowid NOT IN (SELECT min(rowid) FROM handoffs WHERE status = 'pending' GROUP BY session_id);
	CREATE UNIQUE INDEX handoffs_one_pending ON handoffs(session_id) WHERE status = 'pending';`,

	// The web UI reads each profile's latest session on every refresh.
	`CREATE INDEX sessions_profile_created ON sessions(profile, created_at);`,

	// Whether a profile's pages may reach the local network.
	`ALTER TABLE profiles ADD COLUMN local_network INTEGER NOT NULL DEFAULT 0;`,

	// A profile's main site, as set and as read from its history, and the
	// icon it falls back to when the site's favicon isn't known.
	`ALTER TABLE profiles ADD COLUMN site TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN site_inferred TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN icon_glyph TEXT NOT NULL DEFAULT '';
	ALTER TABLE profiles ADD COLUMN icon_color TEXT NOT NULL DEFAULT '';`,

	// A handoff can end by itself when the page reaches an address the
	// agent gave (PROJ-118): the pattern, and whether it ended that way.
	`ALTER TABLE handoffs ADD COLUMN until_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE handoffs ADD COLUMN auto_done INTEGER NOT NULL DEFAULT 0;`,

	// Whether a profile's browser blocks ad and tracker domains (PROJ-119):
	// off for the profiles that exist, which ran without it; new ones get
	// the admin's default. And whether each session did, since an agent
	// can turn it off for one session.
	`ALTER TABLE profiles ADD COLUMN block_ads INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sessions ADD COLUMN block_ads INTEGER NOT NULL DEFAULT 0;`,

	// Whether BrowserPier typed a secret into a session's page (PROJ-125):
	// the values live in memory only, so a browser that outlives the
	// orchestrator with one is stopped when it is picked up, not answered
	// for unscrubbed. Only the flag is stored, never a value.
	`ALTER TABLE sessions ADD COLUMN had_fills INTEGER NOT NULL DEFAULT 0;`,

	// PROJ-23: the profile's Record sessions setting, off until the admin
	// turns it on.
	`ALTER TABLE profiles ADD COLUMN record_sessions INTEGER NOT NULL DEFAULT 0;`,

	// What {sticky} becomes in a profile's proxy (PROJ-153): empty for
	// the profiles that exist, which get one the first time they need it.
	`ALTER TABLE profiles ADD COLUMN sticky_token TEXT NOT NULL DEFAULT '';`,

	// The admin's two-step recovery codes (PROJ-162), by their SHA-256:
	// 80 random bits each, so a slow hash would add nothing. Each row goes
	// when its code is used.
	`CREATE TABLE admin_recovery_codes (
		hash       TEXT PRIMARY KEY,
		created_at INTEGER NOT NULL
	);`,
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	// Build the URI with url.URL so a ?, # or % in the path stays part of
	// the filename instead of being read as URI syntax.
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite allows one writer anyway, and this avoids
	// SQLITE_BUSY between our own connections.
	d.SetMaxOpenConns(1)
	if err := migrate(ctx, d); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return d, nil
}

func migrate(ctx context.Context, d *sql.DB) error {
	var version int
	if err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this BrowserPier (%d); upgrade it", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
