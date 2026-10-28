package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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
