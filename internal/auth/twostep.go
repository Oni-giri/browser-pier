package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 with SHA-1 is what every authenticator app reads; HMAC-SHA-1 is not broken
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Two-step sign-in: after the password, a code from an authenticator app
// (TOTP, RFC 6238) or one of ten single-use recovery codes. Its state is
// in the database and read at every use, so `browserpier 2fa reset` works
// on a running server at once. The secret can't be hashed (the server
// computes codes from it), so it is stored as it is, like device_key.

// TOTP parameters: the ones every authenticator app reads.
const (
	totpPeriod    = 30 // seconds
	totpDigits    = 6
	totpSecretLen = 20 // bytes, as RFC 4226 recommends for SHA-1
	totpIssuer    = "BrowserPier"
)

// RecoveryCodes is how many recovery codes are made at a time.
const RecoveryCodes = 10

// Settings rows for the two-step state.
const (
	keyTOTPSecret  = "totp_secret"     // base32, no padding
	keyTOTPLast    = "totp_last_step"  // the last accepted 30-second step
	keyTOTPEnabled = "totp_enabled_at" // Unix milliseconds
)

var (
	// ErrNeedsCode: the password was right, and two-step sign-in is on.
	ErrNeedsCode = errors.New("a code from the authenticator app is needed")
	// ErrWrongCode: not a current, unused code from the app, nor an unused recovery code.
	ErrWrongCode = errors.New("that code isn't right")
	// ErrSignInAgain: since the password step, the password changed or
	// two-step sign-in was turned off; the password is asked again.
	ErrSignInAgain = errors.New("sign in again")
	ErrTwoStepOff  = errors.New("two-step sign-in is off")
	ErrTwoStepOn   = errors.New("two-step sign-in is already on")
	// ErrTwoStepUnreadable: the stored state is broken. Sign-ins are
	// refused rather than let through on the password alone.
	ErrTwoStepUnreadable = errors.New("the two-step sign-in state can't be read; reset it with `browserpier 2fa reset`")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// EncodeTOTPSecret is the secret as an authenticator app takes it typed in.
func EncodeTOTPSecret(secret []byte) string { return b32.EncodeToString(secret) }

// NewTOTPSecret draws a new 20-byte secret.
func NewTOTPSecret() []byte {
	b := make([]byte, totpSecretLen)
	_, _ = rand.Read(b)
	return b
}

// TOTPURL is the otpauth:// address an authenticator app reads from the
// QR code. host tells two BrowserPier instances apart in the app.
func TOTPURL(secret []byte, host string) string {
	label := strings.ReplaceAll(url.PathEscape(host), ":", "%3A")
	return "otpauth://totp/" + totpIssuer + ":" + label + "?secret=" + EncodeTOTPSecret(secret) + "&issuer=" + totpIssuer
}

// TOTPCode is the app's code for secret at t.
func TOTPCode(secret []byte, t time.Time) string {
	return hotp(secret, uint64(t.Unix()/totpPeriod)) //nolint:gosec // times before 1970 don't occur
}

// hotp is RFC 4226's HOTP with HMAC-SHA-1, cut to six digits.
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%uint32(math.Pow10(totpDigits)))
}

// matchTOTP checks code against the steps before, at and after now's,
// each in constant time and all three every time, and returns the newest
// matching step after last: a step at or before last was already used.
func matchTOTP(secret []byte, code string, now time.Time, last int64) (int64, bool) {
	if !isSixDigits(code) {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	found, ok := 0, 0
	for d := int64(-1); d <= 1; d++ {
		s := cur + d
		eq := subtle.ConstantTimeCompare([]byte(hotp(secret, uint64(s))), []byte(code)) //nolint:gosec // s > 0
		// last isn't secret: a plain comparison leaks nothing.
		fresh := 0
		if s > last {
			fresh = 1
		}
		hit := eq & fresh
		found = subtle.ConstantTimeSelect(hit, int(s), found)
		ok |= hit
	}
	return int64(found), ok == 1
}

func isSixDigits(s string) bool {
	if len(s) != totpDigits {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// recoveryAlphabet has no look-alikes: no 0 or O, no 1 or I. 32 symbols,
// so 16 of them carry 80 random bits.
const recoveryAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newRecoveryCode is 80 random bits, in four groups of four.
func newRecoveryCode() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	var sb strings.Builder
	var acc, bits, n uint
	for _, c := range b {
		acc = acc<<8 | uint(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			if n > 0 && n%4 == 0 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[(acc>>bits)&31])
			n++
		}
	}
	return sb.String()
}

type codeKind int

const (
	codeNone     codeKind = iota // neither shape: wrong whatever it is
	codeTOTP                     // exactly six digits
	codeRecovery                 // 16 symbols of the recovery alphabet
)

func (k codeKind) String() string {
	switch k {
	case codeTOTP:
		return "app"
	case codeRecovery:
		return "recovery"
	}
	return "none"
}

// classifyCode normalises what was typed (case, spaces, dashes) and says
// which kind of code it has the shape of.
func classifyCode(s string) (string, codeKind) {
	if len(s) > 64 {
		return "", codeNone
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '\t', ' ', '‐', '‑', '–':
			return -1
		}
		return r
	}, strings.ToUpper(s))
	if isSixDigits(s) {
		return s, codeTOTP
	}
	if len(s) == 16 && strings.Trim(s, recoveryAlphabet) == "" {
		return s, codeRecovery
	}
	return s, codeNone
}

// TwoStepStatus is what Settings and the command line show. It never
// holds the secret.
type TwoStepStatus struct {
	On        bool
	Since     time.Time // zero if unknown
	CodesLeft int
}

type twoStepState struct {
	on     bool
	secret []byte
	stored string // the secret's row as stored, to check it didn't change
	last   int64
	since  time.Time
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readTwoStep reads the state. Anything it can't make sense of is an
// error, never "off".
func readTwoStep(ctx context.Context, q querier) (twoStepState, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN (?, ?, ?)`, keyTOTPSecret, keyTOTPLast, keyTOTPEnabled)
	if err != nil {
		return twoStepState{}, err
	}
	defer func() { _ = rows.Close() }()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return twoStepState{}, err
		}
		kv[k] = v
	}
	if err := rows.Err(); err != nil {
		return twoStepState{}, err
	}
	stored, on := kv[keyTOTPSecret]
	if !on {
		return twoStepState{}, nil
	}
	st := twoStepState{on: true, stored: stored}
	if st.secret, err = b32.DecodeString(stored); err != nil || len(st.secret) != totpSecretLen {
		return twoStepState{}, ErrTwoStepUnreadable
	}
	if st.last, err = strconv.ParseInt(kv[keyTOTPLast], 10, 64); err != nil {
		return twoStepState{}, ErrTwoStepUnreadable
	}
	if ms, err := strconv.ParseInt(kv[keyTOTPEnabled], 10, 64); err == nil {
		st.since = time.UnixMilli(ms)
	}
	return st, nil
}

// ReadTwoStep reports the two-step state of the database at d.
func ReadTwoStep(ctx context.Context, d *sql.DB) (TwoStepStatus, error) {
	st, err := readTwoStep(ctx, d)
	if err != nil || !st.on {
		return TwoStepStatus{}, err
	}
	s := TwoStepStatus{On: true, Since: st.since}
	err = d.QueryRowContext(ctx, `SELECT count(*) FROM admin_recovery_codes`).Scan(&s.CodesLeft)
	return s, err
}

// ResetTwoStep turns two-step sign-in off: it removes the secret, the
// last step and every recovery code, whatever state they are in.
// Sessions and the password stay. It reports whether it was on.
func ResetTwoStep(ctx context.Context, d *sql.DB) (wasOn bool, codes int, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key = ?`, keyTOTPSecret).Scan(&n); err != nil {
		return false, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM admin_recovery_codes`).Scan(&codes); err != nil {
		return false, 0, err
	}
	if err := clearTwoStep(ctx, tx); err != nil {
		return false, 0, err
	}
	return n > 0, codes, tx.Commit()
}

func clearTwoStep(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key IN (?, ?, ?)`, keyTOTPSecret, keyTOTPLast, keyTOTPEnabled); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM admin_recovery_codes`)
	return err
}

// TwoStep reports whether two-step sign-in is on, read from the database.
func (a *Admin) TwoStep(ctx context.Context) (TwoStepStatus, error) { return ReadTwoStep(ctx, a.db) }

// useCode records the use of a code, inside tx: an app code's step with
// an update that only moves the last step forward (and only for the
// secret st was read with), a recovery code by deleting it. Either
// touches no row when another request got there first, and then the
// code is wrong.
func (a *Admin) useCode(ctx context.Context, tx *sql.Tx, st twoStepState, code string, kind codeKind) error {
	switch kind {
	case codeTOTP:
		step, ok := matchTOTP(st.secret, code, a.now(), st.last)
		if !ok {
			return ErrWrongCode
		}
		res, err := tx.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ? AND CAST(value AS INTEGER) < ?
			AND EXISTS (SELECT 1 FROM settings WHERE key = ? AND value = ?)`,
			strconv.FormatInt(step, 10), keyTOTPLast, step, keyTOTPSecret, st.stored)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrWrongCode
		}
		return nil
	case codeRecovery:
		res, err := tx.ExecContext(ctx, `DELETE FROM admin_recovery_codes WHERE hash = ?`, hashToken(code))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrWrongCode
		}
		return nil
	}
	return ErrWrongCode
}

// finishSignIn is the second step: with the proof from the password step
// and a right code, it records the code's use and makes the session in
// one transaction, so a code whose use can't be written opens nothing.
func (a *Admin) finishSignIn(ctx context.Context, proof, code string, kind codeKind, remember bool, userAgent, oldCookie string) (string, AdminSession, error) {
	a.cred.RLock()
	defer a.cred.RUnlock()
	st, err := readTwoStep(ctx, a.db)
	if err != nil {
		return "", AdminSession{}, err
	}
	if !st.on {
		return "", AdminSession{}, ErrSignInAgain
	}
	if kind == codeNone {
		return "", AdminSession{}, ErrWrongCode
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", AdminSession{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'admin_password_hash'`).Scan(&stored); err != nil {
		return "", AdminSession{}, err
	}
	if !hmac.Equal([]byte(hashToken(stored)), []byte(proof)) {
		return "", AdminSession{}, ErrSignInAgain
	}
	if err := a.useCode(ctx, tx, st, code, kind); err != nil {
		return "", AdminSession{}, err
	}
	if oldCookie != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash = ?`, hashToken(oldCookie)); err != nil {
			return "", AdminSession{}, err
		}
	}
	cookie, s, err := a.insertSession(ctx, tx, remember, userAgent)
	if err != nil {
		return "", AdminSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", AdminSession{}, err
	}
	return cookie, s, nil
}

// checkPasswordAndState checks the current password, then reads the
// two-step state, which must be on.
func (a *Admin) checkPasswordAndState(ctx context.Context, password string) (twoStepState, error) {
	ok, err := a.CheckPassword(ctx, password)
	if err != nil {
		return twoStepState{}, err
	}
	if !ok {
		return twoStepState{}, ErrWrongPassword
	}
	st, err := readTwoStep(ctx, a.db)
	if err != nil {
		return twoStepState{}, err
	}
	if !st.on {
		return twoStepState{}, ErrTwoStepOff
	}
	return st, nil
}

// withPasswordAndCode checks the password and uses a code (from the app
// or a recovery code), then runs then in the same transaction.
func (a *Admin) withPasswordAndCode(ctx context.Context, password, code string, then func(*sql.Tx) error) error {
	st, err := a.checkPasswordAndState(ctx, password)
	if err != nil {
		return err
	}
	n, kind := classifyCode(code)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := a.useCode(ctx, tx, st, n, kind); err != nil {
		return err
	}
	if then != nil {
		if err := then(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConfirmIdentity checks the password and uses a code, before the
// authenticator app is replaced.
func (a *Admin) ConfirmIdentity(ctx context.Context, password, code string) error {
	return a.withPasswordAndCode(ctx, password, code, nil)
}

// DisableTwoStep turns two-step sign-in off, given the password and a code.
func (a *Admin) DisableTwoStep(ctx context.Context, password, code string) error {
	return a.withPasswordAndCode(ctx, password, code, func(tx *sql.Tx) error { return clearTwoStep(ctx, tx) })
}

// NewRecoveryCodes replaces every recovery code with ten new ones, given
// the password and a code, and returns them: they are never shown again.
func (a *Admin) NewRecoveryCodes(ctx context.Context, password, code string) ([]string, error) {
	var codes []string
	err := a.withPasswordAndCode(ctx, password, code, func(tx *sql.Tx) error {
		var err error
		codes, err = a.storeRecoveryCodes(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func (a *Admin) storeRecoveryCodes(ctx context.Context, tx *sql.Tx) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_recovery_codes`); err != nil {
		return nil, err
	}
	codes := make([]string, 0, RecoveryCodes)
	now := a.now().UnixMilli()
	for len(codes) < RecoveryCodes {
		c := newRecoveryCode()
		n, _ := classifyCode(c)
		res, err := tx.ExecContext(ctx, `INSERT INTO admin_recovery_codes (hash, created_at) VALUES (?, ?) ON CONFLICT DO NOTHING`, hashToken(n), now)
		if err != nil {
			return nil, err
		}
		if k, _ := res.RowsAffected(); k == 1 { // a repeat of 80 random bits won't happen, but costs nothing to skip
			codes = append(codes, c)
		}
	}
	return codes, nil
}

// EnableTwoStep turns two-step sign-in on with secret, once code shows
// the app has it. The code's step counts as used. Every other browser is
// signed out and the device key rotates, in the same transaction. It
// returns the ten recovery codes, shown once.
func (a *Admin) EnableTwoStep(ctx context.Context, secret []byte, code, keepCookie string) ([]string, error) {
	var codes []string
	err := a.storeSecret(ctx, secret, code, keepCookie, false, func(tx *sql.Tx) error {
		var err error
		codes, err = a.storeRecoveryCodes(ctx, tx)
		return err
	})
	return codes, err
}

// ReplaceTwoStepApp moves two-step sign-in to a new secret (a new phone),
// once code shows the app has it. The recovery codes stay. As turning
// on, it signs out every other browser and rotates the device key. The
// caller has checked the password and a code first (ConfirmIdentity).
func (a *Admin) ReplaceTwoStepApp(ctx context.Context, secret []byte, code, keepCookie string) error {
	return a.storeSecret(ctx, secret, code, keepCookie, true, nil)
}

func (a *Admin) storeSecret(ctx context.Context, secret []byte, code, keepCookie string, replace bool, then func(*sql.Tx) error) error {
	if len(secret) != totpSecretLen {
		return errors.New("bad secret")
	}
	n, kind := classifyCode(code)
	if kind != codeTOTP {
		return ErrWrongCode
	}
	step, ok := matchTOTP(secret, n, a.now(), math.MinInt32)
	if !ok {
		return ErrWrongCode
	}
	// Held for writing, so no sign-in that read "off" makes a session
	// on the password alone after this commits.
	a.cred.Lock()
	defer a.cred.Unlock()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	st, err := readTwoStep(ctx, tx)
	switch {
	case err != nil:
		return err
	case !replace && st.on:
		return ErrTwoStepOn
	case replace && !st.on:
		return ErrTwoStepOff
	}
	rows := [][2]string{
		{keyTOTPSecret, EncodeTOTPSecret(secret)},
		{keyTOTPLast, strconv.FormatInt(step, 10)},
		{"device_key", randomString(32)},
	}
	if !replace {
		rows = append(rows, [2]string{keyTOTPEnabled, strconv.FormatInt(a.now().UnixMilli(), 10)})
	}
	for _, kv := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	if keepCookie == "" {
		keepCookie = "-" // matches no session
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE hash != ?`, hashToken(keepCookie)); err != nil {
		return err
	}
	if then != nil {
		if err := then(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}
