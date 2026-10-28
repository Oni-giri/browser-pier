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
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	d, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
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
