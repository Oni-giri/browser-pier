package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenMigratesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO profiles VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must not re-run migrations or lose data.
	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var n, version int
	if err := d.QueryRowContext(ctx, "SELECT count(*) FROM profiles").Scan(&n); err != nil || n != 1 {
		t.Fatalf("profiles after reopen = %d, %v", n, err)
	}
	if err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
}

func TestOneLiveSessionPerProfile(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	mustExec := func(q string) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO profiles VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0)`)
	mustExec(`INSERT INTO sessions (id, profile, state, mode, created_at, updated_at) VALUES ('a', 'google', 'stopped', 'api', 0, 0)`)
	mustExec(`INSERT INTO sessions (id, profile, state, mode, created_at, updated_at) VALUES ('b', 'google', 'running', 'api', 0, 0)`)
	_, err = d.ExecContext(ctx, `INSERT INTO sessions (id, profile, state, mode, created_at, updated_at) VALUES ('c', 'google', 'starting', 'api', 0, 0)`)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second live session: err = %v, want a UNIQUE violation", err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open = %v, want a newer-schema error", err)
	}
}
