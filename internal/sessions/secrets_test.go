package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

const testSecret = "vault-Value 42/é"

func TestRegisteredSecretsAreHiddenAndGoWithTheSession(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	d, err := db.Open(context.Background(), filepath.Join(f.dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	m, err := NewManager(d, f.rt, f.layout, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Profiles().Create(context.Background(), profiles.New("github")); err != nil {
		t.Fatal(err)
	}
	s := f.start(m)
	if err := m.RegisterSecret(s.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	_ = m.RegisterSecret(s.ID, "abc") // too short: ignored
	if n := m.SecretCount(s.ID); n != 1 {
		t.Fatalf("%d secrets, want 1", n)
	}

	// The activity log never holds it.
	m.Note(s.ID, "claude", "Downloaded "+testSecret+".csv (3 B)")
	if got := m.Activity(s.ID)[0].What; got != "Downloaded <hidden>.csv (3 B)" {
		t.Errorf("activity: %q", got)
	}

	// Scoped to the profiles a token allows.
	if got := m.Scrubber(func(p string) bool { return p == "google" }).String(testSecret); got != "<hidden>" {
		t.Errorf("google's own scrubber: %q", got)
	}
	if sc := m.Scrubber(func(p string) bool { return p == "github" }); sc != nil {
		t.Error("another profile's token hides google's secret")
	}

	// It stays through a handoff: the page may still show it.
	if _, err := m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	if m.SecretCount(s.ID) != 1 {
		t.Error("a handoff dropped the secrets")
	}

	// A crash's words are kept after the session: scrubbed first.
	f.rt.LastLog = "level=ERROR msg=exiting err=\"page said " + testSecret + "\""
	f.rt.Exit(s.ContainerID, 1)
	eventually(t, "the session to fail", func() bool {
		got, err := getSession(context.Background(), d, s.ID)
		return err == nil && got.State == Failed
	})
	stored, err := getSession(context.Background(), d, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.LastLog, testSecret) || !strings.Contains(stored.LastLog, "<hidden>") {
		t.Errorf("stored last log: %q", stored.LastLog)
	}
	eventually(t, "the secrets to go", func() bool { return m.SecretCount(s.ID) == 0 })
	if err := m.RegisterSecret(s.ID, testSecret); !errors.Is(err, ErrNotRunning) {
		t.Errorf("RegisterSecret on an ended session: %v", err)
	}
	if err := m.RegisterSecret("no-such-session", testSecret); !errors.Is(err, ErrNotRunning) {
		t.Errorf("RegisterSecret on an unknown session: %v", err)
	}
	s2 := f.start(m)
	if m.SecretCount(s2.ID) != 0 || m.Scrubber(func(string) bool { return true }) != nil {
		t.Error("the next session inherited secrets")
	}
	if strings.Contains(logs.String(), "vault-Value") {
		t.Errorf("the secret reached the log:\n%s", logs.String())
	}
	assertNotStored(t, d, "vault-Value")
}

// assertNotStored checks no text column of the sessions table holds s.
func assertNotStored(t *testing.T, d *sql.DB, s string) {
	t.Helper()
	rows, err := d.Query(`SELECT id, last_log, error FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, lastLog, msg sql.NullString
		if err := rows.Scan(&id, &lastLog, &msg); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(lastLog.String+msg.String, s) {
			t.Errorf("session %s stores the secret", id.String)
		}
	}
}

// The values are in memory only: a browser that outlives the orchestrator
// with secrets typed into it is stopped when picked up (logins kept, the
// usual stop), not answered for unscrubbed. Only the mark is stored.
func TestReconcileStopsABrowserWhoseSecretsWereLost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.Profiles().Create(ctx, profiles.New("github")); err != nil {
		t.Fatal(err)
	}
	filled := f.start(f.m)
	if err := f.m.RegisterSecret(filled.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	if err := f.m.RegisterSecret(filled.ID, "another-"+testSecret); err != nil {
		t.Fatal(err)
	}
	clean, _, err := f.m.Start(ctx, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.stored(filled.ID); !got.HadFills {
		t.Fatal("the session's mark wasn't stored")
	}
	if got := f.stored(clean.ID); got.HadFills {
		t.Fatal("a session without secrets is marked")
	}
	f.m.Close() // the orchestrator goes down; browsers keep running

	m := f.manager()
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := m.Live("google"); ok {
		t.Fatalf("a browser whose secrets were lost was picked up: %+v", s)
	}
	if got := f.stored(filled.ID); got.State != Stopped {
		t.Fatalf("filled session: %+v", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), filled.ID) {
		t.Errorf("no warning names the session:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Error("the log holds the secret")
	}
	if s, ok := m.Live("github"); !ok || s.ID != clean.ID {
		t.Fatalf("the browser without secrets wasn't picked up: %+v, %t", s, ok)
	}
	// Started again, it begins unmarked.
	s, attached, err := m.Start(ctx, "google", "")
	if err != nil || attached {
		t.Fatalf("restart: %+v, %t, %v", s, attached, err)
	}
	if got := f.stored(s.ID); got.HadFills {
		t.Fatal("the new session starts marked")
	}
}

// A browser reopens its tabs as it starts, and one could show a secret
// typed into the last session again, with nothing left to hide it: the
// next start removes Chromium's record of the tabs first, and keeps the
// cookies. A start after a session with no secret keeps the tabs.
func TestStartAfterASecretForgetsTheTabs(t *testing.T) {
	f := newFixture(t)
	chrome := f.layout.dir("google", "chrome")
	outside := t.TempDir()
	write := func(name string) {
		t.Helper()
		p := filepath.Join(chrome, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(chrome, name))
		return err == nil
	}
	tabs := []string{"Default/Sessions/Session_13400000000000000", "Default/Sessions/Tabs_13400000000000000", "Default/Current Session", "Default/Last Tabs"}
	kept := []string{"Default/Cookies", "Default/Session Storage/000003.log", "Default/Preferences", "Local State"}

	s := f.start(f.m)
	if err := f.m.RegisterSecret(s.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	f.stop(s.ID)
	for _, n := range append(tabs, kept...) {
		write(n)
	}
	s = f.start(f.m)
	for _, n := range tabs {
		if exists(n) {
			t.Errorf("%s is still there after a session with a secret", n)
		}
	}
	for _, n := range kept {
		if !exists(n) {
			t.Errorf("%s was removed", n)
		}
	}

	// That session had none: the next start reopens its tabs.
	f.stop(s.ID)
	write(tabs[0])
	s = f.start(f.m)
	if !exists(tabs[0]) {
		t.Error("the tabs of a session without a secret were removed")
	}

	// A link the browser left in their place is removed, not followed.
	if err := f.m.RegisterSecret(s.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	f.stop(s.ID)
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(chrome, "Default", "Sessions")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(chrome, "Default", "Sessions")); err != nil {
		t.Fatal(err)
	}
	s = f.start(f.m)
	if exists("Default/Sessions") {
		t.Error("the link is still there")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("a file the link pointed at went: %v", err)
	}
	f.stop(s.ID)
}

// A start that failed before its container was created never ran the
// browser: the start after it still forgets the tabs of the session with
// a secret before it.
func TestAFailedStartDoesntHideAnEarlierSecret(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	if err := f.m.RegisterSecret(s.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	f.stop(s.ID)
	if _, err := f.m.db.Exec(`INSERT INTO sessions (id, profile, state, mode, container_id, last_log, error, created_at, updated_at, block_ads)
		VALUES ('failed1', 'google', 'failed', 'api', '', '', 'creating the browser container: no', ?, ?, 0)`,
		time.Now().Add(time.Second).UnixMilli(), time.Now().Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.layout.dir("google", "chrome"), "Default", "Sessions", "Session_1")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.stop(f.start(f.m).ID)
	if _, err := os.Stat(p); err == nil {
		t.Error("the tabs of the session with a secret were reopened after a failed start")
	}
}
