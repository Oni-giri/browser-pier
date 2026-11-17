package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// runs reads the profile's kept runs and fails unless there are n.
func (f *fixture) runs(profile string, n int) []Run {
	f.t.Helper()
	got := f.m.Runs(profile)
	if len(got) != n {
		f.t.Fatalf("%d kept runs of %s, want %d: %+v", len(got), profile, n, got)
	}
	return got
}

// whats lists entries' words, oldest first.
func whats(entries []Activity) []string {
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i].What)
	}
	return out
}

func TestRunIsKeptAfterStop(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s, _, err := f.m.StartWith(context.Background(), "google", StartOptions{Mode: profiles.ModeAPI, By: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	f.m.Note(s.ID, "claude", "Clicked at 1, 2")
	advance(time.Minute)
	f.m.NoteFor(context.Background(), s.ID, "claude", "Typed 5 characters")
	advance(time.Minute)
	if _, err := f.m.StopFor(context.Background(), s.ID, StopByAgent("claude")); err != nil {
		t.Fatal(err)
	}
	r := f.runs("google", 1)[0]
	if r.SessionID != s.ID || r.Mode != profiles.ModeAPI || !r.Started.Equal(s.CreatedAt) || !r.Ended.Equal(s.CreatedAt.Add(2*time.Minute)) ||
		r.End != EndAgent || r.EndText != "Stopped by claude" || r.StartedBy != "claude" || r.FromApp {
		t.Fatalf("kept run %+v", r)
	}
	if got := strings.Join(whats(r.Entries), "|"); got != "Clicked at 1, 2|Typed 5 characters|Stopped by claude" {
		t.Fatalf("entries: %s", got)
	}
	if r.Entries[0].Kind != KindSession || r.Entries[1].Kind != KindAction || r.Entries[1].Agent != "claude" {
		t.Fatalf("kinds: %+v", r.Entries)
	}
	// A copy.
	r.Entries[0].What = "changed"
	if f.m.Runs("google")[0].Entries[0].What == "changed" {
		t.Fatal("Runs returned the kept run itself")
	}
	// The live log still goes with the session, and the next run starts
	// empty; the kept run stays.
	if got := f.m.Activity(s.ID); got != nil {
		t.Fatalf("a stopped session has a live log: %+v", got)
	}
	s2 := f.start(f.m)
	if got := f.m.Activity(s2.ID); len(got) != 0 {
		t.Fatalf("the next run inherited %+v", got)
	}
	if got := f.runs("google", 1)[0]; got.SessionID != s.ID {
		t.Fatalf("kept %+v", got)
	}
}

func TestKeptRunsBounds(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	ctx := context.Background()
	var ids []string
	for i := range 6 {
		s := f.start(f.m)
		ids = append(ids, s.ID)
		f.m.Note(s.ID, "claude", fmt.Sprintf("run %d", i))
		if _, err := f.m.Stop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		advance(time.Minute)
	}
	runs := f.runs("google", keptRuns)
	for i, r := range runs {
		if want := ids[5-i]; r.SessionID != want {
			t.Fatalf("run %d is %s, want %s (the newest five, newest first)", i, r.SessionID, want)
		}
	}
	// A day after it ended, a run is gone.
	advance(keptFor - 5*time.Minute)
	f.runs("google", 4)
	advance(5 * time.Minute)
	f.runs("google", 0)

	// One run keeps its newest 500 entries...
	s := f.start(f.m)
	for i := range 600 {
		f.m.Note(s.ID, "claude", fmt.Sprintf("step %d", i))
	}
	if got := f.m.Activity(s.ID); len(got) != maxActivity || got[0].What != "step 599" || got[len(got)-1].What != "step 100" {
		t.Fatalf("live: %d entries, %q to %q", len(got), got[len(got)-1].What, got[0].What)
	}
	// ...and at most 64 KiB of them, the oldest dropped first; an entry
	// is cut to 400 characters.
	long := strings.Repeat("x", 1000)
	for range 200 {
		f.m.Note(s.ID, "claude", long)
	}
	f.m.Note(s.ID, "claude", "last")
	live := f.m.Activity(s.ID)
	if n := utf8.RuneCountInString(live[1].What); n != maxEntryRunes || !strings.HasSuffix(live[1].What, "…") {
		t.Fatalf("a 1,000-character entry has %d", n)
	}
	size := 0
	for _, a := range live {
		size += entrySize(a)
	}
	if size > maxActivityBytes || size < maxActivityBytes-entrySize(live[1]) || live[0].What != "last" {
		t.Fatalf("live log of %d entries is %d bytes", len(live), size)
	}
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	r := f.runs("google", 1)[0]
	size = 0
	for _, a := range r.Entries {
		size += entrySize(a)
	}
	if size > maxActivityBytes || r.Entries[1].What != "last" || r.Entries[0].What != "Stopped from the app" {
		t.Fatalf("kept %d entries (%d bytes), %q, %q", len(r.Entries), size, r.Entries[0].What, r.Entries[1].What)
	}

	// All kept runs together: at most 8 MiB, the oldest run dropped
	// first, whatever its profile.
	f.m.mu.Lock()
	f.m.kept = map[string][]*keptRun{}
	big := maxActivityBytes
	n := maxKeptBytes / big
	for i := range n + 2 {
		p := fmt.Sprintf("p%03d", i/keptRuns)
		f.m.keepLocked(p, &keptRun{Run: Run{SessionID: fmt.Sprint(i), Ended: f.m.now().Add(time.Duration(i) * time.Second)}, bytes: big})
	}
	total, count := 0, 0
	oldest := ""
	for _, runs := range f.m.kept {
		for _, r := range runs {
			total += r.bytes
			count++
			if r.SessionID == "0" || r.SessionID == "1" {
				oldest += r.SessionID
			}
		}
	}
	f.m.mu.Unlock()
	if total > maxKeptBytes || count != n || oldest != "" {
		t.Fatalf("%d runs of %d bytes kept in all (%d), the oldest left: %q", count, total, maxKeptBytes, oldest)
	}
}

func TestFailedStartsDontCrowdRuns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var real []string
	for range 4 {
		s := f.start(f.m)
		real = append(real, s.ID)
		if _, err := f.m.Stop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
	}
	status := 1
	f.rt.ExitOnStart = &status
	for range 5 {
		if _, _, err := f.m.Start(ctx, "google", ""); !errors.Is(err, ErrStartFailed) {
			t.Fatalf("start: %v", err)
		}
	}
	runs := f.runs("google", 5)
	if runs[0].End != EndStartFailed || runs[0].EndText != "The browser didn't start" {
		t.Fatalf("newest: %+v", runs[0])
	}
	for i, r := range runs[1:] {
		if r.SessionID != real[3-i] {
			t.Fatalf("the real runs were pushed out: %+v", runs)
		}
	}
}

// encodings are the ways a page may show a value, each of which the
// scrubber covers.
func encodings(v string) []string {
	j, _ := json.Marshal(v)
	return []string{v, url.QueryEscape(v), url.PathEscape(v), strings.Trim(string(j), `"`), html.EscapeString(v), url.QueryEscape(url.QueryEscape(v))}
}

// noteEverywhere writes v, in every encoding, into an entry and a title.
func (f *fixture) noteEverywhere(id, v string) {
	f.m.Note(id, "claude", "Downloaded "+strings.Join(encodings(v)[:3], " ")+".csv")
	f.m.Note(id, "claude", "Downloaded "+strings.Join(encodings(v)[3:], " ")+".csv")
	f.m.NoteTitle(id, "Welcome "+v+" | Inbox")
}

// hidesEverywhere fails if any encoding of v is in entries.
func hidesEverywhere(t *testing.T, entries []Activity, v string) {
	t.Helper()
	hidden := false
	for _, a := range entries {
		for _, e := range encodings(v) {
			if strings.Contains(a.What, e) || strings.Contains(a.Title, e) {
				t.Errorf("%q is in %+v", e, a)
			}
		}
		if a.Kind == KindPage && a.Title != "Welcome <hidden> | Inbox" {
			t.Errorf("title %q", a.Title)
		}
		hidden = hidden || strings.Contains(a.What, "<hidden>")
	}
	if !hidden {
		t.Errorf("nothing hidden in %+v", entries)
	}
}

func TestKeptRunIsScrubbedAtEnd(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.noteEverywhere(s.ID, testSecret)
	// Registered after the entries were written.
	if err := f.m.RegisterSecret(s.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if f.m.SecretCount(s.ID) != 0 {
		t.Fatal("the secrets outlived the session")
	}
	hidesEverywhere(t, f.runs("google", 1)[0].Entries, testSecret)
}

func TestLiveLogIsScrubbedAtRead(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.noteEverywhere(s.ID, testSecret)
	if err := f.m.RegisterSecret(s.ID, testSecret); err != nil {
		t.Fatal(err)
	}
	hidesEverywhere(t, f.m.Activity(s.ID), testSecret)
}

func TestStopReasons(t *testing.T) {
	const marker = "PAGE-WROTE-THIS"
	gib := int64(1 << 30)
	type stopper func(f *fixture, s Session)
	cases := []struct {
		name     string
		autoStop time.Duration
		stop     stopper
		end      EndReason
		text     string
	}{
		{"agent", 0, func(f *fixture, s Session) { _, _ = f.m.StopFor(context.Background(), s.ID, StopByAgent("claude")) }, EndAgent, "Stopped by claude"},
		{"admin", 0, func(f *fixture, s Session) { _, _ = f.m.StopFor(context.Background(), s.ID, StopFromApp) }, EndAdmin, "Stopped from the app"},
		{"idle", 30 * time.Minute, func(f *fixture, s Session) {
			advance := f.clock()
			f.m.Touch(s.ID)
			advance(31 * time.Minute)
			f.m.stopIdle(context.Background())
		}, EndIdle, "Stopped: idle for 30 min"},
		{"room", 0, func(f *fixture, s Session) {
			f.rt.Memory = gib
			f.m.SetMemoryLimit(gib+gib/2, gib) // room for one
			f.profile("other", 0)
			f.startProfile("other")
		}, EndRoom, "Stopped to make room for another browser"},
		{"shutdown", 0, func(f *fixture, s Session) { _ = f.m.StopAll(context.Background()) }, EndShutdown, "Stopped with BrowserPier"},
		{"crash", 0, func(f *fixture, s Session) { f.rt.Exit(s.ContainerID, 1) }, EndCrashed, "The browser exited unexpectedly"},
		{"unclean", 0, func(f *fixture, s Session) {
			f.rt.StopStatus = 1
			_, _ = f.m.Stop(context.Background(), s.ID)
		}, EndUnclean, "The browser didn't close cleanly: recent logins may be lost"},
		{"lost", 0, func(f *fixture, s Session) { f.rt.Lose(s.ContainerID) }, EndLost, "Lost track of the browser container"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.rt.LastLog = `level=ERROR msg=exiting err="` + marker + `"`
			f.profile("p", c.autoStop)
			s := f.startProfile("p")
			f.m.Note(s.ID, "claude", "Clicked at 1, 2")
			c.stop(f, s)
			eventually(t, "the run to be kept", func() bool { return len(f.m.Runs("p")) == 1 })
			r := f.m.Runs("p")[0]
			last := r.Entries[0]
			if r.End != c.end || r.EndText != c.text || last.What != c.text || last.Kind != KindSession || r.End.Failed() != (c.name == "crash" || c.name == "unclean" || c.name == "lost") {
				t.Fatalf("kept %+v", r)
			}
			for _, a := range r.Entries {
				if strings.Contains(a.What+a.Title, marker) {
					t.Fatalf("an entry holds the container's words: %+v", a)
				}
			}
		})
	}

	t.Run("start_failed", func(t *testing.T) {
		f := newFixture(t)
		f.rt.LastLog = `level=ERROR msg=exiting err="` + marker + `"`
		status := 1
		f.rt.ExitOnStart = &status
		_, _, _ = f.m.Start(context.Background(), "google", "")
		r := f.runs("google", 1)[0]
		if r.End != EndStartFailed || r.Entries[0].What != "The browser didn't start" || strings.Contains(fmt.Sprint(r), marker) {
			t.Fatalf("kept %+v", r)
		}
	})

	for _, c := range []struct {
		name string
		end  EndReason
		text string
	}{
		{"outdated", EndOutdated, "Stopped: started by an older BrowserPier"},
		{"secrets", EndSecrets, "Stopped: BrowserPier restarted while it held filled secrets"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.start(f.m)
			if c.end == EndSecrets {
				if err := f.m.RegisterSecret(s.ID, testSecret); err != nil {
					t.Fatal(err)
				}
			}
			f.m.Close()
			if c.end == EndOutdated {
				f.rt.Mu.Lock()
				f.rt.Containers[s.ContainerID].Version = ""
				f.rt.Mu.Unlock()
			}
			m := f.manager()
			if err := m.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs := m.Runs("google")
			if len(runs) != 1 || runs[0].End != c.end || runs[0].EndText != c.text {
				t.Fatalf("kept %+v", runs)
			}
			if got := whats(runs[0].Entries); strings.Join(got, "|") != restartedNote+"|"+c.text {
				t.Fatalf("entries %q", got)
			}
		})
	}
}

func TestControlEntries(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	controls := func() []string {
		var out []string
		for _, a := range f.m.Activity(s.ID) {
			if a.Kind == KindControl {
				out = append([]string{a.What}, out...)
			}
		}
		return out
	}
	// Nobody holds it: handing back writes nothing.
	f.m.ReleaseControl(s.ID)
	// An agent driving: refused, nothing written.
	ctx := context.Background()
	_, done, err := f.m.AgentInput(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while the agent drives: %v", err)
	}
	advance(agentQuiet)
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	// Someone else already has it: refused, nothing written.
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("second take: %v", err)
	}
	f.m.ReleaseControl(s.ID)
	f.m.ReleaseControl(s.ID)
	// Through a handoff link, in Needs help.
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControlByLink(s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	want := []string{tookControl, controlHandedBack, tookControlByLink, controlHandedBack}
	if got := controls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("control entries %q, want %q", got, want)
	}
}

func TestWatchEntries(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	watches := func() []string {
		var out []string
		for _, a := range f.m.Activity(s.ID) {
			if a.Kind == KindWatch {
				out = append([]string{a.What}, out...)
			}
		}
		return out
	}
	one, err := f.m.Watch(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.m.Watch(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	one()
	one() // twice is once
	if got := watches(); strings.Join(got, "|") != startedWatching {
		t.Fatalf("with one still watching: %q", got)
	}
	two()
	if got := watches(); strings.Join(got, "|") != startedWatching+"|"+nobodyWatching {
		t.Fatalf("after the last left: %q", got)
	}
	// Back within 30 seconds (a reload): the pair is taken back.
	advance(unwatchGrace - time.Second)
	again, err := f.m.Watch(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := watches(); strings.Join(got, "|") != startedWatching {
		t.Fatalf("after a reconnection: %q", got)
	}
	again()
	advance(unwatchGrace)
	later, err := f.m.Watch(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	later()
	want := []string{startedWatching, nobodyWatching, startedWatching, nobodyWatching}
	if got := watches(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("after a later view: %q", got)
	}
	// Previews write nothing (they need a control socket that answers
	// /screenshot; the fake's doesn't, which changes nothing here).
	before := len(f.m.Activity(s.ID))
	_, _ = f.m.Screenshot(context.Background(), "google", 0)
	if n := len(f.m.Activity(s.ID)); n != before {
		t.Fatal("a preview wrote an entry")
	}
}

func TestTitleEntries(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	titles := func() []Activity {
		var out []Activity
		for _, a := range f.m.Activity(s.ID) {
			if a.Kind == KindPage {
				out = append(out, a)
			}
		}
		return out
	}
	f.m.NoteTitle(s.ID, "Inbox")
	f.m.NoteTitle(s.ID, "Inbox")
	if got := titles(); len(got) != 1 || got[0].Title != "Inbox" || got[0].What != pageTitled || got[0].Agent != "" {
		t.Fatalf("same title twice: %+v", got)
	}
	// Two in a row: the second replaces the first.
	f.m.NoteTitle(s.ID, "Sent")
	if got := titles(); len(got) != 1 || got[0].Title != "Sent" {
		t.Fatalf("two in a row: %+v", got)
	}
	f.m.Note(s.ID, "claude", "Clicked at 1, 2")
	// Control, C1 and bidi characters go, whitespace collapses.
	f.m.NoteTitle(s.ID, "  Pay\u202e\u2066 now\n\t\x00 \u0085here\x7f ")
	if got := titles(); len(got) != 2 || got[0].Title != "Pay now here" {
		t.Fatalf("cleaned: %+v", got)
	}
	f.m.Note(s.ID, "claude", "Clicked at 3, 4")
	// Cut to 80 characters when read.
	long := strings.Repeat("é", 200) // 400 bytes
	f.m.NoteTitle(s.ID, long)
	if got := titles(); len([]rune(got[0].Title)) != shownTitleRunes || !strings.HasPrefix(long, strings.TrimSuffix(got[0].Title, "…")) {
		t.Fatalf("long title: %q", got[0].Title)
	}
	f.m.Note(s.ID, "claude", "Clicked at 5, 6")
	// Over 512 bytes: not kept.
	f.m.NoteTitle(s.ID, strings.Repeat("a", maxTitleBytes+1))
	if got := titles(); got[0].Title != "" || got[0].What != pageChanged {
		t.Fatalf("over 512 bytes: %+v", got[0])
	}
	f.m.Note(s.ID, "claude", "Clicked at 7, 8")
	n := len(titles())
	// Never while a person has control, nor during a handoff.
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.NoteTitle(s.ID, "What the person typed")
	f.m.ReleaseControl(s.ID)
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	f.m.NoteTitle(s.ID, "During the handoff")
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := titles(); len(got) != n {
		t.Fatalf("a title was noted while a person had the browser: %+v", got)
	}
	for _, a := range f.m.Activity(s.ID) {
		if a.Kind != KindPage && strings.Contains(a.What, "Inbox") {
			t.Fatalf("a title is in an entry's words: %+v", a)
		}
	}
	// Kept cut, too.
	f.m.NoteTitle(s.ID, long+" again")
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.runs("google", 1)[0].Entries {
		if utf8.RuneCountInString(a.Title) > shownTitleRunes {
			t.Fatalf("kept title %q", a.Title)
		}
	}
}

func TestDeleteProfileDropsRuns(t *testing.T) {
	f := newFixture(t)
	f.profile("other", 0)
	for _, p := range []string{"google", "other"} {
		s := f.startProfile(p)
		if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.m.DeleteProfile(context.Background(), "google"); err != nil {
		t.Fatal(err)
	}
	f.runs("google", 0)
	f.runs("other", 1)
}

func TestReconcileNotesTheRestart(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	f.m.Note(s.ID, "claude", "Clicked at 1, 2")
	f.m.Close()
	m := f.manager()
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := whats(m.Activity(s.ID)); strings.Join(got, "|") != restartedNote {
		t.Fatalf("picked up with %q", got)
	}
	if runs := m.Runs("google"); len(runs) != 0 {
		t.Fatalf("a restart kept %+v", runs)
	}
	// A stop the BrowserPier before began, picked up and finished: its
	// reason went with the restart.
	if _, err := m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if r := m.Runs("google"); len(r) != 1 || r[0].StartedBy != "" || r[0].FromApp {
		t.Fatalf("kept %+v", r)
	}
}

func TestHandoffCancelledByStopIsKept(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	got := whats(f.runs("google", 1)[0].Entries)
	if strings.Join(got, "|") != helpCancelledStopped+"|Stopped from the app" {
		t.Fatalf("entries %q", got)
	}
	// A crash in Needs help says so too; a plain stop doesn't.
	s = f.start(f.m)
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	f.rt.Exit(s.ContainerID, 1)
	eventually(t, "the run to be kept", func() bool { return len(f.m.Runs("google")) == 2 })
	if got := whats(f.m.Runs("google")[0].Entries); strings.Join(got, "|") != helpCancelledStopped+"|The browser exited unexpectedly" {
		t.Fatalf("entries %q", got)
	}
	s = f.start(f.m)
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if got := whats(f.m.Runs("google")[0].Entries); strings.Join(got, "|") != "Stopped from the app" {
		t.Fatalf("entries %q", got)
	}
}

func TestStartedFromTheApp(t *testing.T) {
	f := newFixture(t)
	s, _, err := f.m.StartWith(context.Background(), "google", StartOptions{FromApp: true, Mode: profiles.ModeVisual})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.m.Activity(s.ID); len(got) != 1 || got[0].What != startedFromApp || got[0].Kind != KindSession {
		t.Fatalf("log %+v", got)
	}
	// Attaching writes nothing.
	if _, attached, err := f.m.StartWith(context.Background(), "google", StartOptions{FromApp: true}); err != nil || !attached {
		t.Fatal(attached, err)
	}
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if r := f.runs("google", 1)[0]; !r.FromApp || r.StartedBy != "" || r.Mode != profiles.ModeVisual || len(r.Entries) != 2 {
		t.Fatalf("kept %+v", r)
	}
}

// RecentCalls lists the token's own actions only: never BrowserPier's
// entries, a title among them.
func TestRecentCallsHoldActionsOnly(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx := WithCall(context.Background(), f.m.NewCall("tok", "claude", "click"))
	f.m.NoteFor(ctx, s.ID, "claude", "Clicked at 1, 2")
	f.m.NoteTitle(s.ID, "Secret page title")
	w, _ := f.m.Watch(s.ID)
	w()
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	f.m.NoteHandoff(s.ID, "Handed back: done")
	got := f.m.RecentCalls(s.ID, "tok", 20)
	if len(got) != 1 || got[0].What != "Clicked at 1, 2" {
		t.Fatalf("recent calls %+v", got)
	}
}
