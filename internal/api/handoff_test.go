package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHandoffAPI(t *testing.T) {
	f := newFixture(t)
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
