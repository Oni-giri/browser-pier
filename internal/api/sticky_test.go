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

// TestStickyTemplateNeverShown: agents see neither a {sticky} proxy's
// template nor the profile's token, only that the profile is proxied
// (PROJ-205); a profile an agent makes under a {sticky} default gets a
// token of its own; and no agent route can change one.
func TestStickyTemplateNeverShown(t *testing.T) {
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
	for _, leak := range []string{tok, "s3cret", "{sticky}", "%7Bsticky%7D", "pier-", "proxy.example", `"proxy"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the reply holds %q: %s", leak, raw)
		}
	}
	var list struct{ Profiles []profileJSON }
	_ = json.Unmarshal(raw, &list)
	proxied := false
	for _, pr := range list.Profiles {
		if pr.Name == "google" {
			proxied = pr.Proxied
		}
	}
	if !proxied {
		t.Fatalf("google isn't shown as proxied: %s", raw)
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
	var created profileJSON
	_ = json.Unmarshal(raw, &created)
	if strings.Contains(string(raw), shop.StickyToken) || strings.Contains(string(raw), "sticky") || strings.Contains(string(raw), "proxy.example") ||
		strings.Contains(string(raw), `"proxy"`) || !created.Proxied {
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
