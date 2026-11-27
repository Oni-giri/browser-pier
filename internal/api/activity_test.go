package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/settings"
)

// whats lists a session's activity, oldest first.
func (f *fixture) whats(id string) []string {
	var out []string
	for _, a := range f.m.Activity(id) {
		out = append([]string{a.Agent + ": " + a.What}, out...)
	}
	return out
}

func TestActivityLog(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	if code := f.call(f.all, http.MethodPost, "/api/sessions", `{"profile": "google"}`, nil); code != http.StatusOK {
		t.Fatalf("attach: %d", code)
	}
	f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID+"/screenshot", "", nil)
	f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 412, "y": 220}`, nil)
	f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 99999, "y": 1}`, nil) // refused: not logged
	const password = "correct-horse-ünïcode"
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/type", `{"text": "`+password+`"}`, nil); code != http.StatusNoContent {
		t.Fatalf("type: %d", code)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "invoice.pdf")
	_, _ = fw.Write([]byte("%PDF"))
	_ = mw.Close()
	if r, _ := f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/files", &body, mw.FormDataContentType()); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", r.StatusCode)
	}
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, nil); code != http.StatusCreated {
		t.Fatalf("handoff: %d", code)
	}

	got := f.whats(s.ID)
	want := []string{
		"claude: Started the browser", "claude: Attached", "claude: Took a screenshot", "claude: Clicked at 412, 220",
		"claude: Typed 21 characters", "claude: Uploaded invoice.pdf", "claude: Paused, waiting for you: CAPTCHA",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("activity:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, a := range f.m.Activity(s.ID) {
		if strings.Contains(a.What, password) || strings.Contains(a.What, "horse") {
			t.Fatalf("the typed text is in the activity log: %q", a.What)
		}
		if a.Attention != strings.HasPrefix(a.What, "Paused") {
			t.Fatalf("attention on %+v", a)
		}
	}
}

func TestActivityLogCDPAndStop(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "api")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // after an upgrade the connection owns the body
	conn, _, err := websocket.Dial(ctx, f.ticket(f.scoped, s.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.m.Activity(s.ID)) == 2 })
	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return len(f.m.Activity(s.ID)) == 3 })
	if got := strings.Join(f.whats(s.ID), "\n"); got != "claude: Started the browser\nhelper: Connected over CDP\nhelper: Disconnected" {
		t.Fatalf("activity:\n%s", got)
	}

	// Once stopped, the session's log is gone.
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if got := f.m.Activity(s.ID); got != nil {
		t.Fatalf("a stopped session kept its activity: %+v", got)
	}
}

func TestCreatedProfilesStartFromTheSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	st := settings.New(f.db)
	const proxy = "http://user:s3cret@corp-proxy.test:3128"
	if err := st.Save(ctx, settings.Settings{DefaultPolicy: profiles.PolicySignIn, DefaultAutoStop: 30 * time.Minute, DefaultProxy: proxy}); err != nil {
		t.Fatal(err)
	}
	f.api.SetSettings(st)

	var raw json.RawMessage
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "shop", "handoff_policy": "pin"}`, &raw); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var p profileJSON
	_ = json.Unmarshal(raw, &p)
	if p.HandoffPolicy != "signin" || p.AutoStopMinutes != 30 || !p.Proxied || strings.Contains(string(raw), "corp-proxy") || strings.Contains(string(raw), "s3cret") {
		t.Fatalf("created %+v (an agent asking for pin must not lower a signin default)", p)
	}
	stored, err := f.profiles.Get(ctx, "shop")
	if err != nil || stored.Proxy != proxy {
		t.Fatalf("stored proxy %q, %v; want the admin's default", stored.Proxy, err)
	}
	// The agent's own choices still apply where it may make them.
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "quick", "auto_stop_minutes": 5}`, &p); code != http.StatusCreated || p.AutoStopMinutes != 5 {
		t.Fatalf("create with auto-stop: %d %+v", code, p)
	}
	// And still can't choose a proxy.
	if code := f.call(f.all, http.MethodPost, "/api/profiles", `{"name": "evil", "proxy": "http://evil.test:1"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("proxy through the API: %d", code)
	}
}
