package handoff

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// count counts a session's live entries saying what.
func (f *fixture) count(sessionID, what string) int {
	n := 0
	for _, a := range f.m.Activity(sessionID) {
		if a.What == what {
			if a.Kind != sessions.KindHandoff {
				f.t.Errorf("%q has kind %s", what, a.Kind)
			}
			n++
		}
	}
	return n
}

func TestLinkOpenedIsNotedOnce(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	h, token := f.open("")
	if err := f.svc.Unlock(ctx, h, "browser-1"); err != nil {
		t.Fatal(err)
	}
	// The same browser again (a reload), then another one, refused.
	if err := f.svc.Unlock(ctx, h, "browser-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Unlock(ctx, h, "browser-2"); err != ErrOtherBrowser {
		t.Fatalf("another browser: %v", err)
	}
	if n := f.count(h.SessionID, openedLog); n != 1 {
		t.Fatalf("link opened noted %d times", n)
	}
	// Through the page, too: opening and reloading notes nothing more.
	phone := browser()
	for range 2 {
		f.do(phone, "GET", "/h/"+token, nil, nil)
	}
	if n := f.count(h.SessionID, openedLog); n != 1 {
		t.Fatalf("link opened noted %d times", n)
	}
}

func TestCancelledSaysWhy(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	h, _ := f.open("")
	if _, err := f.svc.Withdraw(ctx, h, "the agent's own reason"); err != nil {
		t.Fatal(err)
	}
	if n := f.count(h.SessionID, withdrawnLog); n != 1 {
		t.Fatalf("withdrawal noted %d times", n)
	}
	h, token := f.open("")
	for range MaxPINFailures {
		f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"0000"}}, nil)
	}
	if n := f.count(h.SessionID, pinsLog); n != 1 {
		t.Fatalf("five wrong PINs noted %d times", n)
	}
	if n := f.count(h.SessionID, "Help request cancelled"); n != 0 {
		t.Fatal("a cancellation doesn't say why")
	}
	for _, a := range f.m.Activity(h.SessionID) {
		if strings.Contains(a.What, "agent's own reason") {
			t.Fatalf("the withdrawal note is in the log: %+v", a)
		}
	}
}

// The log names no detail, note, origin, address, PIN or link, from the
// request to the hand-back.
func TestHandoffEntriesHoldNoneOfItsWords(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	ctx := context.Background()
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://login.example/start"})
	s := f.running()
	h, token, err := f.svc.Open(ctx, Request{SessionID: s.ID, Agent: "research-bot", Reason: SensitiveInput,
		Detail: "DETAIL-WORDS", UntilURL: "until.example/account"})
	if err != nil {
		t.Fatal(err)
	}
	phone := browser()
	f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"1111"}}, nil)
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d", p.code)
	}
	if p := f.do(phone, "POST", "/h/"+token+"/finish", url.Values{"status": {"done"}, "note": {"NOTE-WORDS"}}, nil); p.code != http.StatusOK {
		t.Fatalf("finish: %d", p.code)
	}
	var all []string
	for _, a := range f.m.Activity(h.SessionID) {
		all = append(all, a.What+" "+a.Title)
	}
	got := strings.Join(all, "\n")
	for _, never := range []string{"DETAIL-WORDS", "NOTE-WORDS", "login.example", "until.example", "4821", "1111", token} {
		if strings.Contains(got, never) {
			t.Errorf("the log holds %q:\n%s", never, got)
		}
	}
	for _, want := range []string{openedLog, "Handed back: done"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log lacks %q:\n%s", want, got)
		}
	}
}

func TestTheLinksControlIsNoted(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, token := f.open("")
	phone := browser()
	if p := f.do(phone, "POST", "/h/"+token+"/pin", url.Values{"pin": {"4821"}}, nil); p.code != http.StatusSeeOther {
		t.Fatalf("right PIN: %d %s", p.code, p.body)
	}
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/h/"+token+"/offer", strings.NewReader(pionOffer(t)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := phone.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("offer: %d", resp.StatusCode)
	}
	if p := f.do(phone, "POST", "/h/"+token+"/finish", url.Values{"status": {"done"}}, nil); p.code != http.StatusOK {
		t.Fatalf("finish: %d", p.code)
	}
	var got []string
	for _, a := range f.m.Activity(h.SessionID) {
		// The view closing with the handoff may or may not have been
		// noted yet.
		if a.Kind != sessions.KindAction && a.What != "Nobody is watching" {
			got = append([]string{a.What}, got...)
		}
	}
	want := []string{openedLog, "Someone started watching", "The person took control through the handoff link", "Control handed back", "Handed back: done"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries %q, want %q", got, want)
	}
}
