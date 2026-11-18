package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
)

func TestTwoFACommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BROWSERPIER_DATA_DIR", dir)
	ctx := context.Background()
	var out bytes.Buffer

	// Off: says so, and succeeds.
	if err := twoFACommand([]string{"status"}, &out); err != nil || !strings.Contains(out.String(), "off") {
		t.Fatalf("status when off = %v, %q", err, out.String())
	}
	out.Reset()
	if err := twoFACommand([]string{"reset"}, &out); err != nil || !strings.Contains(out.String(), "already off") {
		t.Fatalf("reset when off = %v, %q", err, out.String())
	}

	// A running server: its Admin is loaded on the same database.
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	secret := auth.NewTOTPSecret()
	codes, err := admin.EnableTwoStep(ctx, secret, auth.TOTPCode(secret, time.Now()), "")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := admin.NewSession(ctx, true, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := admin.SignIn(ctx, "the-admin-password", false, "", ""); !errors.Is(err, auth.ErrNeedsCode) {
		t.Fatalf("the password alone while on: %v", err)
	}

	out.Reset()
	if err := twoFACommand([]string{"status"}, &out); err != nil || !strings.Contains(out.String(), "on since") || !strings.Contains(out.String(), "10 recovery codes left") {
		t.Fatalf("status when on = %v, %q", err, out.String())
	}
	for _, s := range append(codes, auth.EncodeTOTPSecret(secret), auth.EncodeTOTPSecret(secret)[:8]) {
		if strings.Contains(out.String(), s) {
			t.Fatalf("status printed a secret: %q", out.String())
		}
	}

	out.Reset()
	if err := twoFACommand([]string{"reset"}, &out); err != nil || !strings.Contains(out.String(), "Two-step sign-in is off") || !strings.Contains(out.String(), "10 recovery codes") {
		t.Fatalf("reset = %v, %q", err, out.String())
	}
	if st, err := auth.ReadTwoStep(ctx, d); err != nil || st.On || st.CodesLeft != 0 {
		t.Fatalf("after reset: %+v, %v", st, err)
	}
	// Sessions and the password stay; the running server signs in on the
	// password alone at once.
	if _, err := admin.Session(ctx, cookie); err != nil {
		t.Fatal("reset signed a browser out")
	}
	if _, _, err := admin.SignIn(ctx, "the-admin-password", false, "", ""); err != nil {
		t.Fatalf("the password alone after reset, no restart: %v", err)
	}

	for _, bad := range [][]string{nil, {"nope"}, {"reset", "now"}} {
		if err := twoFACommand(bad, &out); err == nil {
			t.Errorf("%v: want usage", bad)
		}
	}
}
