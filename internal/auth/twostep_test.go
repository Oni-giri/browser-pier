package auth

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var rfcSecret = []byte("12345678901234567890")

// RFC 6238 Appendix B, SHA-1, cut to six digits.
func TestTOTPMatchesRFC6238(t *testing.T) {
	for unix, want := range map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	} {
		if got := TOTPCode(rfcSecret, time.Unix(unix, 0)); got != want {
			t.Errorf("TOTP at %d = %s, want %s", unix, got, want)
		}
	}
}

func TestTOTPSecretAndURL(t *testing.T) {
	if got := EncodeTOTPSecret(rfcSecret); got != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatalf("base32 = %s", got)
	}
	if got := TOTPURL(rfcSecret, "pier.example.com"); got != "otpauth://totp/BrowserPier:pier.example.com?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=BrowserPier" {
		t.Fatalf("otpauth = %s", got)
	}
	// A port stays in the label, its colon escaped so apps split the
	// issuer from the account at the first one.
	if got := TOTPURL(rfcSecret, "100.64.0.7:7436"); got != "otpauth://totp/BrowserPier:100.64.0.7%3A7436?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=BrowserPier" {
		t.Fatalf("otpauth with a port = %s", got)
	}
	if s := NewTOTPSecret(); len(s) != 20 || len(EncodeTOTPSecret(s)) != 32 {
		t.Fatalf("new secret %d bytes", len(s))
	}
}

func TestTOTPWindowAndShape(t *testing.T) {
	now := time.Unix(1_800_000_015, 0)
	step := now.Unix() / 30
	at := func(d int64) string { return TOTPCode(rfcSecret, time.Unix((step+d)*30, 0)) }
	for d := int64(-1); d <= 1; d++ {
		got, ok := matchTOTP(rfcSecret, at(d), now, math.MinInt32)
		if !ok || got != step+d {
			t.Errorf("code of step %+d: %d, %v", d, got, ok)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, ok := matchTOTP(rfcSecret, at(d), now, math.MinInt32); ok {
			t.Errorf("code two steps away (%+d) accepted", d)
		}
	}
	good := at(0)
	for _, bad := range []string{good[:5], good + "1", "abcdef", "", "12345a", " " + good[:5]} {
		if _, ok := matchTOTP(rfcSecret, bad, now, math.MinInt32); ok {
			t.Errorf("%q accepted", bad)
		}
		if _, kind := classifyCode(bad); kind == codeTOTP {
			t.Errorf("%q has the shape of an app code", bad)
		}
	}
}

func TestTOTPReplayRefused(t *testing.T) {
	now := time.Unix(1_800_000_015, 0)
	n := now.Unix() / 30
	at := func(s int64) string { return TOTPCode(rfcSecret, time.Unix(s*30, 0)) }
	// Step n was used: n and n-1 are refused, n+1 still works.
	for _, s := range []int64{n, n - 1} {
		if _, ok := matchTOTP(rfcSecret, at(s), now, n); ok {
			t.Errorf("step %d accepted after step %d was used", s-n, 0)
		}
	}
	if got, ok := matchTOTP(rfcSecret, at(n+1), now, n); !ok || got != n+1 {
		t.Fatalf("step n+1 after n: %d, %v", got, ok)
	}
}

// twoStepAdmin is an admin with a fake clock and two-step on.
func twoStepAdmin(t *testing.T) (*Admin, *sql.DB, *time.Time, []byte, []string) {
	t.Helper()
	d := openDB(t)
	a, _ := newAdmin(t, d, "the-password")
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }
	secret := NewTOTPSecret()
	codes, _, err := a.EnableTwoStep(context.Background(), secret, TOTPCode(secret, now), "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	return a, d, &now, secret, codes
}

func TestRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	a, d, _, _, codes := twoStepAdmin(t)
	if len(codes) != RecoveryCodes {
		t.Fatalf("%d codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] || len(c) != 19 || strings.Count(c, "-") != 3 || strings.ContainsAny(c, "01IO") {
			t.Fatalf("code %q: want 4 groups of 4, distinct, no look-alikes", c)
		}
		seen[c] = true
	}
	// Only hashes are stored.
	var all strings.Builder
	rows, err := d.Query(`SELECT hash FROM admin_recovery_codes UNION ALL SELECT value FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		all.WriteString(v + "\n")
	}
	_ = rows.Close()
	for _, c := range codes {
		n, _ := classifyCode(c)
		if strings.Contains(strings.ToUpper(all.String()), n) || strings.Contains(all.String(), c) {
			t.Fatalf("recovery code %s is stored as is", c)
		}
	}

	left := func() int {
		s, err := a.TwoStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s.CodesLeft
	}
	use := func(code string) error {
		st, err := readTwoStep(ctx, d) // before the transaction: the database has one connection
		if err != nil {
			t.Fatal(err)
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		n, kind := classifyCode(code)
		if err := a.useCode(ctx, tx, st, n, kind); err != nil {
			return err
		}
		return tx.Commit()
	}
	// A wrong one leaves the ten untouched.
	if err := use("2345-6789-ABCD-EFGH"); !errors.Is(err, ErrWrongCode) || left() != 10 {
		t.Fatalf("wrong recovery code: %v, %d left", err, left())
	}
	// Dashes, case and spaces don't matter.
	spaced := strings.ToLower(strings.ReplaceAll(codes[0], "-", " "))
	if err := use(spaced); err != nil {
		t.Fatalf("lower case with spaces: %v", err)
	}
	if left() != 9 {
		t.Fatalf("%d left after one use", left())
	}
	if err := use(codes[0]); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("a recovery code worked twice: %v", err)
	}
	if err := use(strings.ReplaceAll(codes[1], "-", "")); err != nil {
		t.Fatalf("without dashes: %v", err)
	}
	if err := use(codes[2]); err != nil || left() != 7 {
		t.Fatalf("as shown: %v, %d left", err, left())
	}
}
