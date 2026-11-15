package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
