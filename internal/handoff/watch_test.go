package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// fakeTabs is what the fake browser's /json/list answers (the tab in
// front first), and how many times it was asked.
type fakeTabs struct {
	mu    sync.Mutex
	tabs  []tab
	polls int
}

func (f *fakeTabs) set(tabs ...tab) {
	f.mu.Lock()
	f.tabs = tabs
	f.mu.Unlock()
}

func (f *fakeTabs) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// serveList answers /json/list, a service worker listed before the pages.
func (f *fakeTabs) serveList(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.polls++
	list := append([]tab{{ID: "W1", Typ: "service_worker", URL: "https://site.example/account/sw.js"}}, f.tabs...)
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(list)
}

func (f *fixture) openUntil(until string) (Handoff, string) {
	f.t.Helper()
	s := f.running()
	h, token, err := f.svc.Open(context.Background(), Request{SessionID: s.ID, Agent: "research-bot", Reason: SensitiveInput,
		Detail: "Sign in", UntilURL: until})
	if err != nil {
		f.t.Fatal(err)
	}
	return h, token
}

// watched returns the handoff's watch, or nil.
func (f *fixture) watched(id string) *watch {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	return f.svc.watching[id]
}

// watchEnds fails unless the watch w has ended, and forgotten, within 2 s.
func (f *fixture) watchEnds(id string, w *watch) {
	f.t.Helper()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		f.t.Fatal("the watch is still running")
	}
	if f.watched(id) != nil {
		f.t.Fatal("the watch ended but is still listed")
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestCheckUntilURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"   ":                    "",
		"/account":               "/account",
		"  example.com/account ": "example.com/account",
		"/café":                  "/café",
		"/acc":                   "/acc",
		"x.io":                   "x.io",
		"https://x.io":           "https://x.io",
		"httpbin.org":            "httpbin.org",
		strings.Repeat("a", 200): strings.Repeat("a", 200),
		strings.Repeat("é", 200): strings.Repeat("é", 200), // characters, not bytes
	} {
		if got, err := CheckUntilURL(in); err != nil || got != want {
			t.Errorf("CheckUntilURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		strings.Repeat("a", 201),
		"/acc\nount", "/acc\tount", "/acc\x00ount", "/acc\x7fount", "/acc\u0085ount",
		"/acc\u202eount", "/acc\u200bount", "/acc\u2028ount", // bidi, zero-width, line separator
		"/acc\xffount",                  // not UTF-8
		"/cb?code=", "/account#welcome", // never matched, so refused
		// Too general: they would match nearly any address.
		"/", ".", "/a", "abc", "///", "/./", "....", ":/:/", "https://", "http://", "HTTPS://", "https", "http:",
		"https:///", "  http://  ",
	} {
		if got, err := CheckUntilURL(in); !errors.Is(err, ErrBadUntilURL) {
			t.Errorf("CheckUntilURL(%q) = %q, %v; want ErrBadUntilURL", in, got, err)
		}
	}
}

func TestMatchesOriginAndPathOnly(t *testing.T) {
	for _, c := range []struct {
		pattern, url string
		want         bool
	}{
		{"code=", "https://x/cb?code=1", false}, // only in the query
		{"/cb", "https://x/cb?code=1", true},
		{"x/cb", "https://x/cb?code=1", true},
		{"https://x/cb", "https://x/cb", true},
		{"welcome", "https://x/account#welcome", false},  // only in the fragment
		{"secret", "https://me:secret@x/account", false}, // only in the user info
		{"x:8443/account", "https://x:8443/account/settings", true},
		{"/café", "https://x/caf%C3%A9", true},
		{"/caf%C3%A9", "https://x/caf%C3%A9", true},
		{"/Account", "https://x/account", false}, // the path as it is
		{"blank", "about:blank", false},          // http and https pages only
		{"newtab", "chrome://newtab/", false},
		{"chromewebdata", "chrome-error://chromewebdata/", false},
		{"", "https://x/", false},
	} {
		if got := matches(c.pattern, c.url); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.pattern, c.url, got, c.want)
		}
	}
}

func TestLookCountsOnlyTheFrontTabOnceItMoved(t *testing.T) {
	const until = "/account"
	login := tab{ID: "A", Typ: "page", URL: "https://site/login?next=/account"}
	seen, hit := look(nil, []tab{{ID: "B", Typ: "page", URL: "https://site/account"}, login}, until)
	if hit {
		t.Fatal("the first look matched a tab already at the address")
	}
	// Still there, reloaded, or with another query or fragment: not moved.
	for _, b := range []string{"https://site/account", "https://site/account?tab=2", "https://site/account#top"} {
		if seen, hit = look(seen, []tab{{ID: "B", Typ: "page", URL: b}, login}, until); hit {
			t.Fatalf("a tab that stayed at the address matched: %s", b)
		}
	}
	// A navigation to it does, in front.
	if _, hit = look(seen, []tab{{ID: "A", Typ: "page", URL: "https://site/account"}, {ID: "B", Typ: "page", URL: "https://site/account"}}, until); !hit {
		t.Fatal("the front tab that navigated to the address didn't match")
	}
	// Not in the background: a tab (or popup) behind the person's.
	back, hit := look(seen, []tab{{ID: "B", Typ: "page", URL: "https://site/account"}, {ID: "A", Typ: "page", URL: "https://site/account"}}, until)
	if hit {
		t.Fatal("a background tab that navigated to the address matched")
	}
	// Once it comes to the front, having moved, it does (an opener a
	// sign-in popup sent on, then closed).
	if _, hit = look(back, []tab{{ID: "A", Typ: "page", URL: "https://site/account"}, {ID: "B", Typ: "page", URL: "https://site/account"}}, until); !hit {
		t.Fatal("a tab that moved in the background didn't match once in front")
	}
	// So does one that went elsewhere first, then got there.
	s2, _ := look(seen, []tab{{ID: "A", Typ: "page", URL: "https://site/2fa"}}, until)
	if _, hit = look(s2, []tab{{ID: "A", Typ: "page", URL: "https://site/account?welcome=1"}}, until); !hit {
		t.Fatal("a tab that moved twice didn't match")
	}
	// And a tab opened since the first look, in front; not behind.
	if _, hit = look(seen, []tab{{ID: "C", Typ: "page", URL: "https://site/account"}, login}, until); !hit {
		t.Fatal("a new tab in front at the address didn't match")
	}
	if _, hit = look(seen, []tab{login, {ID: "C", Typ: "page", URL: "https://site/account"}}, until); hit {
		t.Fatal("a new background tab at the address matched")
	}
	// A first look that saw no tab at all still counts as a look.
	empty, _ := look(nil, nil, until)
	if _, hit = look(empty, []tab{{ID: "D", Typ: "page", URL: "https://site/account"}}, until); !hit {
		t.Fatal("a tab opened after an empty first look didn't match")
	}
}

// The watch looks only once the person has opened the link: its first look
// is then, so nothing before (the page, the agent) can end a handoff the
// person hasn't seen.
func TestTheWatchStartsWhenTheLinkIsOpened(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	f.svc.watchPoll = 20 * time.Millisecond
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/login"})
	h, token := f.openUntil("/account")
	n := f.tabs.count() // the origin, read as it opened
	time.Sleep(10 * f.svc.watchPoll)
	if f.tabs.count() != n {
		t.Fatal("the watch looked at the tabs before the link was opened")
	}
	// The page gets to the address before anyone opened the link.
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/account"})
	time.Sleep(10 * f.svc.watchPoll)
	if got, _ := f.svc.Get(ctx, h.ID); got.Status != Pending {
		t.Fatalf("before the link was opened: %s", got.Status)
	}
	// The person opens it: the first look is there, already at the address.
	if p := f.do(browser(), "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther {
		t.Fatalf("opening the link: %d", p.code)
	}
	eventually(t, "a look", func() bool { return f.tabs.count() > n+1 })
	time.Sleep(5 * f.svc.watchPoll)
	if got, _ := f.svc.Get(ctx, h.ID); got.Status != Pending {
		t.Fatalf("a page already at the address when the link was opened ended it: %s", got.Status)
	}
	// Moving away and back, in front, does.
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/2fa"})
	time.Sleep(5 * f.svc.watchPoll)
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/account"})
	if got, err := f.svc.Wait(ctx, h.ID, 5*time.Second); err != nil || got.Status != Done || !got.Auto {
		t.Fatalf("after the person got there: %+v, %v", got, err)
	}
}

// TestUntilURLEndsTheHandoff is the fake browser's check: /json/list
// answers a new address after two looks, and the handoff ends as done with
// the fixed note within a second, with no CDP connection at all, and its
// watch is gone.
func TestUntilURLEndsTheHandoff(t *testing.T) {
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	ctx := context.Background()
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/login"})
	h, token := f.openUntil("site.example/account")
	if h.UntilURL != "site.example/account" {
		t.Fatalf("UntilURL %q", h.UntilURL)
	}
	w := f.watched(h.ID)
	if w == nil {
		t.Fatal("no watch for a handoff with an until_url")
	}
	// The person opens the link, as they would.
	b := browser()
	if p := f.do(b, "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther {
		t.Fatalf("opening the link: %d", p.code)
	}
	eventually(t, "two looks", func() bool { return f.tabs.count() >= 2 })
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/account?session=s3cr3t#done"})
	changed := time.Now()
	got, err := f.svc.Wait(ctx, h.ID, 5*time.Second)
	took := time.Since(changed)
	if err != nil || got.Status != Done || got.Note != AutoNote || !got.Auto {
		t.Fatalf("after the page reached the address: %+v, %v", got, err)
	}
	if took > time.Second {
		t.Fatalf("the handoff ended %s after the change, want within 1 s", took)
	}
	if n := f.cdp.Dials(); n != 0 {
		t.Fatalf("the watch made %d CDP connections; it reads /json/list over HTTP only", n)
	}
	f.watchEnds(h.ID, w)
	if live, _ := f.m.Live("google"); live.State != sessions.Running {
		t.Fatalf("the browser wasn't given back: %s", live.State)
	}
	// The activity log says how, and never where.
	var notes []string
	for _, a := range f.m.Activity(h.SessionID) {
		notes = append(notes, a.What)
	}
	all := strings.Join(notes, "\n")
	if !strings.Contains(all, "Handed back when the page reached the expected address") || strings.Contains(all, "site.example") || strings.Contains(all, "s3cr3t") {
		t.Fatalf("activity: %q", notes)
	}
	// Nor the request's words, an address or the link (PROJ-158).
	for _, a := range f.m.Activity(h.SessionID) {
		if strings.Contains(a.What+a.Title, h.Detail) || strings.Contains(a.What+a.Title, h.UntilURL) || strings.Contains(a.What+a.Title, token) {
			t.Fatalf("activity: %+v", a)
		}
	}
	// The phone's page says it was handed back by itself.
	p := f.do(b, "GET", "/h/"+token, nil, nil)
	if p.code != http.StatusGone || !strings.Contains(p.body, "Done: handed back automatically") || strings.Contains(p.body, "s3cr3t") {
		t.Fatalf("the phone's page after: %d %s", p.code, p.body)
	}
	// Polling stopped with it.
	n := f.tabs.count()
	time.Sleep(3 * f.svc.watchPoll)
	if f.tabs.count() != n {
		t.Fatal("the browser is still being asked for its tabs")
	}
}

func TestNoUntilURLNoWatch(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	h, _ := f.open("")
	n := f.tabs.count() // the origin, read as it opened
	time.Sleep(3 * f.svc.watchPoll)
	if f.watched(h.ID) != nil || f.tabs.count() != n || f.cdp.Dials() != 0 {
		t.Fatalf("a handoff without until_url is watched: %d looks, %d dials", f.tabs.count()-n, f.cdp.Dials())
	}
}

func TestOpenRefusesABadUntilURL(t *testing.T) {
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	s := f.running()
	_, _, err := f.svc.Open(context.Background(), Request{SessionID: s.ID, Agent: "a", Reason: Captcha, UntilURL: "/cb?code="})
	if !errors.Is(err, ErrBadUntilURL) {
		t.Fatalf("Open = %v", err)
	}
	if live, _ := f.m.Live("google"); live.State != sessions.Running {
		t.Fatalf("a refused handoff paused the agent: %s", live.State)
	}
}

func TestAMatchAfterTheEndDoesNothing(t *testing.T) {
	ctx := context.Background()
	t.Run("expired", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		var skew atomic.Int64
		f.svc.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
		f.svc.watchPoll = time.Hour // only the first look, so the test drives it
		h, _ := f.openUntil("/account")
		skew.Store(int64(Lifetime + time.Second))
		f.svc.reached(ctx, h)
		if got, _ := f.settled(h.ID); got.Status != Expired || got.Auto || got.Note != "" {
			t.Fatalf("a match after expiry: %+v", got)
		}
	})
	t.Run("expired, not yet marked", func(t *testing.T) {
		// The database's own check: end refuses a handoff past its time.
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		var skew atomic.Int64
		f.svc.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
		f.svc.watchPoll = time.Hour
		h, _ := f.openUntil("/account")
		skew.Store(int64(Lifetime + time.Second))
		if _, err := f.svc.endWith(ctx, h, Done, AutoNote, endAuto, "", ""); !errors.Is(err, ErrInUse) {
			t.Fatalf("end after expiry = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, _ := f.openUntil("/account")
		if _, err := f.svc.Withdraw(ctx, h, "the agent withdrew it"); err != nil {
			t.Fatal(err)
		}
		f.svc.reached(ctx, h)
		if got, _ := f.svc.Get(ctx, h.ID); got.Status != Cancelled || got.Auto || got.Note != "the agent withdrew it" {
			t.Fatalf("a match after a cancel: %+v", got)
		}
	})
	t.Run("person's can't", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, _ := f.openUntil("/account")
		if _, err := f.svc.Finish(ctx, h, Cant, "no phone"); err != nil {
			t.Fatal(err)
		}
		f.svc.reached(ctx, h)
		if got, _ := f.svc.Get(ctx, h.ID); got.Status != Cant || got.Auto || got.Note != "no phone" {
			t.Fatalf("a match after can't: %+v", got)
		}
	})
}

// TestATapAndAMatchEndItOnce: a person tapping Done as the page reaches
// the address ends the handoff once, one way or the other.
func TestATapAndAMatchEndItOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
	f.svc.watchPoll = time.Hour
	for range 20 {
		h, _ := f.openUntil("/account")
		waited := make(chan Handoff, 1)
		go func() {
			got, _ := f.svc.Wait(ctx, h.ID, 10*time.Second)
			waited <- got
		}()
		before := handedBack(f.m.Activity(h.SessionID))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = f.svc.Finish(ctx, h, Done, "by hand") }()
		go func() { defer wg.Done(); f.svc.reached(ctx, h) }()
		wg.Wait()
		got, err := f.svc.Get(ctx, h.ID)
		if err != nil || got.Status != Done || (got.Auto != (got.Note == AutoNote)) || (!got.Auto && got.Note != "by hand") {
			t.Fatalf("after a tap and a match: %+v, %v", got, err)
		}
		if ended := handedBack(f.m.Activity(h.SessionID)) - before; ended != 1 {
			t.Fatalf("the handoff ended %d times", ended)
		}
		if w := <-waited; w.Status != Done {
			t.Fatalf("the agent heard %s", w.Status)
		}
		if live, _ := f.m.Live("google"); live.State != sessions.Running {
			t.Fatalf("browser not given back: %s", live.State)
		}
	}
}

// handedBack counts the activity entries that say a handoff ended done.
func handedBack(log []sessions.Activity) int {
	n := 0
	for _, a := range log {
		if strings.HasPrefix(a.What, "Handed back") {
			n++
		}
	}
	return n
}

// The watch ends with the handoff, however it ends: one goroutine per
// open handoff, none left after.
func TestTheWatchEndsWithTheHandoff(t *testing.T) {
	ctx := context.Background()
	t.Run("tapped", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, _ := f.openUntil("/account")
		w := f.watched(h.ID)
		if _, err := f.svc.Finish(ctx, h, Done, ""); err != nil {
			t.Fatal(err)
		}
		f.watchEnds(h.ID, w)
	})
	t.Run("withdrawn", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, _ := f.openUntil("/account")
		w := f.watched(h.ID)
		if _, err := f.svc.Withdraw(ctx, h, "no"); err != nil {
			t.Fatal(err)
		}
		f.watchEnds(h.ID, w)
	})
	t.Run("expired", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		var skew atomic.Int64
		f.svc.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
		h, _ := f.openUntil("/account")
		w := f.watched(h.ID)
		skew.Store(int64(Lifetime + time.Second))
		f.watchEnds(h.ID, w)
		if got, _ := f.settled(h.ID); got.Status != Expired {
			t.Fatalf("after its time: %s", got.Status)
		}
	})
	t.Run("browser stopped", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, _ := f.openUntil("/account")
		w := f.watched(h.ID)
		if _, err := f.m.Stop(ctx, h.SessionID); err != nil {
			t.Fatal(err)
		}
		f.watchEnds(h.ID, w)
		_, _ = f.settled(h.ID) // the end the watch started
		var status Status
		if err := f.svc.db.QueryRow(`SELECT status FROM handoffs WHERE id = ?`, h.ID).Scan(&status); err != nil || status != Cancelled {
			t.Fatalf("after the browser stopped: %s, %v", status, err)
		}
	})
	t.Run("closed", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		h, _ := f.openUntil("/account")
		w := f.watched(h.ID)
		f.svc.Close()
		f.watchEnds(h.ID, w)
		// Closing leaves the handoff open, and no new watch starts.
		if got, _ := f.svc.Get(ctx, h.ID); got.Status != Pending {
			t.Fatalf("after Close: %s", got.Status)
		}
		f.svc.sweep(ctx)
		if f.watched(h.ID) != nil {
			t.Fatal("a closed service started a watch")
		}
	})
	t.Run("many", func(t *testing.T) {
		f := newFixture(t, profiles.PolicyPIN, auth.WebOptions{})
		for range 5 {
			h, _ := f.openUntil("/account")
			w := f.watched(h.ID)
			if _, err := f.svc.Finish(ctx, h, Cant, ""); err != nil {
				t.Fatal(err)
			}
			f.watchEnds(h.ID, w)
		}
		f.svc.mu.Lock()
		n := len(f.svc.watching)
		f.svc.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d watches left", n)
		}
	})
}

// After a restart, the sweep watches an open handoff again; its first look
// is where the tabs are then.
func TestSweepWatchesAgainAfterARestart(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, profiles.PolicyLink, auth.WebOptions{})
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/login"})
	h, token := f.openUntil("/account")
	if p := f.do(browser(), "GET", "/h/"+token, nil, nil); p.code != http.StatusSeeOther {
		t.Fatalf("opening the link: %d", p.code)
	}
	f.svc.Close()
	again := New(f.svc.db, f.m, f.svc.log)
	t.Cleanup(again.Close)
	again.sweep(ctx) // each handoff on its own
	eventually(t, "the sweep watching the open handoff again", func() bool {
		again.mu.Lock()
		defer again.mu.Unlock()
		return again.watching[h.ID] != nil
	})
	n := f.tabs.count()
	eventually(t, "a look", func() bool { return f.tabs.count() > n })
	f.tabs.set(tab{ID: "T1", Typ: "page", URL: "https://site.example/account"})
	if got, err := again.Wait(ctx, h.ID, 5*time.Second); err != nil || got.Status != Done || !got.Auto {
		t.Fatalf("after the restart: %+v, %v", got, err)
	}
}
