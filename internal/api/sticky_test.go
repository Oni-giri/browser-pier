package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/settings"
)

// TestStickyTemplateShownNeverFilled: agents see a {sticky} proxy as its
// template with the password redacted, never the profile's token; a
// profile an agent makes under a {sticky} default gets a token of its
// own; and no agent route can change one.
func TestStickyTemplateShownNeverFilled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, _ := f.profiles.Get(ctx, "google")
	p.Proxy = "http://pier-{sticky}:pw-{sticky}-s3cret@proxy.example:8080"
	if _, err := f.profiles.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	tok, err := f.profiles.StickyToken(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}

	var raw json.RawMessage
	if code := f.call(f.all, http.MethodGet, "/api/profiles", "", &raw); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), "s3cret") {
		t.Fatalf("the reply holds the token or the password: %s", raw)
	}
	var list struct{ Profiles []profileJSON }
	_ = json.Unmarshal(raw, &list)
	shown := ""
	for _, pr := range list.Profiles {
		if pr.Name == "google" {
			shown = pr.Proxy
		}
	}
	if shown != "http://pier-{sticky}:xxxxx@proxy.example:8080" {
		t.Fatalf("proxy shown as %q", shown)
	}

	// A start fills the template in for the browser only.
	s := f.start(f.all, "google", "api")
	for _, path := range []string{"/api/profiles", "/api/sessions", "/api/sessions/" + s.ID} {
		raw = nil
		f.call(f.all, http.MethodGet, path, "", &raw)
		if strings.Contains(string(raw), tok) {
			t.Errorf("GET %s holds the token: %s", path, raw)
		}
	}
	f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", nil)

	// Under a {sticky} default, an agent's new profile gets a token of
	// its own; it still can't choose a proxy.
	st := settings.New(f.db)
	const template = "socks5://pier-{sticky}:pw@proxy.example:1080"
	if err := st.Save(ctx, settings.Settings{DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, DefaultProxy: template}); err != nil {
		t.Fatal(err)
	}
	f.api.SetSettings(st)
	raw = nil
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "shop"}`, &raw); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	shop, err := f.profiles.Get(ctx, "shop")
	if err != nil || shop.Proxy != template || shop.StickyToken == "" || shop.StickyToken == tok {
		t.Fatalf("the agent's profile: proxy %q, token %q (google's %q), %v", shop.Proxy, shop.StickyToken, tok, err)
	}
	if strings.Contains(string(raw), shop.StickyToken) || !strings.Contains(string(raw), "pier-{sticky}") {
		t.Fatalf("create replied %s", raw)
	}
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "y", "proxy": "http://pier-{sticky}:pw@proxy.example:8080"}`, nil); code != http.StatusBadRequest {
		t.Errorf("a proxy through the API: %d, want 400", code)
	}
	// sticky_token isn't a field an agent can send: the request is
	// refused and no profile is made.
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "z", "sticky_token": "abcd1234"}`, nil); code != http.StatusBadRequest {
		t.Errorf("a sticky_token through the API: %d, want 400", code)
	}
	if _, err := f.profiles.Get(ctx, "z"); !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("a profile was made with an agent's token: %v", err)
	}

	// No agent route is about the exit or the token.
	routes := routeRecorder{}
	f.api.Register(routes)
	for pattern := range routes {
		if l := strings.ToLower(pattern); strings.Contains(l, "exit") || strings.Contains(l, "sticky") {
			t.Errorf("agent route %s", pattern)
		}
	}
}
