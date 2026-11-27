package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/settings"
)

// The parts of the proxies below that no agent reply may hold.
var proxyParts = []string{"hidden-proxy.example", "31877", "acct-9f3k2", "pw-7q2x", "sticky", "proxy.example"}

// noProxyKey fails if any object in the JSON body has a key that names a
// proxy other than "proxied", however deep: a "proxy" field coming back in
// any agent reply fails here.
func noProxyKey(t *testing.T, where string, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v: %s", where, err, raw)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if l := strings.ToLower(k); strings.Contains(l, "prox") && l != "proxied" {
					t.Errorf("%s has a %q field: %s", where, k, raw)
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(v)
}

// noProxyText fails if the body holds any part of a proxy.
func noProxyText(t *testing.T, where string, raw []byte) {
	t.Helper()
	for _, part := range proxyParts {
		if strings.Contains(string(raw), part) {
			t.Errorf("%s holds %q: %s", where, part, raw)
		}
	}
	noProxyKey(t, where, raw)
}

// TestAgentsSeeOnlyWhetherAProfileIsProxied (PROJ-205): the agent API's
// profile and session replies hold neither the host, port, user name nor
// {sticky} template of a profile's proxy, only "proxied".
func TestAgentsSeeOnlyWhetherAProfileIsProxied(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, proxy := range map[string]string{
		"google": "http://acct-9f3k2-{sticky}:pw-7q2x@hidden-proxy.example:31877", // a template
		"github": "socks5://acct-9f3k2:pw-7q2x@hidden-proxy.example:31877",        // a plain proxy
	} {
		p, err := f.profiles.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		p.Proxy = proxy
		if _, err := f.profiles.Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.profiles.Create(ctx, profiles.New("direct")); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"google": true, "github": true, "direct": false}

	check := func(where string, raw []byte, profs []profileJSON) {
		t.Helper()
		noProxyText(t, where, raw)
		for _, p := range profs {
			if w, ok := want[p.Name]; ok && p.Proxied != w {
				t.Errorf("%s: %s proxied %v, want %v", where, p.Name, p.Proxied, w)
			}
		}
	}

	var raw json.RawMessage
	if code := f.call(f.all, http.MethodGet, "/api/profiles", "", &raw); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	var list struct{ Profiles []profileJSON }
	_ = json.Unmarshal(raw, &list)
	if len(list.Profiles) != 3 {
		t.Fatalf("list: %s", raw)
	}
	check("GET /api/profiles", raw, list.Profiles)
	if !strings.Contains(string(raw), `"proxied":false`) {
		t.Errorf("proxied is left out when false: %s", raw)
	}

	// Running sessions, for a template, a plain proxy and none.
	var ids, names []string
	for name := range want {
		s := f.start(f.all, name, "api")
		ids, names = append(ids, s.ID), append(names, name)
	}
	raw = nil
	f.call(f.all, http.MethodGet, "/api/profiles", "", &raw)
	list.Profiles = nil
	_ = json.Unmarshal(raw, &list)
	check("GET /api/profiles while running", raw, list.Profiles)
	raw = nil
	if code := f.call(f.all, http.MethodGet, "/api/sessions", "", &raw); code != http.StatusOK {
		t.Fatalf("sessions: %d", code)
	}
	check("GET /api/sessions", raw, nil)
	for _, id := range ids {
		raw = nil
		f.call(f.all, http.MethodGet, "/api/sessions/"+id, "", &raw)
		check("GET /api/sessions/"+id, raw, nil)
	}
	raw = nil
	f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google"}`, &raw)
	check("POST /api/sessions (attach)", raw, nil)

	// A browser's last log line can quote its proxy (an older image's
	// dial error did): the session's last_log and error hide it, after a
	// stop and after a failed start, the {sticky} token filled in too.
	tok, err := f.profiles.StickyToken(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	proxyParts := append(proxyParts, tok)
	// What a browser's log could say of its own proxy, filled in.
	leakyLog := func(name string) string {
		proxy := map[string]string{
			"google": "http://acct-9f3k2-" + tok + ":pw-7q2x@hidden-proxy.example:31877",
			"github": "socks5://acct-9f3k2:pw-7q2x@hidden-proxy.example:31877",
		}[name]
		if proxy == "" {
			return `level=INFO msg="Chromium closed"`
		}
		u, _ := url.Parse(proxy)
		pw, _ := u.User.Password()
		return `level=ERROR msg=exiting err="couldn't find where the proxy exits (Get \"http://geo.test/geo\": ` +
			`dial tcp: lookup ` + u.Hostname() + `: no such host; ` + proxy + `; user ` + u.User.Username() + ` password ` + pw + ` at ` + u.Host + `)"`
	}
	checkLeaks := func(where string, raw []byte) {
		t.Helper()
		for _, part := range proxyParts {
			if strings.Contains(string(raw), part) {
				t.Errorf("%s holds %q: %s", where, part, raw)
			}
		}
		noProxyKey(t, where, raw)
	}
	for i, id := range ids {
		f.rt.LastLog = leakyLog(names[i])
		raw = nil
		if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+id, "", &raw); code != http.StatusOK {
			t.Fatalf("stop: %d %s", code, raw)
		}
		checkLeaks("DELETE /api/sessions/"+id, raw)
		var s sessionJSON
		_ = json.Unmarshal(raw, &s)
		if s.LastLog == "" || (names[i] != "direct" && !strings.Contains(s.LastLog, "<hidden>")) {
			t.Errorf("the stop kept no last_log: %s", raw)
		}
		raw = nil
		f.call(f.all, http.MethodGet, "/api/sessions/"+id, "", &raw)
		checkLeaks("GET /api/sessions/"+id+" after the stop", raw)
	}
	status := 1
	f.rt.ExitOnStart = &status
	for _, name := range []string{"google", "github"} {
		f.rt.LastLog = leakyLog(name)
		raw = nil
		if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "`+name+`"}`, &raw); code != http.StatusBadGateway {
			t.Fatalf("failed start of %s: %d %s", name, code, raw)
		}
		checkLeaks("a failed start of "+name, raw)
		var s sessionJSON
		_ = json.Unmarshal(raw, &s)
		if !strings.Contains(s.Error, "couldn't find where the proxy exits") || !strings.Contains(s.LastLog, "<hidden>") {
			t.Errorf("the failed start says too little: %s", raw)
		}
		raw = nil
		f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &raw)
		checkLeaks("GET /api/sessions/"+s.ID+" after a failed start", raw)
	}
	raw = nil
	f.call(f.all, http.MethodGet, "/api/sessions", "", &raw)
	checkLeaks("GET /api/sessions after a failed start", raw)
	f.rt.ExitOnStart, f.rt.LastLog = nil, ""

	// A profile an agent makes gets the admin's default proxy, template
	// or not, and the answer says only that it's proxied.
	st := settings.New(f.db)
	for i, def := range []string{
		"http://acct-9f3k2-{sticky}:pw-7q2x@hidden-proxy.example:31877",
		"http://acct-9f3k2:pw-7q2x@hidden-proxy.example:31877",
		"",
	} {
		if err := st.Save(ctx, settings.Settings{DefaultPolicy: profiles.PolicyPIN, DefaultAutoStop: 15 * time.Minute, DefaultProxy: def}); err != nil {
			t.Fatal(err)
		}
		f.api.SetSettings(st)
		name := []string{"made-template", "made-plain", "made-direct"}[i]
		raw = nil
		if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "`+name+`"}`, &raw); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, code, raw)
		}
		var p profileJSON
		_ = json.Unmarshal(raw, &p)
		want[name] = def != ""
		check("POST /api/profiles "+name, raw, []profileJSON{p})
		if stored, err := f.profiles.Get(ctx, name); err != nil || stored.Proxy != def {
			t.Fatalf("stored proxy %q, %v; want the admin's default %q", stored.Proxy, err, def)
		}
	}
}
