package profiles

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewStore(d)
}

func TestDefaultsMatchTheDesign(t *testing.T) {
	p := New("google")
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.DefaultMode != ModeVisual || p.AutoStop != 15*time.Minute || p.Screen() != "1440x900" || p.HandoffPolicy != PolicyPIN || p.Proxy != "" {
		t.Fatalf("defaults = %+v", p)
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"google", "x-com", "a", "0", "abc123", strings.Repeat("a", 32)} {
		if err := New(name).Validate(); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "Google", "-a", "a-", "a_b", "a.b", "../etc", "a b", "é", strings.Repeat("a", 33)} {
		if err := New(name).Validate(); err == nil {
			t.Errorf("%q: want an error", name)
		}
	}
}

func TestValidateSettings(t *testing.T) {
	bad := map[string]func(*Profile){
		"mode":         func(p *Profile) { p.DefaultMode = "cdp" },
		"auto-stop":    func(p *Profile) { p.AutoStop = 7 * time.Minute },
		"too narrow":   func(p *Profile) { p.Width = 639 },
		"too tall":     func(p *Profile) { p.Height = 4321 },
		"policy":       func(p *Profile) { p.HandoffPolicy = "none" },
		"proxy scheme": func(p *Profile) { p.Proxy = "ftp://proxy:21" },
		"proxy port":   func(p *Profile) { p.Proxy = "http://proxy" },
		"proxy path":   func(p *Profile) { p.Proxy = "http://proxy:8080/x" },
	}
	for name, mutate := range bad {
		p := New("google")
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	good := New("google")
	good.AutoStop = 0
	good.Proxy = "socks5://user:secret@proxy.example:1080"
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyErrorsNeverEchoTheURL(t *testing.T) {
	for _, proxy := range []string{"ftp://user:s3cret@proxy:21", "http://user:s3cret@proxy", "http://user:s3cret@proxy:1/x", "http://user:s3cret@[::1"} {
		p := New("google")
		p.Proxy = proxy
		err := p.Validate()
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("Validate(%q) = %v; want an error without the password", proxy, err)
		}
	}
}

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	created, err := s.Create(ctx, New("google"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, New("google")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create = %v, want ErrExists", err)
	}
	if _, err := s.Create(ctx, New("Bad Name")); err == nil {
		t.Fatal("Create accepted an invalid name")
	}

	got, err := s.Get(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	if got != created {
		t.Fatalf("Get = %+v, want %+v", got, created)
	}

	got.DefaultMode, got.AutoStop, got.Width, got.Height = ModeAPI, 0, 1920, 1080
	got.Proxy, got.HandoffPolicy = "http://u:p@proxy.example:8080", PolicySignIn
	updated, err := s.Update(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DefaultMode != ModeAPI || updated.AutoStop != 0 || updated.Screen() != "1920x1080" ||
		updated.Proxy != got.Proxy || updated.HandoffPolicy != PolicySignIn || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("after Update: %+v", updated)
	}
	if _, err := s.Update(ctx, New("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing = %v, want ErrNotFound", err)
	}

	if _, err := s.Create(ctx, New("github")); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "github" || list[1].Name != "google" {
		t.Fatalf("List = %v, %v", list, err)
	}

	if err := s.Delete(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "google"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "google"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}
