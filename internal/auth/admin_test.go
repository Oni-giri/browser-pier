package auth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newAdmin(t *testing.T, d *sql.DB, env string) (*Admin, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	a, err := NewAdmin(context.Background(), d, env, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a, &logs
}

func TestFirstRunSetup(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, logs := newAdmin(t, d, "")
	if !a.NeedsSetup() || a.EnvManaged() {
		t.Fatal("fresh install should need setup")
	}
	code := a.setupCode
	if len(code) != 26 || !strings.Contains(logs.String(), "setup_code="+code) {
		t.Fatalf("setup code %q not logged: %s", code, logs)
	}
	if ok, _ := a.CheckPassword(ctx, ""); ok {
		t.Fatal("empty password accepted before setup")
	}
	if err := a.Setup(ctx, "nope", "hunter2hunter2"); !errors.Is(err, ErrWrongSetupCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if err := a.Setup(ctx, code, "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := a.Setup(ctx, code, "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	if a.NeedsSetup() {
		t.Fatal("still needs setup")
	}
	if err := a.Setup(ctx, code, "another-password"); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("second setup: %v", err)
	}
	if ok, _ := a.CheckPassword(ctx, "hunter2hunter2"); !ok {
		t.Fatal("password doesn't verify")
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Fatal("the password was logged")
	}

	// After a restart there's no setup page any more.
	if a2, _ := newAdmin(t, d, ""); a2.NeedsSetup() {
		t.Fatal("setup needed again after restart")
	}
}

func TestEnvPasswordWinsAtEveryStart(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, logs := newAdmin(t, d, "from-env-1")
	if a.NeedsSetup() || !a.EnvManaged() {
		t.Fatal("env password should skip setup")
	}
	if ok, _ := a.CheckPassword(ctx, "from-env-1"); !ok {
		t.Fatal("env password doesn't verify")
	}
	if err := a.ChangePassword(ctx, "from-env-1", "something-else", ""); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("ChangePassword = %v", err)
	}
	cookie, _, _ := a.NewSession(ctx, true, "")
	a, _ = newAdmin(t, d, "from-env-1") // same password: sessions stay
	if _, err := a.Session(ctx, cookie); err != nil {
		t.Fatal("a restart with the same password signed the browser out")
	}
	a, _ = newAdmin(t, d, "from-env-2") // changed in the environment: the reset path
	if _, err := a.Session(ctx, cookie); !errors.Is(err, ErrNoSession) {
		t.Fatal("a browser signed in with the old password is still signed in")
	}
	if ok, _ := a.CheckPassword(ctx, "from-env-1"); ok {
		t.Fatal("old env password still works")
	}
	if ok, _ := a.CheckPassword(ctx, "from-env-2"); !ok {
		t.Fatal("new env password doesn't work")
	}
	if strings.Contains(logs.String(), "from-env") {
		t.Fatal("the password was logged")
	}
}

func TestAdminSessions(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, _ := newAdmin(t, d, "the-password")
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }

	short, s, err := a.NewSession(ctx, false, "Firefox")
	if err != nil || !s.ExpiresAt.Equal(now.Add(SessionFor)) {
		t.Fatalf("NewSession = %+v, %v", s, err)
	}
	long, s2, _ := a.NewSession(ctx, true, "Phone")
	if !s2.ExpiresAt.Equal(now.Add(RememberFor)) || short == long {
		t.Fatalf("remembered session %+v", s2)
	}
	var stored int
	_ = d.QueryRow(`SELECT count(*) FROM admin_sessions WHERE hash = ?`, short).Scan(&stored)
	if stored != 0 {
		t.Fatal("the cookie value is stored as is")
	}

	if got, err := a.Session(ctx, short); err != nil || got.UserAgent != "Firefox" {
		t.Fatalf("Session = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "x", short + "x"} {
		if _, err := a.Session(ctx, bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("Session(%q) = %v", bad, err)
		}
	}

	now = now.Add(SessionFor)
	if _, err := a.Session(ctx, short); !errors.Is(err, ErrNoSession) {
		t.Fatal("a day-long session outlived its day")
	}
	if _, err := a.Session(ctx, long); err != nil {
		t.Fatalf("remembered session expired early: %v", err)
	}

	third, _, _ := a.NewSession(ctx, false, "Laptop")
	if list, _ := a.Sessions(ctx); len(list) != 2 {
		t.Fatalf("Sessions = %d, want 2", len(list))
	}
	if err := a.EndOtherSessions(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Session(ctx, long); !errors.Is(err, ErrNoSession) {
		t.Fatal("other session survived EndOtherSessions")
	}
	if err := a.EndSession(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Session(ctx, third); !errors.Is(err, ErrNoSession) {
		t.Fatal("session survived sign-out")
	}
}

func TestChangePasswordSignsOutOthers(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, _ := newAdmin(t, d, "")
	if err := a.Setup(ctx, a.setupCode, "first-password"); err != nil {
		t.Fatal(err)
	}
	mine, _, _ := a.NewSession(ctx, false, "")
	other, _, _ := a.NewSession(ctx, true, "")
	if err := a.ChangePassword(ctx, "wrong", "second-password", mine); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong current: %v", err)
	}
	if err := a.ChangePassword(ctx, "first-password", "second-password", mine); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Session(ctx, mine); err != nil {
		t.Fatal("the changing session was signed out")
	}
	if _, err := a.Session(ctx, other); !errors.Is(err, ErrNoSession) {
		t.Fatal("another session survived a password change")
	}
	if ok, _ := a.CheckPassword(ctx, "second-password"); !ok {
		t.Fatal("new password doesn't work")
	}
}

func TestHandoffPIN(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdmin(t, openDB(t), "the-password")
	if ok, _ := a.CheckPIN(ctx, "1234"); ok {
		t.Fatal("a PIN matched before one was set")
	}
	if at, _ := a.PINChangedAt(ctx); !at.IsZero() {
		t.Fatal("PIN change time before one was set")
	}
	for _, bad := range []string{"123", "1234567", "12a4", " 1234", ""} {
		if err := a.SetPIN(ctx, bad); !errors.Is(err, ErrBadPIN) {
			t.Errorf("SetPIN(%q) = %v", bad, err)
		}
	}
	if err := a.SetPIN(ctx, "048213"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.CheckPIN(ctx, "048213"); !ok {
		t.Fatal("PIN doesn't verify")
	}
	if ok, _ := a.CheckPIN(ctx, "48213"); ok {
		t.Fatal("PIN without its leading zero verifies")
	}
	if at, _ := a.PINChangedAt(ctx); at.IsZero() {
		t.Fatal("no PIN change time")
	}
	var stored string
	_ = a.db.QueryRow(`SELECT value FROM settings WHERE key = 'handoff_pin_hash'`).Scan(&stored)
	if !strings.HasPrefix(stored, "$argon2id$") {
		t.Fatalf("PIN stored as %q", stored)
	}
}

func TestSignInReplacesTheOldSession(t *testing.T) {
	ctx := context.Background()
	a, _ := newAdmin(t, openDB(t), "the-password")
	old, _, err := a.SignIn(ctx, "the-password", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.SignIn(ctx, "nope", false, "", old); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := a.Session(ctx, old); err != nil {
		t.Fatal("a failed sign-in ended the existing session")
	}
	fresh, _, err := a.SignIn(ctx, "the-password", true, "", old)
	if err != nil || fresh == old {
		t.Fatalf("SignIn = %q, %v", fresh, err)
	}
	if _, err := a.Session(ctx, old); !errors.Is(err, ErrNoSession) {
		t.Fatal("the old session survived a new sign-in")
	}
}
