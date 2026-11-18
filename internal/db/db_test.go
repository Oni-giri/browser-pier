package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	if _, err := d.ExecContext(ctx, `INSERT INTO profiles (name, default_mode, auto_stop_minutes, screen_width, screen_height, proxy, handoff_policy, created_at, updated_at) VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0)`); err != nil {
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
	mustExec(`INSERT INTO profiles (name, default_mode, auto_stop_minutes, screen_width, screen_height, proxy, handoff_policy, created_at, updated_at) VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0)`)
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

func TestOpenKeepsOddCharactersInThePath(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"bp?x", "bp#x", "bp%41x", "bp x"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "browserpier.db")
		d, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = d.Close()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: the database isn't where it was asked to be: %v", name, err)
		}
	}
}

// TestIconColumnsReachExistingProfiles: a database from before profile
// icons (PROJ-44) keeps its profiles, which get no site and the default icon.
func TestIconColumnsReachExistingProfiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	before := len(migrations) - 1
	for i, q := range migrations[:before] {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := d.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", before)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO profiles (name, default_mode, auto_stop_minutes, screen_width, screen_height, proxy, handoff_policy, created_at, updated_at) VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var site, inferred, glyph, color string
	err = d.QueryRowContext(ctx, `SELECT site, site_inferred, icon_glyph, icon_color FROM profiles WHERE name = 'google'`).Scan(&site, &inferred, &glyph, &color)
	if err != nil || site != "" || inferred != "" || glyph != "" || color != "" {
		t.Fatalf("after migrating: site %q, inferred %q, glyph %q, colour %q, %v; want all empty", site, inferred, glyph, color, err)
	}
}

// TestBlockAdsIsOffForExistingProfiles: profiles from before PROJ-119 ran
// without blocking and keep running without it; their sessions too.
func TestBlockAdsIsOffForExistingProfiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	before := len(migrations) - 1
	for i, q := range migrations[:before] {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := d.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", before)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO profiles (name, default_mode, auto_stop_minutes, screen_width, screen_height, proxy, handoff_policy, created_at, updated_at) VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0);
		INSERT INTO sessions (id, profile, state, mode, created_at, updated_at) VALUES ('s1', 'google', 'stopped', 'visual', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var profile, session bool
	if err := d.QueryRowContext(ctx, `SELECT block_ads FROM profiles WHERE name = 'google'`).Scan(&profile); err != nil || profile {
		t.Fatalf("existing profile: block_ads %v, %v; want off", profile, err)
	}
	if err := d.QueryRowContext(ctx, `SELECT block_ads FROM sessions WHERE id = 's1'`).Scan(&session); err != nil || session {
		t.Fatalf("existing session: block_ads %v, %v; want off", session, err)
	}
}

func TestRecordSessionsIsOffForExistingProfiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	before := len(migrations) - 1
	for i, q := range migrations[:before] {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := d.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", before)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO profiles (name, default_mode, auto_stop_minutes, screen_width, screen_height, proxy, handoff_policy, created_at, updated_at) VALUES ('google', 'visual', 15, 1440, 900, '', 'pin', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	d, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var on bool
	if err := d.QueryRowContext(ctx, `SELECT record_sessions FROM profiles WHERE name = 'google'`).Scan(&on); err != nil || on {
		t.Fatalf("existing profile: record_sessions %v, %v; want off", on, err)
	}
}
