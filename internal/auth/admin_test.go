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

func TestTurningTwoStepOn(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, logs := newAdmin(t, d, "the-password")
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }
	mine, _, _ := a.NewSession(ctx, false, "")
	other, _, _ := a.NewSession(ctx, true, "")
	dev, _ := a.NewDeviceToken(ctx)
	secret := NewTOTPSecret()

	// A wrong confirming code stores nothing.
	if _, err := a.EnableTwoStep(ctx, secret, "123456", mine); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("wrong confirming code: %v", err)
	}
	if st, _ := a.TwoStep(ctx); st.On {
		t.Fatal("on after a wrong confirming code")
	}
	if _, _, err := a.SignIn(ctx, "the-password", false, "", ""); err != nil {
		t.Fatalf("the password alone after a wrong confirming code: %v", err)
	}

	code := TOTPCode(secret, now)
	codes, err := a.EnableTwoStep(ctx, secret, code, mine)
	if err != nil || len(codes) != RecoveryCodes {
		t.Fatalf("EnableTwoStep = %d codes, %v", len(codes), err)
	}
	st, err := a.TwoStep(ctx)
	if err != nil || !st.On || !st.Since.Equal(now) || st.CodesLeft != RecoveryCodes {
		t.Fatalf("TwoStep = %+v, %v", st, err)
	}
	if _, err := a.Session(ctx, mine); err != nil {
		t.Fatal("this browser was signed out")
	}
	if _, err := a.Session(ctx, other); !errors.Is(err, ErrNoSession) {
		t.Fatal("another browser stayed signed in")
	}
	if a.KnownDevice(ctx, dev) {
		t.Fatal("a device cookie from before is still known")
	}
	// The password alone makes no session now, and the confirming code
	// can't sign in.
	if _, _, err := a.SignIn(ctx, "the-password", false, "", ""); !errors.Is(err, ErrNeedsCode) {
		t.Fatalf("the password alone: %v", err)
	}
	_, _, proof, _ := a.signIn(ctx, "the-password", false, "", "")
	if _, _, err := a.finishSignIn(ctx, proof, code, codeTOTP, false, "", ""); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("the confirming code signed in: %v", err)
	}
	if _, err := a.EnableTwoStep(ctx, NewTOTPSecret(), code, mine); !errors.Is(err, ErrTwoStepOn) && !errors.Is(err, ErrWrongCode) {
		t.Fatalf("turning on twice: %v", err)
	}
	if strings.Contains(logs.String(), EncodeTOTPSecret(secret)) || strings.Contains(logs.String(), code) {
		t.Fatal("the secret or a code was logged")
	}
}

func TestChangingTwoStepNeedsPasswordAndCode(t *testing.T) {
	ctx := context.Background()
	a, d, now, secret, codes := twoStepAdmin(t)
	code := func() string { *now = now.Add(30 * time.Second); return TOTPCode(secret, *now) }
	actions := map[string]func(password, code string) error{
		"turn off": func(p, c string) error { return a.DisableTwoStep(ctx, p, c) },
		"replace":  func(p, c string) error { return a.ConfirmIdentity(ctx, p, c) },
		"new codes": func(p, c string) error {
			_, err := a.NewRecoveryCodes(ctx, p, c)
			return err
		},
	}
	for name, do := range actions {
		if err := do("the-password", ""); !errors.Is(err, ErrWrongCode) {
			t.Errorf("%s with the password alone: %v", name, err)
		}
		if err := do("", code()); !errors.Is(err, ErrWrongPassword) {
			t.Errorf("%s with a code alone: %v", name, err)
		}
		if err := do("wrong-password", code()); !errors.Is(err, ErrWrongPassword) {
			t.Errorf("%s with a wrong password: %v", name, err)
		}
	}

	// New recovery codes make the old ones useless.
	fresh, err := a.NewRecoveryCodes(ctx, "the-password", code())
	if err != nil || len(fresh) != RecoveryCodes {
		t.Fatalf("new codes: %d, %v", len(fresh), err)
	}
	if err := a.ConfirmIdentity(ctx, "the-password", codes[0]); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("an old recovery code after new ones: %v", err)
	}
	// Replacing: the password and a recovery code, then the new app.
	if err := a.ConfirmIdentity(ctx, "the-password", fresh[0]); err != nil {
		t.Fatal(err)
	}
	next := NewTOTPSecret()
	*now = now.Add(30 * time.Second)
	if err := a.ReplaceTwoStepApp(ctx, next, TOTPCode(secret, *now), ""); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("replacing with the old app's code: %v", err)
	}
	if err := a.ReplaceTwoStepApp(ctx, next, TOTPCode(next, *now), ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.TwoStep(ctx); !st.On || st.CodesLeft != RecoveryCodes-1 {
		t.Fatalf("after replacing: %+v", st)
	}
	secret = next
	// Turning off with both.
	if err := a.DisableTwoStep(ctx, "the-password", code()); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = d.QueryRow(`SELECT (SELECT count(*) FROM settings WHERE key LIKE 'totp_%') + (SELECT count(*) FROM admin_recovery_codes)`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d two-step rows left after turning off", rows)
	}
	if _, _, err := a.SignIn(ctx, "the-password", false, "", ""); err != nil {
		t.Fatalf("the password alone after turning off: %v", err)
	}
}

func TestEnvPasswordDoesNotTurnTwoStepOff(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, _ := newAdmin(t, d, "from-env-1")
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }
	secret := NewTOTPSecret()
	if _, err := a.EnableTwoStep(ctx, secret, TOTPCode(secret, now), ""); err != nil {
		t.Fatal(err)
	}
	cookie, _, _ := a.NewSession(ctx, true, "")
	a, _ = newAdmin(t, d, "from-env-2")
	if _, err := a.Session(ctx, cookie); !errors.Is(err, ErrNoSession) {
		t.Fatal("sessions survived a changed environment password")
	}
	if ok, _ := a.CheckPassword(ctx, "from-env-2"); !ok {
		t.Fatal("the new password doesn't work")
	}
	var stored string
	_ = d.QueryRow(`SELECT value FROM settings WHERE key = ?`, keyTOTPSecret).Scan(&stored)
	if st, _ := a.TwoStep(ctx); !st.On || stored != EncodeTOTPSecret(secret) {
		t.Fatalf("two-step after a changed environment password: %+v", st)
	}
	if _, _, err := a.SignIn(ctx, "from-env-2", false, "", ""); !errors.Is(err, ErrNeedsCode) {
		t.Fatalf("the new password alone: %v", err)
	}
}

func TestChangePasswordLeavesTwoStepOn(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	a, _ := newAdmin(t, d, "")
	if err := a.Setup(ctx, a.setupCode, "first-password"); err != nil {
		t.Fatal(err)
	}
	secret := NewTOTPSecret()
	if _, err := a.EnableTwoStep(ctx, secret, TOTPCode(secret, time.Now()), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.ChangePassword(ctx, "first-password", "second-password", ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.TwoStep(ctx); !st.On {
		t.Fatal("changing the password turned two-step off")
	}
	if _, _, err := a.SignIn(ctx, "second-password", false, "", ""); !errors.Is(err, ErrNeedsCode) {
		t.Fatalf("the new password alone: %v", err)
	}
}

// A password changed between the password step and the code voids the step.
func TestPasswordChangeVoidsAPendingStep(t *testing.T) {
	ctx := context.Background()
	a, _, now, secret, _ := twoStepAdmin(t)
	_, _, proof, err := a.signIn(ctx, "the-password", false, "", "")
	if !errors.Is(err, ErrNeedsCode) {
		t.Fatal(err)
	}
	a.cred.Lock()
	err = a.storePassword(ctx, "another-password", "") // as a changed BROWSERPIER_ADMIN_PASSWORD does
	a.cred.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.finishSignIn(ctx, proof, TOTPCode(secret, *now), codeTOTP, false, "", ""); !errors.Is(err, ErrSignInAgain) {
		t.Fatalf("a step from before a password change: %v", err)
	}
}
