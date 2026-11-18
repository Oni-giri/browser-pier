package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session lifetimes. A remembered device stays signed in for 30 days;
// otherwise the cookie lasts until the browser closes, and at most a day.
const (
	RememberFor = 30 * 24 * time.Hour
	SessionFor  = 24 * time.Hour
)

var (
	ErrWrongPassword  = errors.New("wrong password")
	ErrWrongSetupCode = errors.New("wrong setup code")
	ErrAlreadySetUp   = errors.New("the admin password is already set")
	ErrEnvManaged     = errors.New("the admin password is set by BROWSERPIER_ADMIN_PASSWORD; change it there")
	ErrNoSession      = errors.New("not signed in")
	ErrBadPIN         = errors.New("the PIN must be 4 to 6 digits")
)

const minPasswordLen = 8

// AdminSession is a signed-in browser.
type AdminSession struct {
	ID         string // the SHA-256 of its cookie, never the cookie
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	Remember   bool
	UserAgent  string
}

// Admin is the single admin account: its password, its signed-in browsers,
// the first-run setup code and the handoff PIN.
type Admin struct {
	db         *sql.DB
	log        *slog.Logger
	now        func() time.Time
	envManaged bool
	argon      chan struct{} // caps concurrent argon2 runs, ~20 MiB each

	mu        sync.Mutex
	setupCode string // set until a password exists

	// cred is held for reading across a sign-in's password check and
	// session creation, and for writing while the password changes, so no
	// session is ever created on a password that was just replaced.
	cred sync.RWMutex
}

// NewAdmin loads the admin account. A password from the environment wins
// at every start, so changing it there is also how to reset a forgotten
// one. Without any password, it makes a one-time setup code and logs it:
// the setup page asks for it, so whoever first reaches a fresh public
// instance can't claim it.
func NewAdmin(ctx context.Context, db *sql.DB, envPassword string, log *slog.Logger) (*Admin, error) {
	a := &Admin{db: db, log: log, now: time.Now, argon: make(chan struct{}, 2)}
	stored, err := a.setting(ctx, "admin_password_hash")
	if err != nil {
		return nil, err
	}
	switch {
	case envPassword != "":
		a.envManaged = true
		if stored == "" || !VerifySecret(stored, envPassword) {
			// A changed password is how a lost or leaked one is reset, so
			// every browser signed in with the old one is signed out.
			if err := a.storePassword(ctx, envPassword, ""); err != nil {
				return nil, err
			}
			log.Info("admin password set from BROWSERPIER_ADMIN_PASSWORD; signed out every browser")
		}
	case stored == "":
		a.setupCode = randomString(16) // 128 bits
		log.Warn("no admin password yet: open /setup in a browser and enter this setup code", "setup_code", a.setupCode)
	}
	return a, nil
}

// EnvManaged reports whether the password comes from BROWSERPIER_ADMIN_PASSWORD.
func (a *Admin) EnvManaged() bool { return a.envManaged }

// NeedsSetup reports whether no admin password is set yet.
func (a *Admin) NeedsSetup() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setupCode != ""
}

// Setup sets the first admin password, given the setup code from the log.
func (a *Admin) Setup(ctx context.Context, code, password string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.setupCode == "" {
		return ErrAlreadySetUp
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(a.setupCode)) != 1 {
		return ErrWrongSetupCode
	}
	if err := checkPassword(password); err != nil {
		return err
	}
	a.cred.Lock()
	defer a.cred.Unlock()
	if err := a.storePassword(ctx, password, ""); err != nil {
		return err
	}
	a.setupCode = ""
	a.log.Info("admin password set on the setup page")
	return nil
}

func checkPassword(p string) error {
	if len(p) < minPasswordLen {
		return errors.New("use at least " + strconv.Itoa(minPasswordLen) + " characters")
	}
	if len(p) > 1024 {
		return errors.New("use at most 1024 characters")
	}
	return nil
}

// CheckPassword reports whether password is the admin password.
func (a *Admin) CheckPassword(ctx context.Context, password string) (bool, error) {
	stored, err := a.setting(ctx, "admin_password_hash")
	if err != nil || stored == "" {
		return false, err
	}
	return a.verify(ctx, stored, password)
}

// ChangePassword changes the password and signs out every other browser.
func (a *Admin) ChangePassword(ctx context.Context, current, next, keepCookie string) error {
	if a.envManaged {
		return ErrEnvManaged
	}
	if err := checkPassword(next); err != nil {
		return err
	}
	a.cred.Lock()
	defer a.cred.Unlock()
	ok, err := a.CheckPassword(ctx, current)
	if err != nil {
		return err
	}
	if !ok {
		return ErrWrongPassword
	}
	if keepCookie == "" {
		keepCookie = "-" // matches no session
	}
	return a.storePassword(ctx, next, keepCookie)
}

// storePassword sets the password, rotates the device key and signs out
// every browser but keepCookie's ("" signs out all), in one transaction:
// a failure part way never leaves a new password with old sessions or
// device cookies still trusted. The caller holds a.cred for writing.
func (a *Admin) storePassword(ctx context.Context, password, keepCookie string) error {
	h, err := HashSecret(password)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{{"admin_password_hash", h}, {"device_key", randomString(32)}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash != ?`, hashToken(keepCookie)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeviceTTL is how long a device that signed in stays known.
const DeviceTTL = 365 * 24 * time.Hour

// NewDeviceToken marks a browser that just signed in as known. Known
// devices skip the global sign-in throttle, so an attack from elsewhere
// never slows the owner's usual browsers (OWASP's "device cookie").
func (a *Admin) NewDeviceToken(ctx context.Context) (string, error) {
	key, err := a.setting(ctx, "device_key")
	if err != nil {
		return "", err
	}
	id := randomString(16) + "." + strconv.FormatInt(a.now().Add(DeviceTTL).Unix(), 10)
	return id + "." + deviceMAC(key, id), nil
}

// KnownDevice reports whether v came from NewDeviceToken, under the
// current password, and hasn't expired.
func (a *Admin) KnownDevice(ctx context.Context, v string) bool {
	i := strings.LastIndexByte(v, '.')
	if i < 0 {
		return false
	}
	id, mac := v[:i], v[i+1:]
	_, exp, ok := strings.Cut(id, ".")
	if !ok {
		return false
	}
	until, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || a.now().Unix() >= until {
		return false
	}
	key, err := a.setting(ctx, "device_key")
	if err != nil || key == "" {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(deviceMAC(key, id)))
}

func deviceMAC(key, id string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil))
}

// verify runs argon2 with at most two in flight, so a flood of logins
// can't exhaust a small server's memory.
func (a *Admin) verify(ctx context.Context, hash, secret string) (bool, error) {
	select {
	case a.argon <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-a.argon }()
	return VerifySecret(hash, secret), nil
}

// SignIn checks the password and, if it is right, replaces the browser's
// old session (oldCookie, if any) with a new one, in one transaction. It
// returns ErrWrongPassword for a wrong password, and ErrNeedsCode, with
// no session, when two-step sign-in is on.
func (a *Admin) SignIn(ctx context.Context, password string, remember bool, userAgent, oldCookie string) (string, AdminSession, error) {
	cookie, s, _, err := a.signIn(ctx, password, remember, userAgent, oldCookie)
	return cookie, s, err
}

// signIn is SignIn, also returning, with ErrNeedsCode, a proof of the
// password step for finishSignIn: the SHA-256 of the stored password
// hash, so a password changed in between voids it. The two-step state is
// read only after a right password, so a wrong one never tells whether
// it is on, and a state that can't be read refuses the sign-in.
func (a *Admin) signIn(ctx context.Context, password string, remember bool, userAgent, oldCookie string) (string, AdminSession, string, error) {
	a.cred.RLock()
	defer a.cred.RUnlock()
	stored, err := a.setting(ctx, "admin_password_hash")
	if err != nil {
		return "", AdminSession{}, "", err
	}
	if stored == "" {
		return "", AdminSession{}, "", ErrWrongPassword
	}
	ok, err := a.verify(ctx, stored, password)
	if err != nil {
		return "", AdminSession{}, "", err
	}
	if !ok {
		return "", AdminSession{}, "", ErrWrongPassword
	}
	st, err := readTwoStep(ctx, a.db)
	if err != nil {
		return "", AdminSession{}, "", err
	}
	if st.on {
		return "", AdminSession{}, hashToken(stored), ErrNeedsCode
	}
	cookie, s, err := a.replaceSession(ctx, remember, userAgent, oldCookie)
	return cookie, s, "", err
}

// replaceSession ends oldCookie's session, if any, and starts a new one,
// in one transaction.
func (a *Admin) replaceSession(ctx context.Context, remember bool, userAgent, oldCookie string) (string, AdminSession, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", AdminSession{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if oldCookie != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash = ?`, hashToken(oldCookie)); err != nil {
			return "", AdminSession{}, err
		}
	}
	cookie, s, err := a.insertSession(ctx, tx, remember, userAgent)
	if err != nil {
		return "", AdminSession{}, err
	}
	return cookie, s, tx.Commit()
}

// NewSession signs a browser in and returns its cookie value. Each login
// gets a fresh value, so a session can't be fixed in advance.
func (a *Admin) NewSession(ctx context.Context, remember bool, userAgent string) (string, AdminSession, error) {
	return a.insertSession(ctx, a.db, remember, userAgent)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (a *Admin) insertSession(ctx context.Context, db execer, remember bool, userAgent string) (string, AdminSession, error) {
	cookie := randomString(32)
	now := a.now().Truncate(time.Millisecond)
	life := SessionFor
	if remember {
		life = RememberFor
	}
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	s := AdminSession{ID: hashToken(cookie), CreatedAt: now, ExpiresAt: now.Add(life), LastSeenAt: now, Remember: remember, UserAgent: userAgent}
	_, err := db.ExecContext(ctx, `INSERT INTO admin_sessions (hash, created_at, expires_at, last_seen_at, remember, user_agent) VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.CreatedAt.UnixMilli(), s.ExpiresAt.UnixMilli(), s.LastSeenAt.UnixMilli(), s.Remember, s.UserAgent)
	if err != nil {
		return "", AdminSession{}, err
	}
	return cookie, s, nil
}

// Session returns the signed-in session for a cookie value.
func (a *Admin) Session(ctx context.Context, cookie string) (AdminSession, error) {
	if cookie == "" {
		return AdminSession{}, ErrNoSession
	}
	var s AdminSession
	var created, expires, seen int64
	err := a.db.QueryRowContext(ctx, `SELECT hash, created_at, expires_at, last_seen_at, remember, user_agent FROM admin_sessions WHERE hash = ?`,
		hashToken(cookie)).Scan(&s.ID, &created, &expires, &seen, &s.Remember, &s.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminSession{}, ErrNoSession
	}
	if err != nil {
		return AdminSession{}, err
	}
	s.CreatedAt, s.ExpiresAt, s.LastSeenAt = time.UnixMilli(created), time.UnixMilli(expires), time.UnixMilli(seen)
	now := a.now()
	if !now.Before(s.ExpiresAt) {
		_, _ = a.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash = ?`, s.ID)
		return AdminSession{}, ErrNoSession
	}
	if now.Sub(s.LastSeenAt) >= lastUsedEvery {
		s.LastSeenAt = now.Truncate(time.Millisecond)
		if _, err := a.db.ExecContext(ctx, `UPDATE admin_sessions SET last_seen_at = ? WHERE hash = ?`, s.LastSeenAt.UnixMilli(), s.ID); err != nil {
			return AdminSession{}, err
		}
	}
	return s, nil
}

// EndSession signs a browser out.
func (a *Admin) EndSession(ctx context.Context, cookie string) error {
	_, err := a.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash = ?`, hashToken(cookie))
	return err
}

// EndOtherSessions signs out every browser but the one with keepCookie.
func (a *Admin) EndOtherSessions(ctx context.Context, keepCookie string) error {
	_, err := a.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash != ?`, hashToken(keepCookie))
	return err
}

// Sessions lists the signed-in browsers, most recently seen first.
func (a *Admin) Sessions(ctx context.Context) ([]AdminSession, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT hash, created_at, expires_at, last_seen_at, remember, user_agent FROM admin_sessions
		WHERE expires_at > ? ORDER BY last_seen_at DESC`, a.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdminSession
	for rows.Next() {
		var s AdminSession
		var created, expires, seen int64
		if err := rows.Scan(&s.ID, &created, &expires, &seen, &s.Remember, &s.UserAgent); err != nil {
			return nil, err
		}
		s.CreatedAt, s.ExpiresAt, s.LastSeenAt = time.UnixMilli(created), time.UnixMilli(expires), time.UnixMilli(seen)
		out = append(out, s)
	}
	return out, rows.Err()
}

var pinRE = regexp.MustCompile(`^[0-9]{4,6}$`)

// SetPIN sets the handoff PIN.
func (a *Admin) SetPIN(ctx context.Context, pin string) error {
	if !pinRE.MatchString(pin) {
		return ErrBadPIN
	}
	h, err := HashSecret(pin)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{{"handoff_pin_hash", h}, {"handoff_pin_changed_at", strconv.FormatInt(a.now().UnixMilli(), 10)}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PINChangedAt returns when the handoff PIN was last set; zero if it never was.
func (a *Admin) PINChangedAt(ctx context.Context) (time.Time, error) {
	v, err := a.setting(ctx, "handoff_pin_changed_at")
	if err != nil || v == "" {
		return time.Time{}, err
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(ms), nil
}

// CheckPIN reports whether pin is the handoff PIN. With no PIN set,
// nothing matches. A 4–6 digit PIN can be brute-forced offline from its
// hash in seconds; the attempt limits are what protect it.
func (a *Admin) CheckPIN(ctx context.Context, pin string) (bool, error) {
	stored, err := a.setting(ctx, "handoff_pin_hash")
	if err != nil || stored == "" || !pinRE.MatchString(pin) {
		return false, err
	}
	return a.verify(ctx, stored, pin)
}

func (a *Admin) setting(ctx context.Context, key string) (string, error) {
	var v string
	err := a.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
