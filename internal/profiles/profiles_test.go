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
		"time zone":    func(p *Profile) { p.TimeZone = "Mars/Olympus" },
		"locale":       func(p *Profile) { p.Locale = "german" },
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
	long := strings.Repeat("s3cret", 50)
	for _, proxy := range []string{
		"ftp://user:s3cret@proxy:21", "http://user:s3cret@proxy", "http://user:s3cret@proxy:1/x", "http://user:s3cret@[::1",
		// Templates, refused before or after filling in.
		"ftp://n4me-{sticky}:s3cret@pr0xyhost:21", "http://n4me-{sticky}:s3cret@{sticky}:1", "http://n4me-{other}:s3cret@pr0xyhost:1",
		"http://n4me-{sticky}:s3cret@pr0xyhost:1/x", "http://n4me-{sticky}:" + long + "@pr0xyhost:1", "socks5://" + long + "{sticky}:pw@pr0xyhost:1",
		"http://n4%3Ame-{sticky}:s3cret@pr0xyhost:1", "http://n4me-{sticky}:s3cret{sticky}@[::1",
	} {
		p := New("google")
		p.Proxy = proxy
		p.StickyToken = "t0k3nabc"
		err := p.Validate()
		if err == nil {
			t.Errorf("Validate(%q) accepted it", proxy)
			continue
		}
		for _, leak := range []string{proxy, "s3cret", "n4me", "pr0xyhost", "{other}", "t0k3nabc", sampleSticky} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("Validate(%q) = %v; the error holds %q", proxy, err, leak)
			}
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

func TestOverridesAndLocation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := New("google")
	p.TimeZone, p.Locale = "Europe/Berlin", "de-DE"
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	when := time.UnixMilli(1_800_000_000_000)
	if err := s.SetLocation(ctx, "google", Location{Country: "DE", TimeZone: "Europe/Berlin", IP: "203.0.113.7", CheckedAt: when}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "google")
	if got.TimeZone != "Europe/Berlin" || got.Locale != "de-DE" || got.Location.Country != "DE" || got.Location.IP != "203.0.113.7" || !got.Location.CheckedAt.Equal(when) {
		t.Fatalf("got %+v", got)
	}
	got.TimeZone = ""
	if got, err := s.Update(ctx, got); err != nil || got.TimeZone != "" || got.Location.Country != "DE" {
		t.Fatalf("update kept the location? %+v, %v", got, err)
	}
}

func TestLocalNetworkIsOffUntilSet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Create(ctx, New("router")); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, "router")
	if p.LocalNetwork {
		t.Fatal("a new profile can reach the local network")
	}
	p.LocalNetwork = true
	if got, err := s.Update(ctx, p); err != nil || !got.LocalNetwork {
		t.Fatalf("update: %+v, %v", got, err)
	}
	if list, _ := s.List(ctx); len(list) != 1 || !list[0].LocalNetwork {
		t.Fatalf("list: %+v", list)
	}
}

func TestNewProfilesBlockAds(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Create(ctx, New("news")); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, "news")
	if !p.BlockAds {
		t.Fatal("a new profile doesn't block ads")
	}
	p.BlockAds = false
	if got, err := s.Update(ctx, p); err != nil || got.BlockAds {
		t.Fatalf("update: %+v, %v", got, err)
	}
	if list, _ := s.List(ctx); len(list) != 1 || list[0].BlockAds {
		t.Fatalf("list: %+v", list)
	}
}

func TestRecordSessionsIsOffUntilTheAdminSetsIt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if New("mail").Record {
		t.Fatal("a new profile records sessions")
	}
	if _, err := s.Create(ctx, New("mail")); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, "mail")
	if p.Record {
		t.Fatal("a stored new profile records sessions")
	}
	p.Record = true
	if got, err := s.Update(ctx, p); err != nil || !got.Record {
		t.Fatalf("update: %+v, %v", got, err)
	}
	if list, _ := s.List(ctx); len(list) != 1 || !list[0].Record {
		t.Fatalf("list: %+v", list)
	}
}
