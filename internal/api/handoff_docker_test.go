//go:build docker

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// TestHandoffHandsBackAtUntilURL is PROJ-118 with a real browser: an agent
// asks for a sign-in with until_url "/account", a person signs in on the
// test site and opens the account page (here through the container's X
// display, as the live view's input goes), and the handoff ends as done
// without a tap, with the fixed note. Meanwhile no page has a CDP client
// attached.
func TestHandoffHandsBackAtUntilURL(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "hu")
	// Link only, so the handoff needs no PIN (only a person can lower it).
	p, err := a.m.Profiles().Get(context.Background(), s.Profile)
	if err != nil {
		t.Fatal(err)
	}
	p.HandoffPolicy = profiles.PolicyLink
	if _, err := a.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	a.mustTool(s.ID, "navigate", map[string]any{"url": site.URL + "/login"})
	sock := a.m.Layout().ControlSocket(s.Profile)
	control := browsertest.ControlClient(sock)

	var h handoffJSON
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "sensitive_input", "detail": "Sign in", "until_url": "/account"}`, &h); code != http.StatusCreated {
		t.Fatalf("handoff: %d %+v", code, h)
	}
	noneAttached := func(when string) {
		t.Helper()
		for _, tg := range targets(t, sock) {
			if tg.Type == "page" && tg.Attached {
				t.Fatalf("%s, a page has a CDP client attached: %+v", when, tg)
			}
		}
	}
	noneAttached("as the handoff opens")
	status := func() handoffJSON {
		t.Helper()
		var got handoffJSON
		if code := a.do(http.MethodGet, "/api/handoffs/"+h.ID, "", &got); code != http.StatusOK {
			t.Fatalf("handoff status: %d", code)
		}
		return got
	}

	// The person: the X display, as the live view drives it.
	input := func(path, body string) {
		t.Helper()
		resp, err := control.Post("http://control/input/"+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d %s", path, body, resp.StatusCode, b)
		}
	}
	// shows reports whether the screen at (x, y) is about the colour.
	shows := func(x, y int, r, g, b uint8) bool {
		t.Helper()
		resp, err := control.Get("http://control/screenshot")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("screenshot: %v", err)
		}
		cr, cg, cb, _ := img.At(x, y).RGBA()
		near := func(v uint32, want uint8) bool { return max(int(v>>8), int(want))-min(int(v>>8), int(want)) <= 8 }
		return near(cr, r) && near(cg, g) && near(cb, b)
	}
	// The fields are 200 px bands under the toolbar: the user field blue
	// across the screen's y 200, the password field green across y 400.
	deadline := time.Now().Add(20 * time.Second)
	for !shows(720, 200, 0x2f, 0x6b, 0xff) {
		if time.Now().After(deadline) {
			t.Fatal("the login page never showed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !shows(720, 400, 0x16, 0xa3, 0x4a) {
		t.Fatal("the password field isn't where the test clicks")
	}
	input("click", `{"x": 720, "y": 200, "pace": "instant"}`)
	input("type", `{"text": "pier", "pace": "instant"}`)
	input("click", `{"x": 720, "y": 400, "pace": "instant"}`)
	input("type", `{"text": "hunter2", "pace": "instant"}`)
	input("key", `{"keys": "Return", "pace": "instant"}`)
	for deadline := time.Now().Add(10 * time.Second); site.Issued() == ""; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the login form was never submitted (%s)", site.Rejected())
		}
	}
	noneAttached("after signing in")
	// Signed in, still on /login: not the address yet.
	time.Sleep(time.Second)
	if got := status(); got.Status != "pending" {
		t.Fatalf("before the account page: %+v", got)
	}

	asked := time.Now()
	input("key", `{"keys": "ctrl+l", "pace": "instant"}`)
	input("type", fmt.Sprintf(`{"text": %q, "pace": "instant"}`, site.URL+"/account?from=handoff"))
	input("key", `{"keys": "Return", "pace": "instant"}`)
	var got handoffJSON
	req, _ := http.NewRequest(http.MethodGet, a.base+"/api/handoffs/"+h.ID+"?wait=20", nil)
	req.Header = a.header()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if got.Status != "done" || got.Note != handoff.AutoNote || !got.Reached {
		t.Fatalf("after the account page: %s", raw)
	}
	// The origin it opened on (§7) is all it says of where the page was.
	if strings.Contains(string(raw), "from=handoff") || strings.Contains(string(raw), site.URL+"/") {
		t.Fatalf("the answer quotes the address: %s", raw)
	}
	t.Logf("handed back %s after the address was typed", time.Since(asked).Round(time.Millisecond))
	if site.Seen() == "" {
		t.Fatal("the account page didn't get the login's cookie")
	}
	if again := status(); again.Status != "done" || again.Note != handoff.AutoNote {
		t.Fatalf("GET /api/handoffs/{id}: %+v", again)
	}
	noneAttached("after hand-back")
	// The agent has its browser back.
	if out := a.mustTool(s.ID, "snapshot", map[string]any{}); !strings.Contains(out, "Signed in as pier") {
		t.Fatalf("snapshot after hand-back: %s", out)
	}
}
