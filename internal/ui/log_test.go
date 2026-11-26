package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/recording"
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

// While the live session's recording covers an entry, the entry links to
// its moment in the player, on the side column and the log page (PROJ-23);
// a kept run's entries don't.
func TestLogEntriesLinkToTheRecording(t *testing.T) {
	f := newFixture(t)
	f.profile("google")
	s, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	f.m.Note(s.ID, "claude", "Clicked at 1, 2")
	dir := filepath.Join(f.m.Layout().DataDir, "recordings", "google", s.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	ix := recording.Index{Profile: "google", Session: s.ID,
		Segments: []recording.Segment{{File: "000001.mp4", Start: at, End: at.Add(2 * time.Hour)}}}
	b, _ := json.Marshal(ix)
	for name, body := range map[string]string{"index.json": string(b), "000001.mp4": "ftypMP4-one"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.recordings.Reconcile([]string{"google"}); err != nil {
		t.Fatal(err)
	}
	link := "/profiles/google/recordings/" + s.ID + "?t="
	want(t, f.get("/profiles/google/side"), http.StatusOK, link, "Play the recording from here")
	want(t, f.get("/profiles/google/log"), http.StatusOK, link, "Play the recording from here")
	if _, err := f.m.StopFor(context.Background(), s.ID, sessions.StopFromApp); err != nil {
		t.Fatal(err)
	}
	r := f.get("/profiles/google/log")
	want(t, r, http.StatusOK, "Clicked at 1, 2")
	if strings.Contains(r.body, link) {
		t.Fatal("a kept run's entry links to the player")
	}

	// A new run whose recording covers the kept run's moments: only its
	// own entries link.
	time.Sleep(5 * time.Millisecond) // the runs' entries are a few ms apart
	s2, _, err := f.m.Start(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	f.m.Note(s2.ID, "claude", "Clicked at 3, 4")
	dir2 := filepath.Join(f.m.Layout().DataDir, "recordings", "google", s2.ID)
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	ix.Session = s2.ID
	b, _ = json.Marshal(ix)
	for name, body := range map[string]string{"index.json": string(b), "000001.mp4": "ftypMP4-one"} {
		if err := os.WriteFile(filepath.Join(dir2, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.recordings.Reconcile([]string{"google"}); err != nil {
		t.Fatal(err)
	}
	r = f.get("/profiles/google/log")
	link2 := "/profiles/google/recordings/" + s2.ID + "?t="
	current := f.m.Activity(s2.ID)
	for _, a := range current {
		want(t, r, http.StatusOK, link2+strconv.FormatInt(a.At.UnixMilli(), 10))
	}
	for _, a := range f.m.Runs("google")[0].Entries {
		if l := link2 + strconv.FormatInt(a.At.UnixMilli(), 10); strings.Contains(r.body, l) {
			t.Fatalf("the kept entry %q links to the current recording", a.What)
		}
	}
	if n := strings.Count(r.body, "Play the recording from here"); n != len(current) {
		t.Fatalf("%d links for %d current entries", n, len(current))
	}
}

// An entry in a gap between a recording's segments (a person had
// control) has no link; one in a segment does.
func TestRecordingAtSkipsGaps(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ix := &recording.Index{Session: "0123456789abcdef01234567", Segments: []recording.Segment{
		{File: "000001.mp4", Start: t0, End: t0.Add(time.Minute)},
		{File: "000002.mp4", Start: t0.Add(5 * time.Minute), End: t0.Add(6 * time.Minute)},
	}}
	for at, linked := range map[time.Time]bool{
		t0.Add(-2 * time.Second):            false,
		t0.Add(-500 * time.Millisecond):     true, // written just before
		t0.Add(30 * time.Second):            true,
		t0.Add(2 * time.Minute):             false, // the gap
		t0.Add(5*time.Minute - time.Hour):   false,
		t0.Add(5*time.Minute + time.Second): true,
		t0.Add(7 * time.Minute):             false,
	} {
		if got := recordingAt("google", ix, at) != ""; got != linked {
			t.Errorf("at %s: linked %t", at.Sub(t0), got)
		}
	}
}
