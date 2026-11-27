package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

func TestHandoffAPI(t *testing.T) {
	f := newFixture(t)
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	s := f.start(f.all, "github", "visual") // the scoped token can't see github

	var h handoffJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "sensitive_input", "detail": "Enter the 2FA code", "require": "link"}`, &h); code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, h)
	}
	if !strings.Contains(h.URL, "/h/") || h.ExpiresIn != 900 || h.Status != "pending" || h.Policy != "pin" {
		t.Fatalf("created %+v (an agent asking for link only must not lower the pin default)", h)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, nil); code != http.StatusConflict {
		t.Fatalf("second handoff: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "please"}`, nil); code != http.StatusConflict && code != http.StatusBadRequest {
		t.Fatalf("bad reason: %d", code)
	}
	// While a person helps, the agent is paused.
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 1, "y": 1}`, nil); code != http.StatusConflict {
		t.Fatalf("click during a handoff: %d", code)
	}

	var got handoffJSON
	start := time.Now()
	if code := f.call(f.all, http.MethodGet, "/api/handoffs/"+h.ID+"?wait=1", "", &got); code != http.StatusOK || got.Status != "pending" || time.Since(start) < time.Second {
		t.Fatalf("short wait: %d %+v after %s", code, got, time.Since(start))
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		hh, _ := f.handoffs.Get(context.Background(), h.ID)
		_, _ = f.handoffs.Finish(context.Background(), hh, "done", "used a backup code")
	}()
	if code := f.call(f.all, http.MethodGet, "/api/handoffs/"+h.ID+"?wait=30", "", &got); code != http.StatusOK || got.Status != "done" || got.Note != "used a backup code" || !strings.Contains(got.Message, "re-read") {
		t.Fatalf("long-poll: %d %+v", code, got)
	}
	if code := f.call(f.scoped, http.MethodGet, "/api/handoffs/"+h.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("scoped token on another profile's handoff: %d", code)
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 1, "y": 1}`, nil); code != http.StatusNoContent {
		t.Fatalf("click after hand-back: %d", code)
	}
}

func TestHandoffNeedsAPINToBeSet(t *testing.T) {
	f := newFixture(t) // no PIN set; profiles default to the PIN policy
	s := f.start(f.all, "github", "visual")
	var e struct{ Error string }
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "sensitive_input"}`, &e); code != http.StatusConflict || !strings.Contains(e.Error, "PIN") {
		t.Fatalf("PIN handoff with no PIN set: %d %+v", code, e)
	}
	if live, _ := f.m.Live("github"); live.State != sessions.Running {
		t.Fatalf("refused handoff left the session %s", live.State)
	}
	// Full sign-in needs no PIN, and nothing was left open by the refusal.
	var h handoffJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "sensitive_input", "require": "signin"}`, &h); code != http.StatusCreated || h.Policy != "signin" {
		t.Fatalf("sign-in handoff with no PIN set: %d %+v", code, h)
	}
}

func TestWithdrawHandoff(t *testing.T) {
	f := newFixture(t)
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	s := f.start(f.all, "github", "visual")
	var h handoffJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, &h); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if code := f.call(f.scoped, http.MethodDelete, "/api/handoffs/"+h.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("another profile's token withdrawing: %d", code)
	}
	var got handoffJSON
	if code := f.call(f.all, http.MethodDelete, "/api/handoffs/"+h.ID, `{"note": "the person said no"}`, &got); code != http.StatusOK || got.Status != "cancelled" || !strings.Contains(got.Note, "the person said no") {
		t.Fatalf("withdraw: %d %+v", code, got)
	}
	if live, _ := f.m.Live("github"); live.State != sessions.Running {
		t.Fatalf("browser not given back: %s", live.State)
	}
	// Withdrawing again changes nothing.
	if code := f.call(f.all, http.MethodDelete, "/api/handoffs/"+h.ID, "", &got); code != http.StatusOK || got.Status != "cancelled" {
		t.Fatalf("second withdraw: %d %+v", code, got)
	}
}

func TestCantWithdrawAHandoffAPersonOpened(t *testing.T) {
	f := newFixture(t)
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	s := f.start(f.all, "github", "visual")
	var h handoffJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "sensitive_input"}`, &h); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	// Asking again names the open one, so an agent that lost its ID can recover.
	var e struct{ Error string }
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, &e); code != http.StatusConflict || !strings.Contains(e.Error, h.ID) {
		t.Fatalf("second handoff: %d %s", code, e.Error)
	}
	hh, _ := f.handoffs.Get(context.Background(), h.ID)
	if err := f.handoffs.Unlock(context.Background(), hh, "the person's browser"); err != nil {
		t.Fatal(err)
	}
	if code := f.call(f.all, http.MethodDelete, "/api/handoffs/"+h.ID, "", nil); code != http.StatusConflict {
		t.Fatalf("withdraw while a person has it: %d", code)
	}
	if got, _ := f.handoffs.Get(context.Background(), h.ID); got.Status != "pending" {
		t.Fatalf("withdrawn anyway: %s", got.Status)
	}
}

// A note sent chunked, with no length, is read all the same.
func TestWithdrawReadsAChunkedNote(t *testing.T) {
	f := newFixture(t)
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	s := f.start(f.all, "github", "visual")
	var h handoffJSON
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, &h); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, `{"note": "not needed after all"}`)
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodDelete, f.srv.URL+"/api/handoffs/"+h.ID, pr)
	req.Header.Set("Authorization", "Bearer "+f.all)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got, _ := f.handoffs.Get(context.Background(), h.ID); resp.StatusCode != http.StatusOK || !strings.Contains(got.Note, "not needed after all") {
		t.Fatalf("withdraw with a chunked note: %d, note %q", resp.StatusCode, got.Note)
	}
}

// TestHandoffUntilURL: until_url is checked, echoed, and ends the handoff
// as done once the page navigates to a matching origin and path, with a
// fixed note that never quotes the address.
func TestHandoffUntilURL(t *testing.T) {
	f := newFixture(t)
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	f.b.mu.Lock()
	f.b.pageURL = "https://site.example/login"
	f.b.mu.Unlock()
	s := f.start(f.all, "google", "api")
	path := "/api/sessions/" + s.ID + "/handoff"
	for _, bad := range []string{
		`"/` + strings.Repeat("a", 200) + `"`,
		`"/acc\u0007ount"`, `"/acc\nount"`, `"/acc\u202eount"`,
		`"/cb?code="`, `"/account#top"`, `"/"`, `"/a"`, `"https://"`, `"..."`,
	} {
		var e struct{ Error string }
		if code := f.call(f.all, http.MethodPost, path, `{"reason": "sensitive_input", "until_url": `+bad+`}`, &e); code != http.StatusBadRequest || !strings.Contains(e.Error, "until_url") {
			t.Fatalf("until_url %s: %d %q", bad, code, e.Error)
		}
	}
	if live, _ := f.m.Live("google"); live.State != sessions.Running {
		t.Fatalf("a refused handoff paused the agent: %s", live.State)
	}

	// A pattern only in the query never matches.
	var h handoffJSON
	if code := f.call(f.all, http.MethodPost, path, `{"reason": "sensitive_input", "until_url": "code="}`, &h); code != http.StatusCreated || h.UntilURL != "code=" {
		t.Fatalf("create: %d %+v", code, h)
	}
	f.unlock(h.ID)
	f.b.mu.Lock()
	f.b.pageURL = "https://site.example/cb?code=1"
	f.b.mu.Unlock()
	var got handoffJSON
	if code := f.call(f.all, http.MethodGet, "/api/handoffs/"+h.ID+"?wait=2", "", &got); code != http.StatusOK || got.Status != "pending" {
		t.Fatalf("a match in the query: %d %+v", code, got)
	}
	// Opened, it is the person's to end now.
	if ho, err := f.handoffs.Get(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	} else if _, err := f.handoffs.Finish(context.Background(), ho, handoff.Cant, ""); err != nil {
		t.Fatal(err)
	}

	// An empty one is no pattern.
	h = handoffJSON{}
	if code := f.call(f.all, http.MethodPost, path, `{"reason": "sensitive_input", "until_url": ""}`, &h); code != http.StatusCreated || h.UntilURL != "" {
		t.Fatalf("create with an empty until_url: %d %+v", code, h)
	}
	if code := f.call(f.all, http.MethodDelete, "/api/handoffs/"+h.ID, "", &got); code != http.StatusOK {
		t.Fatalf("withdraw: %d %+v", code, got)
	}

	f.b.mu.Lock()
	f.b.pageURL = "https://site.example/login"
	f.b.methods = nil
	f.b.mu.Unlock()
	if code := f.call(f.all, http.MethodPost, path, `{"reason": "sensitive_input", "detail": "Sign in", "until_url": "site.example/account"}`, &h); code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, h)
	}
	f.unlock(h.ID) // the person opens the link: the watch looks from then
	go func() {
		time.Sleep(1200 * time.Millisecond)
		f.b.mu.Lock()
		f.b.pageURL = "https://site.example/account/home?token=s3cr3t"
		f.b.mu.Unlock()
	}()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/handoffs/"+h.ID+"?wait=30", nil)
	req.Header.Set("Authorization", "Bearer "+f.all)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || !got.Reached || got.Note != handoff.AutoNote || !strings.Contains(got.Message, "until_url") || !strings.Contains(got.Message, "re-read") {
		t.Fatalf("after the page reached the address: %s", raw)
	}
	if strings.Contains(string(raw), "s3cr3t") || strings.Contains(string(raw), "/home") {
		t.Fatalf("the answer quotes the page's address: %s", raw)
	}
	f.b.mu.Lock()
	methods := f.b.methods
	f.b.mu.Unlock()
	if len(methods) != 0 {
		t.Fatalf("the watch sent %v; it reads /json/list over HTTP only", methods)
	}
	if live, _ := f.m.Live("google"); live.State != sessions.Running {
		t.Fatalf("browser not given back: %s", live.State)
	}
}

// unlock does what a person opening a handoff's link does.
func (f *fixture) unlock(id string) {
	f.t.Helper()
	h, err := f.handoffs.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.handoffs.Unlock(context.Background(), h, "the person's browser"); err != nil {
		f.t.Fatal(err)
	}
}

// A handoff whose browser doesn't answer its hand-back (PROJ-196): reading
// it never waits on that browser, a wait answers within its time, still
// pending, and a withdrawal answers 503 within its bound. None of it is
// an internal error.
func TestWedgedHandoffAnswersInTime(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelError}))
	f := newFixtureLogging(t, log)
	if err := f.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	f.handoffs.SetHandBackWait(2 * time.Second)
	// Every browser's lift fails: no hand-back ever ends.
	inner := f.b.handler()
	f.rt.Control = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/input/release" {
			http.Error(w, "wedged", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(method, path string) (int, handoffJSON, time.Duration) {
		t.Helper()
		req, _ := http.NewRequest(method, f.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+f.all)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v after %s", method, path, err, time.Since(start))
		}
		defer func() { _ = resp.Body.Close() }()
		var h handoffJSON
		_ = json.NewDecoder(resp.Body).Decode(&h)
		return resp.StatusCode, h, time.Since(start)
	}
	open := func(profile string) (sessionJSON, handoffJSON) {
		t.Helper()
		s := f.start(f.all, profile, "visual")
		var h handoffJSON
		if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, &h); code != http.StatusCreated {
			t.Fatalf("create: %d", code)
		}
		// The admin takes the browser in the live view meanwhile, so the
		// handoff's end has a hand-back to wait for.
		if err := f.m.TakeControl(s.ID, sessions.AdminHolder); err != nil {
			t.Fatal(err)
		}
		return s, h
	}
	expired, eh := open("github")
	withdrawn, wh := open("google")
	t.Cleanup(func() {
		for _, s := range []sessionJSON{expired, withdrawn} {
			_, _ = f.m.Stop(context.Background(), s.ID) // which ends the hand-backs
		}
	})

	// Its time is up: a wait starts its end, and answers within its time.
	if _, err := f.db.Exec(`UPDATE handoffs SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Second).UnixMilli(), eh.ID); err != nil {
		t.Fatal(err)
	}
	code, got, took := call(http.MethodGet, "/api/handoffs/"+eh.ID+"?wait=5")
	if code != http.StatusOK || got.Status != "pending" || took < 4*time.Second || took > 8*time.Second {
		t.Fatalf("waiting 5 s on a wedged expired handoff: %d %s after %s", code, got.Status, took)
	}
	if code, got, took = call(http.MethodGet, "/api/handoffs/"+eh.ID); code != http.StatusOK || got.Status != "pending" || took > 2*time.Second {
		t.Fatalf("reading it: %d %s after %s", code, got.Status, took)
	}

	// The agent withdraws one: 503 once its bound has passed.
	code, _, took = call(http.MethodDelete, "/api/handoffs/"+wh.ID)
	if code != http.StatusServiceUnavailable || took > 8*time.Second {
		t.Fatalf("withdrawing a wedged handoff: %d after %s", code, took)
	}
	if code, got, _ = call(http.MethodGet, "/api/handoffs/"+wh.ID+"?wait=1"); code != http.StatusOK || got.Status != "pending" {
		t.Fatalf("after the withdrawal: %d %s", code, got.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if buf.Len() > 0 {
		t.Fatalf("logged as errors: %s", buf.String())
	}
}
