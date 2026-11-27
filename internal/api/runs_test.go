package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// recent_calls holds the token's own actions only: no title, and none of
// BrowserPier's entries (PROJ-158).
func TestRecentCallsHoldNoneOfBrowserPiersEntries(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.send(f.srv, f.all, s.ID, "click", "c1", `{"x": 1, "y": 2}`)
	const title = "Private page title"
	f.m.NoteTitle(s.ID, title)
	release, err := f.m.Watch(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	// A handoff, as the handoff service runs one.
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	epoch, _ := f.m.HelpEpoch(s.ID)
	if _, err := f.m.TakeControlTurnByLink(s.ID, sessions.HandoffHolder("h1"), epoch); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	f.m.NoteHandoff(s.ID, "Handed back: done")
	var st sessionStatusJSON
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st); code != http.StatusOK {
		t.Fatalf("session: %d", code)
	}
	var whats []string
	for _, c := range st.RecentCalls {
		whats = append(whats, c.What)
	}
	if got := strings.Join(whats, "|"); got != "Clicked at 1, 2|Started the browser" {
		t.Fatalf("recent_calls: %s", got)
	}
	kinds := map[sessions.Kind]bool{}
	for _, a := range f.m.Activity(s.ID) {
		kinds[a.Kind] = true
	}
	for _, k := range []sessions.Kind{sessions.KindAction, sessions.KindPage, sessions.KindWatch, sessions.KindControl, sessions.KindHandoff} {
		if !kinds[k] {
			t.Errorf("the admin's log has no %s entry", k)
		}
	}
}

// An agent's stop is kept with its reason and who started the browser.
func TestAgentStopIsKept(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.send(f.srv, f.all, s.ID, "click", "", `{"x": 1, "y": 2}`)
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	runs := f.m.Runs("google")
	if len(runs) != 1 {
		t.Fatalf("kept %+v", runs)
	}
	r := runs[0]
	var got []string
	for i := len(r.Entries) - 1; i >= 0; i-- {
		got = append(got, r.Entries[i].What)
	}
	if r.End != sessions.EndAgent || r.StartedBy != "claude" || r.FromApp ||
		strings.Join(got, "|") != "Started the browser|Clicked at 1, 2|Stopping the browser|Stopped by claude" {
		t.Fatalf("kept %+v: %q", r, got)
	}
	// Nothing an agent reads names it: the stopped session answers as before.
	var st sessionStatusJSON
	if code := f.call(f.all, http.MethodGet, "/api/sessions/"+s.ID, "", &st); code != http.StatusOK || len(st.RecentCalls) != 0 {
		t.Fatalf("a stopped session: %d %+v", code, st.RecentCalls)
	}
}
