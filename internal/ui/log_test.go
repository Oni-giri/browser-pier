package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// stopRun starts google's browser as agent, runs what, and stops it.
func (f *fixture) stopRun(agent string, what func(id string)) sessions.Session {
	f.t.Helper()
	ctx := context.Background()
	s, _, err := f.m.StartWith(ctx, "google", sessions.StartOptions{By: agent})
	if err != nil {
		f.t.Fatal(err)
	}
	what(s.ID)
	if _, err := f.m.StopFor(ctx, s.ID, sessions.StopByAgent(agent)); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestStoppedProfileShowsHowItsLastRunEnded(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	want(t, f.get("/profiles/google"), http.StatusOK, "Starts when the browser does.")
	// Started and stopped from the app.
	if r := f.post("/profiles/google/start", url.Values{}); r.code != http.StatusSeeOther {
		t.Fatalf("start: %d", r.code)
	}
	if r := f.post("/profiles/google/stop", url.Values{}); r.code != http.StatusSeeOther {
		t.Fatalf("stop: %d", r.code)
	}
	runs := f.m.Runs("google")
	if len(runs) != 1 || !runs[0].FromApp || runs[0].End != sessions.EndAdmin {
		t.Fatalf("kept %+v", runs)
	}
	page := f.get("/profiles/google")
	want(t, page, http.StatusOK, "Last run ended", "Stopped from the app", "/profiles/google/log#run-"+runs[0].SessionID)
	if strings.Contains(page.body, "Starts when the browser does.") {
		t.Fatal("the page still says the log starts with the browser")
	}
	log := f.get("/profiles/google/log")
	want(t, log, http.StatusOK, "Previous runs", "Started from the app", "Stopped from the app")
}

func TestLogPageListsRunsNewestFirst(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	first := f.stopRun("claude", func(id string) { f.m.Note(id, "claude", "Clicked at 1, 2") })
	second := f.stopRun("helper", func(id string) {
		f.m.Note(id, "helper", "Typed 4 characters")
		f.m.NoteTitle(id, "Second page")
	})
	r := f.get("/profiles/google/log")
	want(t, r, http.StatusOK, keptSentence, "Clicked at 1, 2", "Typed 4 characters", "Second page", "Started by claude", "Stopped by helper",
		`id="run-`+first.ID+`"`, `id="run-`+second.ID+`"`)
	if strings.Index(r.body, second.ID) > strings.Index(r.body, first.ID) {
		t.Fatal("the older run comes first")
	}
	if keptSentence != "Kept in memory for 24 hours, the last 5 runs. A BrowserPier restart or update clears it." {
		t.Fatalf("the sentence says %q", keptSentence)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", r.header.Get("Cache-Control"))
	}
	// The current run is on it too, all of it.
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		f.m.Note(s.ID, "claude", fmt.Sprintf("Step %d", i))
	}
	r = f.get("/profiles/google/log")
	want(t, r, http.StatusOK, "Current run", "Step 0<", "Step 59<", "Previous runs")
	if got := f.get("/profiles/nope/log"); got.code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", got.code)
	}
}

// A title and an agent's name are text: escaped, wherever they show.
func TestLogEntriesAreEscaped(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	const script = "<script>alert(1)</script>"
	check := func(where string, body string) {
		t.Helper()
		if strings.Contains(body, script) || strings.Contains(body, "<b>:") || strings.Contains(body, "by <b>") {
			t.Errorf("%s has a raw tag", where)
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(body, "&lt;b&gt;") {
			t.Errorf("%s doesn't show them escaped", where)
		}
	}
	s, _, err := f.m.StartWith(context.Background(), "google", sessions.StartOptions{By: "<b>"})
	if err != nil {
		t.Fatal(err)
	}
	f.m.Note(s.ID, "<b>", "Clicked at 1, 2")
	f.m.NoteTitle(s.ID, script)
	check("the side column", f.get("/profiles/google/side").body)
	check("the live log page", f.get("/profiles/google/log").body)
	if _, err := f.m.StopFor(context.Background(), s.ID, sessions.StopByAgent("<b>")); err != nil {
		t.Fatal(err)
	}
	check("the kept run", f.get("/profiles/google/log").body)
}

func TestLogPageIsTheAdminsOnly(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	_, token, err := f.tokens.Create(context.Background(), "research-bot", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, header := range map[string][]string{"signed out": nil, "agent token": {"Authorization", "Bearer " + token}} {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/profiles/google/log", nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

// The side column stays the current run's newest 50 entries, however
// long its log.
func TestSideShowsFiftyEntries(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		f.m.Note(s.ID, "claude", fmt.Sprintf("Clicked at %d, 1", i))
	}
	r := f.get("/profiles/google/side")
	if n := strings.Count(r.body, "<time datetime="); n != sideLog || !strings.Contains(r.body, "Clicked at 499, 1") || strings.Contains(r.body, "Clicked at 449, 1<") {
		t.Fatalf("the side column has %d entries", n)
	}
	want(t, r, http.StatusOK, "All entries →")
	t.Logf("side fragment: %d bytes", len(r.body))
}
