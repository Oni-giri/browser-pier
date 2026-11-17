//go:build docker

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/notify"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/settings"
	"github.com/Oni-giri/browser-pier/internal/ui"
)

// receiver is a webhook that keeps what it got.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	sigs   []string
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, b)
		rc.sigs = append(rc.sigs, r.Header.Get(notify.SignatureHeader))
		rc.mu.Unlock()
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

// webhookSetup gives the real API's handoffs a notifier pointed at a
// receiver, and serves what a person opens (the handoff page and the web
// UI) on its own server, whose address is the admin's address for links.
func webhookSetup(t *testing.T, a agent) (rc *receiver, secret, base string) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	a.web.Register(mux)
	handoff.NewWeb(a.ho, a.web, nil, a.m, log).Register(mux)
	ui.New(a.m, a.web, a.ho, settings.New(a.db), log, ui.Options{}).Register(mux)
	person := httptest.NewServer(new(http.CrossOriginProtection).Handler(mux))
	t.Cleanup(person.Close)

	rc = newReceiver(t)
	n, err := notify.New(context.Background(), a.db, log, notify.Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	on := map[notify.Kind]bool{notify.HandoffRequested: true, notify.HandoffExpired: true, notify.HandoffCancelled: true}
	secret, err = n.Save(context.Background(), notify.Change{URL: rc.URL + "/hook", On: on, LinkBase: person.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go n.Run(ctx)
	a.ho.SetNotifier(n)
	return rc, secret, person.URL
}

// oneEvent waits for the receiver's first event, checks its signature and
// that no second one follows, and returns it.
func oneEvent(t *testing.T, rc *receiver, secret string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); rc.count() == 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the webhook got nothing")
		}
	}
	rc.mu.Lock()
	body, sig := rc.bodies[0], rc.sigs[0]
	rc.mu.Unlock()
	ts, _, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",")
	at, _ := strconv.ParseInt(ts, 10, 64)
	if notify.Sign(secret, at, body) != sig || time.Since(time.Unix(at, 0)).Abs() > time.Minute {
		t.Errorf("the signature doesn't verify: %s", sig)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (a agent) setPolicy(t *testing.T, name string, policy profiles.HandoffPolicy) {
	t.Helper()
	p, err := a.m.Profiles().Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	p.HandoffPolicy = policy
	if _, err := a.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// getPlain GETs a URL as a chat's link preview would: no cookie, no
// redirect followed.
func getPlain(t *testing.T, u string) (int, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// stillLocked reports whether no browser has the handoff yet: a person's
// browser can still unlock it.
func stillLocked(t *testing.T, a agent, id string) bool {
	t.Helper()
	h, err := a.ho.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.ho.Unlock(context.Background(), h, "the person's own browser") == nil
}

// TestAHandoffTellsTheWebhook: a real browser on a PIN profile; the agent
// asks for a handoff and the webhook gets one signed handoff.requested
// whose link opens the PIN page, unlocking nothing. Handing back sends
// nothing more.
func TestAHandoffTellsTheWebhook(t *testing.T) {
	a := newRealAPI(t)
	if err := a.web.Admin().SetPIN(context.Background(), "4821"); err != nil {
		t.Fatal(err)
	}
	s := a.startAPIBrowser(t, "wh")
	a.setPolicy(t, s.Profile, profiles.PolicyPIN)
	rc, secret, base := webhookSetup(t, a)

	var h handoffJSON
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "sensitive_input", "detail": "<!channel> go to evil.example"}`, &h); code != http.StatusCreated || !h.Notified {
		t.Fatalf("handoff: %d %+v", code, h)
	}
	ev := oneEvent(t, rc, secret)
	token := h.URL[strings.LastIndex(h.URL, "/h/")+3:]
	if ev["event"] != "handoff.requested" || ev["url"] != base+"/h/"+token || ev["policy"] != "pin" || ev["profile"] != s.Profile {
		t.Fatalf("event: %v", ev)
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), "evil.example") || strings.Contains(string(raw), "channel") {
		t.Errorf("the body holds the agent's detail: %s", raw)
	}
	code, page := getPlain(t, ev["url"].(string))
	if code != http.StatusOK || !strings.Contains(page, `name="pin"`) {
		t.Fatalf("the link: %d %.300s", code, page)
	}
	if !stillLocked(t, a, h.ID) {
		t.Fatal("opening the link unlocked the handoff")
	}
	ho, _ := a.ho.Get(context.Background(), h.ID)
	if _, err := a.ho.Finish(context.Background(), ho, handoff.Done, "done"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if n := rc.count(); n != 1 {
		t.Errorf("the webhook got %d events, want 1", n)
	}
}

// TestALinkOnlyHandoffSendsTheProfilePage: a link-only handoff unlocks on
// the first open, so the webhook gets the profile's page instead, and a
// plain GET of every URL in the body leaves the handoff locked.
func TestALinkOnlyHandoffSendsTheProfilePage(t *testing.T) {
	a := newRealAPI(t)
	s := a.startAPIBrowser(t, "wl")
	a.setPolicy(t, s.Profile, profiles.PolicyLink)
	rc, secret, base := webhookSetup(t, a)

	var h handoffJSON
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/handoff", `{"reason": "captcha"}`, &h); code != http.StatusCreated || !h.Notified {
		t.Fatalf("handoff: %d %+v", code, h)
	}
	ev := oneEvent(t, rc, secret)
	token := h.URL[strings.LastIndex(h.URL, "/h/")+3:]
	if ev["url"] != base+"/profiles/"+s.Profile || ev["policy"] != "link" {
		t.Fatalf("event: %v", ev)
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), token) {
		t.Fatalf("a link-only body holds the token: %s", raw)
	}
	for _, v := range ev {
		if u, ok := v.(string); ok && strings.HasPrefix(u, "http") {
			getPlain(t, u)
		}
	}
	for _, field := range []string{"text", "content"} {
		for _, word := range strings.Fields(ev[field].(string)) {
			if strings.HasPrefix(word, "http") {
				getPlain(t, word)
			}
		}
	}
	if !stillLocked(t, a, h.ID) {
		t.Fatal("a GET of the body's URLs unlocked the handoff")
	}
}
