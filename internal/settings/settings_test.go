package settings

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d)
}

func TestDefaultsAndSave(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	got, err := s.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Settings{DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute}); got != want {
		t.Fatalf("defaults %+v, want %+v", got, want)
	}
	if p := got.NewProfile("fresh"); p != profiles.New("fresh") {
		t.Fatalf("the defaults change new profiles: %+v", p)
	}

	set := Settings{DefaultPolicy: profiles.PolicySignIn, DefaultAutoStop: 0, DefaultProxy: "socks5://u:p@proxy.example:1080", RAMLimitPercent: 50}
	if err := s.Save(ctx, set); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Get(ctx); err != nil || got != set {
		t.Fatalf("saved %+v, got %+v, %v", set, got, err)
	}
	p := got.NewProfile("work")
	if p.Name != "work" || p.HandoffPolicy != profiles.PolicySignIn || p.AutoStop != 0 || p.Proxy != set.DefaultProxy || p.Width != 1440 {
		t.Fatalf("NewProfile = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveRefusesBadSettings(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	good := Settings{DefaultPolicy: profiles.PolicyLink, DefaultAutoStop: 30 * time.Minute}
	if err := s.Save(ctx, good); err != nil {
		t.Fatal(err)
	}
	const secret = "hunter2-secret"
	for name, set := range map[string]Settings{
		"policy":    {DefaultPolicy: "nobody", DefaultAutoStop: 15 * time.Minute},
		"auto-stop": {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 7 * time.Minute},
		"ram":       {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, RAMLimitPercent: 101},
		"negative":  {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, RAMLimitPercent: -1},
		"scheme":    {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, DefaultProxy: "ftp://u:" + secret + "@corp-proxy.test:21"},
		"no port":   {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, DefaultProxy: "http://u:" + secret + "@corp-proxy.test"},
		"path":      {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, DefaultProxy: "http://u:" + secret + "@corp-proxy.test:80/x"},
		"garbage":   {DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, DefaultProxy: "http://u:" + secret + "@[::1"},
	} {
		err := s.Save(ctx, set)
		if err == nil {
			t.Errorf("%s: saved %+v", name, set)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "corp-proxy") {
			t.Errorf("%s: the error echoes the proxy: %v", name, err)
		}
	}
	if got, err := s.Get(ctx); err != nil || got != good {
		t.Fatalf("a refused save changed the settings to %+v, %v", got, err)
	}
}
