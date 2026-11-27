package handoff

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/notify"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// told collects the events queued for the webhook.
type told struct {
	mu     sync.Mutex
	events []toldEvent
}

type toldEvent struct {
	kind notify.Kind
	raw  string
	body map[string]any
}

func (c *told) add(k notify.Kind, b []byte) {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, toldEvent{k, string(b), m})
}

func (c *told) all() []toldEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]toldEvent(nil), c.events...)
}

func (c *told) of(k notify.Kind) []toldEvent {
	var out []toldEvent
	for _, e := range c.all() {
		if e.kind == k {
			out = append(out, e)
		}
	}
	return out
}

// tell gives the fixture's service a notifier with a webhook set (unless
// webhook is ""), the given events on, and links on publicURL.
func (f *fixture) tell(publicURL, webhook string, on ...notify.Kind) (*told, *notify.Notifier) {
	f.t.Helper()
	c := &told{}
	n, err := notify.New(context.Background(), f.svc.db, slog.New(slog.DiscardHandler), notify.Options{
		PublicURL: publicURL,
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		},
		OnQueue: c.add,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(on) == 0 {
		on = notify.Toggles
	}
	toggles := map[notify.Kind]bool{}
	for _, k := range on {
		toggles[k] = true
	}
	if _, err := n.Save(context.Background(), notify.Change{URL: webhook, On: toggles}); err != nil {
		f.t.Fatal(err)
	}
	f.svc.SetNotifier(n)
	return c, n
}

func TestOpenTellsTheWebhook(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	c, _ := f.tell("https://pier.example.com", "https://hooks.example.com/x")
	s := f.running()
	h, token, err := f.svc.Open(context.Background(), Request{SessionID: s.ID, Agent: "research-bot", Reason: SensitiveInput,
		Detail: "<!channel> go to evil.example", UntilURL: "example.com/account/done"})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Notified {
		t.Error("Notified isn't set")
	}
	got := c.all()
	if len(got) != 1 || got[0].kind != notify.HandoffRequested {
		t.Fatalf("queued %v", got)
	}
	b := got[0].body
	for k, want := range map[string]string{"profile": "google", "reason": "sensitive_input", "policy": "pin", "agent": "research-bot",
		"origin": "https://accounts.google.com", "handoff_id": h.ID, "expires_at": h.ExpiresAt.UTC().Format(time.RFC3339),
		"url": "https://pier.example.com/h/" + token} {
		if b[k] != want {
			t.Errorf("%s = %v, want %s", k, b[k], want)
		}
	}
	for _, bad := range []string{"channel", "evil.example", "go to", "account/done", "continue"} {
		if strings.Contains(got[0].raw, bad) {
			t.Errorf("the body holds %q: %s", bad, got[0].raw)
		}
	}
	if b["text"] != b["content"] || strings.Contains(b["text"].(string), "accounts.google.com") {
		t.Errorf("text: %v", b["text"])
	}
	if found, err := f.svc.ByToken(context.Background(), token); err != nil || found.ID != h.ID {
		t.Errorf("ByToken: %v", err)
	}
}

// TestTheLinkFollowsThePolicy: the handoff's own link only for a PIN or
// sign-in handoff, by its policy after Stricter.
func TestTheLinkFollowsThePolicy(t *testing.T) {
	for _, c := range []struct {
		profile, require profiles.HandoffPolicy
		link             bool
	}{
		{profiles.PolicyPIN, "", true},
		{profiles.PolicySignIn, "", true},
		{profiles.PolicyLink, "", false},
		{profiles.PolicyPIN, profiles.PolicySignIn, true},
		{profiles.PolicyLink, profiles.PolicyPIN, true},
		{profiles.PolicySignIn, profiles.PolicyLink, true}, // never lowered
	} {
		t.Run(string(c.profile)+"+"+string(c.require), func(t *testing.T) {
			f := newFixture(t, c.profile, auth.WebOptions{})
			told, _ := f.tell("https://pier.example.com", "https://hooks.example.com/x")
			_, token := f.open(c.require)
			got := told.of(notify.HandoffRequested)
			if len(got) != 1 {
				t.Fatalf("queued %d", len(got))
			}
			want := "https://pier.example.com/profiles/google"
			if c.link {
				want = "https://pier.example.com/h/" + token
			}
			if got[0].body["url"] != want {
				t.Errorf("url = %v, want %s", got[0].body["url"], want)
			}
			if !c.link && strings.Contains(got[0].raw, token) {
				t.Errorf("a link-only body holds the token: %s", got[0].raw)
			}
		})
	}
}

func TestNoAddressForLinksNoURL(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	c, _ := f.tell("", "https://hooks.example.com/x")
	_, token := f.open("")
	got := c.all()
	if len(got) != 1 {
		t.Fatalf("queued %d", len(got))
	}
	if _, ok := got[0].body["url"]; ok || strings.Contains(got[0].raw, token) {
		t.Errorf("a body with no address for links holds a link: %s", got[0].raw)
	}
}

// TestExpiryTellsOnce: a sweep and a Get that both find the handoff
// expired send one event.
func TestExpiryTellsOnce(t *testing.T) {
	for range 5 {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		c, _ := f.tell("https://pier.example.com", "https://hooks.example.com/x")
		h, token := f.open("")
		later := time.Now().Add(Lifetime + time.Second)
		f.svc.now = func() time.Time { return later }
		var wg sync.WaitGroup
		wg.Go(func() { f.svc.sweep(context.Background()) })
		wg.Go(func() { _, _ = f.svc.Get(context.Background(), h.ID) })
		wg.Go(func() { _, _ = f.svc.ByToken(context.Background(), token) })
		wg.Wait()
		_, _ = f.settled(h.ID) // the end they started
		got := c.of(notify.HandoffExpired)
		if len(got) != 1 {
			t.Fatalf("queued %d handoff.expired", len(got))
		}
		if got[0].body["url"] != "https://pier.example.com/profiles/google" || strings.Contains(got[0].raw, token) {
			t.Errorf("expired: %s", got[0].raw)
		}
	}
}

func TestFiveWrongPINsTell(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	c, _ := f.tell("https://pier.example.com", "https://hooks.example.com/x")
	h, token := f.open("")
	for range MaxPINFailures + 2 {
		f.do(browser(), "POST", "/h/"+token+"/pin", url.Values{"pin": {"0000"}}, nil)
	}
	_, _ = f.settled(h.ID) // the fifth PIN starts the end; it runs on its own
	got := c.of(notify.HandoffCancelled)
	if len(got) != 1 || got[0].body["cause"] != "wrong_pins" {
		t.Fatalf("handoff.cancelled: %v", got)
	}
	if len(c.all()) != 2 {
		t.Errorf("queued %d events", len(c.all()))
	}
}

// TestOtherEndsTellNothing: done, can't, until_url, withdrawn and a
// stopped browser send nothing.
func TestOtherEndsTellNothing(t *testing.T) {
	ctx := context.Background()
	for name, end := range map[string]func(f *fixture, h Handoff){
		"done":     func(f *fixture, h Handoff) { _, _ = f.svc.Finish(ctx, h, Done, "all set, see evil.example") },
		"cant":     func(f *fixture, h Handoff) { _, _ = f.svc.Finish(ctx, h, Cant, "no") },
		"until":    func(f *fixture, h Handoff) { _, _ = f.svc.endWith(ctx, h, Done, AutoNote, endAuto, "", "") },
		"withdraw": func(f *fixture, h Handoff) { _, _ = f.svc.Withdraw(ctx, h, "five wrong PINs") },
		"stopped": func(f *fixture, h Handoff) {
			if _, err := f.m.Stop(ctx, h.SessionID); err != nil {
				f.t.Fatal(err)
			}
			f.svc.sweep(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
			c, _ := f.tell("https://pier.example.com", "https://hooks.example.com/x")
			h, _ := f.open("")
			end(f, h)
			got, _ := f.settled(h.ID)
			if !got.Status.Final() {
				t.Fatalf("still %s", got.Status)
			}
			if all := c.all(); len(all) != 1 {
				t.Errorf("queued %d events, want only handoff.requested", len(all))
			}
		})
	}
}

func TestTurnedOffOrNoWebhookTellsNothing(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	c, _ := f.tell("https://pier.example.com", "https://hooks.example.com/x", notify.HandoffCancelled)
	h, _ := f.open("")
	if h.Notified || len(c.all()) != 0 {
		t.Errorf("a handoff.requested turned off is queued")
	}
	f.svc.now = func() time.Time { return time.Now().Add(Lifetime + time.Second) }
	_, _ = f.settled(h.ID)
	if len(c.all()) != 0 {
		t.Error("a handoff.expired turned off is queued")
	}

	g := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	c2, _ := g.tell("https://pier.example.com", "")
	h2, _ := g.open("")
	if h2.Notified || len(c2.all()) != 0 {
		t.Error("queued with no webhook set")
	}
}

// TestAStuckWebhookHoldsNothingUp: with a webhook that never answers and
// its queue full, Open and the end of a handoff return at once.
func TestAStuckWebhookHoldsNothingUp(t *testing.T) {
	stuck := make(chan struct{})
	arrived := make(chan struct{}, notify.Workers)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
		<-stuck
	}))
	defer srv.Close()
	defer close(stuck)
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	_, n := f.tell("https://pier.example.com", srv.URL+"/hook")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	// Every worker stuck in a POST first, then the queue filled behind
	// them: on one CPU the workers might not have taken theirs yet.
	for range notify.Workers {
		n.BrowserFailed("other", "crashed")
	}
	for range notify.Workers {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the workers' POSTs didn't arrive")
		}
	}
	for range notify.QueueSize {
		if !n.BrowserFailed("other", "crashed") {
			t.Fatal("the queue filled early")
		}
	}
	s := f.running()
	start := time.Now()
	h, _, err := f.svc.Create(context.Background(), s.ID, "research-bot", SensitiveInput, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Open took %s", d)
	}
	if h.Notified {
		t.Error("Notified with the queue full")
	}
	f.svc.now = func() time.Time { return time.Now().Add(Lifetime + time.Second) }
	start = time.Now()
	if got, err := f.settled(h.ID); err != nil || got.Status != Expired {
		t.Fatalf("expiry: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ending it took %s", d)
	}
}

// TestADeadHandoffIsntSent: a handoff.requested still queued when its
// browser stops or its time runs out isn't sent, though nothing swept
// the handoff meanwhile; an expiry tells handoff.expired instead.
func TestADeadHandoffIsntSent(t *testing.T) {
	ctx := context.Background()
	for name, kill := range map[string]func(f *fixture, h Handoff){
		"stopped": func(f *fixture, h Handoff) {
			if _, err := f.m.Stop(ctx, h.SessionID); err != nil {
				f.t.Fatal(err)
			}
		},
		"expired": func(f *fixture, _ Handoff) {
			later := time.Now().Add(Lifetime + time.Second)
			f.svc.now = func() time.Time { return later }
		},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var events []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b struct{ Event string }
				_ = json.NewDecoder(r.Body).Decode(&b)
				mu.Lock()
				events = append(events, b.Event)
				mu.Unlock()
			}))
			defer srv.Close()
			f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
			_, n := f.tell("https://pier.example.com", srv.URL+"/hook")
			h, _ := f.open("") // queued; no worker runs yet
			kill(f, h)
			var st Status
			if err := f.svc.db.QueryRowContext(ctx, `SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&st); err != nil || st != Pending {
				t.Fatalf("the row is %s before delivery, %v; the test needs it still pending", st, err)
			}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { n.Run(runCtx); close(done) }()
			defer func() { cancel(); <-done }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if err := f.svc.db.QueryRowContext(ctx, `SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&st); err == nil && st.Final() {
					break
				}
				mu.Lock()
				posted := len(events) > 0
				mu.Unlock()
				if posted {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the worker neither sent the event nor ended the handoff")
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			for _, e := range events {
				if e == string(notify.HandoffRequested) {
					t.Errorf("a dead handoff's link was sent: %v", events)
				}
			}
		})
	}
}
