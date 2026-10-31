package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCDPTickets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.start(f.all, "google", "api")
	get := func(u string, out any) int {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	httpURL := func(cdpURL string) string { return "http" + strings.TrimPrefix(cdpURL, "ws") }
	dial := func(u string) (*websocket.Conn, int) {
		t.Helper()
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, resp, err := websocket.Dial(dctx, u, nil)
		if err != nil {
			if resp == nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return nil, resp.StatusCode
		}
		return conn, http.StatusSwitchingProtocols
	}

	// The token itself is never taken in the URL.
	for _, p := range []string{"/cdp/google?token=", "/cdp/google/json/version?token="} {
		if code := get(f.srv.URL+p+f.all, nil); code != http.StatusBadRequest {
			t.Errorf("%s…: %d", p, code)
		}
	}

	// A ticket opens the browser once.
	u := f.ticket(f.all, s.ID)
	conn, code := dial(u)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("first use: %d", code)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if _, code := dial(u); code != http.StatusUnauthorized {
		t.Fatalf("second use: %d", code)
	}

	// The /json endpoints use it up too, and hand back fresh ones, one
	// per URL, so a client can follow them.
	entry := f.ticket(f.all, s.ID)
	var version map[string]any
	if code := get(httpURL(strings.Replace(entry, "?", "/json/version?", 1)), &version); code != http.StatusOK {
		t.Fatalf("json/version: %d", code)
	}
	next, _ := version["webSocketDebuggerUrl"].(string)
	nu, err := url.Parse(next)
	if err != nil || nu.Path != "/cdp/google/devtools/browser/abc" || nu.Query().Get("ticket") == "" || strings.Contains(next, f.all) {
		t.Fatalf("version %v", version)
	}
	if strings.Contains(next, strings.SplitN(entry, "?ticket=", 2)[1]) {
		t.Fatal("the rewritten URL reuses the used ticket")
	}
	if code := get(httpURL(strings.Replace(entry, "?", "/json/list?", 1)), nil); code != http.StatusUnauthorized {
		t.Fatalf("entry ticket reused on json/list: %d", code)
	}
	conn, code = dial(next)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("following the rewritten URL: %d", code)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	var targets []map[string]any
	if code := get(httpURL(strings.Replace(f.ticket(f.all, s.ID), "?", "/json/list?", 1)), &targets); code != http.StatusOK || len(targets) != 1 {
		t.Fatalf("json/list: %d %v", code, targets)
	}
	if v, _ := targets[0]["webSocketDebuggerUrl"].(string); !strings.Contains(v, "/devtools/page/p1?ticket="+"bp_cdp_") {
		t.Fatalf("targets %v", targets)
	}

	// A ticket is for one profile.
	other := f.start(f.all, "github", "api")
	if _, code := dial(strings.Replace(f.ticket(f.all, s.ID), "/cdp/google", "/cdp/github", 1)); code != http.StatusUnauthorized {
		t.Fatalf("google's ticket on github: %d", code)
	}
	if code := f.call(f.scoped, http.MethodPost, "/api/sessions/"+other.ID+"/cdp-ticket", "", nil); code != http.StatusNotFound {
		t.Fatalf("scoped token minting for another profile: %d", code)
	}

	// Revoking the token voids its tickets.
	tok, secret, err := f.tokens.Create(ctx, "doomed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	doomed := f.ticket(secret, s.ID)
	if err := f.tokens.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, code := dial(doomed); code != http.StatusUnauthorized {
		t.Fatalf("revoked token's ticket: %d", code)
	}

	// No tickets for Visual mode, where nothing may attach over CDP.
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+other.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	visual := f.start(f.all, "github", "visual")
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+visual.ID+"/cdp-ticket", "", nil); code != http.StatusConflict {
		t.Fatalf("ticket for a Visual-mode session: %d", code)
	}

	f.b.mu.Lock()
	leaks := f.b.leaks
	f.b.mu.Unlock()
	if len(leaks) != 0 {
		t.Fatalf("agent credentials reached the browser: %v", leaks)
	}
}
