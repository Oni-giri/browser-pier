package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/recording"
)

// fakeRecorder notes what the manager tells it, in order.
type fakeRecorder struct {
	mu     sync.Mutex
	allow  map[string]bool
	why    map[string]recording.Reason
	events []string
	calls  int
	ended  []string
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{allow: map[string]bool{}, why: map[string]recording.Reason{}}
}

func (r *fakeRecorder) Set(id, _ string, allow bool, why recording.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.allow[id], r.why[id] = allow, why
	if allow {
		r.events = append(r.events, "recording")
	} else {
		r.events = append(r.events, "stopped:"+string(why))
	}
}

func (r *fakeRecorder) End(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, id)
}

func (r *fakeRecorder) note(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *fakeRecorder) state(id string) (bool, recording.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.allow[id], r.why[id]
}

// recordingFixture is a fixture whose profile records sessions.
func recordingFixture(t *testing.T, on bool) (*fixture, *fakeRecorder) {
	t.Helper()
	f := newFixture(t)
	rec := newFakeRecorder()
	f.m.SetRecorder(rec, recording.NewStore(filepath.Join(f.dir, "recordings"), slog.New(slog.DiscardHandler)))
	p, _ := f.m.Profiles().Get(context.Background(), "google")
	p.Record = on
	if _, err := f.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return f, rec
}

func wantRecording(t *testing.T, rec *fakeRecorder, id string, want bool, why recording.Reason) {
	t.Helper()
	got, gotWhy := rec.state(id)
	if got != want || (!want && why != "" && gotWhy != why) {
		t.Fatalf("recording = %v (%s), want %v (%s)", got, gotWhy, want, why)
	}
}

func TestTakeControlStopsTheRecorderBeforeItReturns(t *testing.T) {
	for _, viaHandoff := range []bool{false, true} {
		f, rec := recordingFixture(t, true)
		advance := f.clock()
		s := f.start(f.m)
		// An act that has run long enough for the person to break it off:
		// TakeControl lets go of the lock while it waits for it.
		lease, err := f.m.PageLease(context.Background(), s.ID, "tok")
		if err != nil {
			t.Fatal(err)
		}
		lease.Breakable()
		wantRecording(t, rec, s.ID, true, "")
		advance(takeOverAfter + time.Second)
		go func() { <-lease.Ctx.Done(); time.Sleep(20 * time.Millisecond); lease.Release() }()

		// Someone polls for control the whole time, as the live view would
		// before forwarding a key.
		stop := make(chan struct{})
		polled := make(chan struct{})
		go func() {
			defer close(polled)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if viaHandoff {
					if snap, _ := f.m.Live("google"); snap.State == NeedsHelp {
						rec.note("person has it")
						return
					}
				} else if f.m.HumanInput(s.ID) == nil {
					rec.note("person has it")
					return
				}
			}
		}()
		if viaHandoff {
			if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
				t.Fatal(err)
			}
		} else if err := f.m.TakeControl(s.ID); err != nil {
			t.Fatal(err)
		}
		want := recording.Person
		if viaHandoff {
			want = recording.Handoff
		}
		wantRecording(t, rec, s.ID, false, want)
		eventually(t, "the poller", func() bool {
			select {
			case <-polled:
				return true
			default:
				return false
			}
		})
		close(stop)
		rec.mu.Lock()
		events := append([]string(nil), rec.events...)
		rec.mu.Unlock()
		stoppedAt, tookAt := -1, -1
		for i, e := range events {
			switch e {
			case "stopped:" + string(want):
				if stoppedAt < 0 {
					stoppedAt = i
				}
			case "person has it":
				tookAt = i
			}
		}
		if stoppedAt < 0 || tookAt < 0 || stoppedAt > tookAt {
			t.Fatalf("handoff %v: events %v: the recorder must stop before the person has the browser", viaHandoff, events)
		}
	}
}

func TestRecordingWaitsForTheAgentAfterHandBack(t *testing.T) {
	f, rec := recordingFixture(t, true)
	advance := f.clock()
	s := f.start(f.m)
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	advance(agentQuiet + time.Second) // within the tail, past the agent's quiet time
	wantRecording(t, rec, s.ID, true, "")
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	// The tail from before doesn't carry over the person's control.
	wantRecording(t, rec, s.ID, false, recording.Idle)
	_, done, err = f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRecording(t, rec, s.ID, true, "")
	done()

	// The same around a handoff, whose agent connection closes as it opens.
	_, release, err := f.m.Attach(context.Background(), s.ID, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	release() // the connection closes after the handoff opened
	wantRecording(t, rec, s.ID, false, recording.Handoff)
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	wantRecording(t, rec, s.ID, false, recording.Idle)
	lease, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	wantRecording(t, rec, s.ID, true, "")
	lease.Release()
}

func TestAgentActionsStartRecordingAndTheTailEnds(t *testing.T) {
	f, rec := recordingFixture(t, true)
	advance := f.clock()
	s := f.start(f.m)
	wantRecording(t, rec, s.ID, false, "")
	if rec.calls != 0 {
		t.Fatalf("%d calls to the recorder with no agent", rec.calls)
	}
	actions := map[string]func() func(){
		"CDP connection": func() func() {
			_, release, err := f.m.Attach(context.Background(), s.ID, "tok", "claude")
			if err != nil {
				t.Fatal(err)
			}
			return release
		},
		"page call": func() func() {
			l, err := f.m.PageLease(context.Background(), s.ID, "tok")
			if err != nil {
				t.Fatal(err)
			}
			return l.Release
		},
		"input action": func() func() {
			_, done, err := f.m.AgentInput(context.Background(), s.ID)
			if err != nil {
				t.Fatal(err)
			}
			return done
		},
	}
	for name, act := range actions {
		end := act()
		wantRecording(t, rec, s.ID, true, "")
		advance(time.Minute) // under way: still recording
		f.m.mu.Lock()
		f.m.regateLocked(f.m.live["google"])
		f.m.mu.Unlock()
		wantRecording(t, rec, s.ID, true, "")
		end()
		advance(recording.Tail - time.Second)
		f.m.mu.Lock()
		f.m.regateLocked(f.m.live["google"])
		f.m.mu.Unlock()
		if ok, _ := rec.state(s.ID); !ok {
			t.Fatalf("%s: stopped within the tail", name)
		}
		advance(2 * time.Second)
		f.m.mu.Lock()
		f.m.regateLocked(f.m.live["google"]) // what the tail's timer does
		f.m.mu.Unlock()
		wantRecording(t, rec, s.ID, false, recording.Idle)
	}
	// The timer itself, on the real clock.
	f.m.now = func() time.Time { return time.Now() }
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	f.m.mu.Lock()
	l := f.m.live["google"]
	l.agentAt = time.Now().Add(-recording.Tail + 50*time.Millisecond)
	f.m.regateLocked(l)
	f.m.mu.Unlock()
	eventually(t, "the tail to end", func() bool { ok, _ := rec.state(s.ID); return !ok })
}

func TestRecordingNeedsTheSettingAndARunningBrowser(t *testing.T) {
	f, rec := recordingFixture(t, false)
	s := f.start(f.m)
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	wantRecording(t, rec, s.ID, false, "")
	// The admin turns it on: the running session follows at once.
	p, _ := f.m.Profiles().Get(context.Background(), "google")
	p.Record = true
	if _, err := f.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := f.m.RefreshRecording(context.Background(), "google"); err != nil {
		t.Fatal(err)
	}
	wantRecording(t, rec, s.ID, true, "")
	resume, err := f.m.PauseRecording(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRecording(t, rec, s.ID, false, recording.Paused)
	resume()
	wantRecording(t, rec, s.ID, true, "")
	if !f.m.Recording(s.ID) {
		t.Fatal("Recording() false while recording")
	}
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	wantRecording(t, rec, s.ID, false, recording.Stopped)
	if len(rec.ended) != 1 || rec.ended[0] != s.ID {
		t.Fatalf("End calls %v", rec.ended)
	}
}

func TestDefaultProfileNeverCallsTheRecorder(t *testing.T) {
	f := newFixture(t)
	rec := newFakeRecorder()
	f.m.SetRecorder(rec, recording.NewStore(filepath.Join(f.dir, "recordings"), slog.New(slog.DiscardHandler)))
	if p, _ := f.m.Profiles().Get(context.Background(), "google"); p.Record {
		t.Fatal("a new profile records sessions")
	}
	s := f.start(f.m)
	_, release, err := f.m.Attach(context.Background(), s.ID, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if ok, _ := rec.state(s.ID); ok {
		t.Fatal("recording a profile with the setting off")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "recordings")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a recordings folder with recording off everywhere: %v", err)
	}
}

func TestRecordingDoesntHoldOffIdleStop(t *testing.T) {
	f, _ := recordingFixture(t, false)
	rec := newFakeRecorder()
	f.m.SetRecorder(rec, nil)
	ctx := context.Background()
	advance := f.clock()
	for _, name := range []string{"recorded", "plain"} {
		p := profiles.New(name)
		p.AutoStop = 5 * time.Minute
		p.Record = name == "recorded"
		if _, err := f.m.Profiles().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	rs, ps := f.startProfile("recorded"), f.startProfile("plain")
	for _, s := range []Session{rs, ps} {
		_, done, err := f.m.AgentInput(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		done()
	}
	if ok, _ := rec.state(rs.ID); !ok {
		t.Fatal("the recorded profile isn't recording")
	}
	advance(5*time.Minute - time.Second)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("recorded"); !ok {
		t.Fatal("stopped before its auto-stop")
	}
	advance(2 * time.Second)
	f.m.stopIdle(ctx)
	for _, name := range []string{"recorded", "plain"} {
		if _, ok := f.m.Live(name); ok {
			t.Errorf("%s still running past its auto-stop", name)
		}
	}
}

// writeRecording puts a one-segment recording on disk, as the recorder leaves it.
func writeRecording(t *testing.T, dir, profile, session string) string {
	t.Helper()
	sd := filepath.Join(dir, "recordings", profile, session)
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ix := recording.Index{Profile: profile, Session: session, Segments: []recording.Segment{{File: "000001.mp4", Start: now.Add(-time.Minute), End: now}}}
	b, _ := json.Marshal(ix)
	if err := os.WriteFile(filepath.Join(sd, "index.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "000001.mp4"), []byte("mp4"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sd
}

func TestDeleteProfileDeletesItsRecordings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	store := recording.NewStore(filepath.Join(f.dir, "recordings"), slog.New(slog.DiscardHandler))
	f.m.SetRecorder(newFakeRecorder(), store)
	s := f.start(f.m)
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	sd := writeRecording(t, f.dir, "google", s.ID)
	if err := store.Reconcile([]string{"google"}); err != nil {
		t.Fatal(err)
	}

	// A failure leaves the profile and its recordings both in place.
	profilesDir := filepath.Join(f.dir, "profiles")
	recDir := filepath.Join(f.dir, "recordings")
	if err := os.Chmod(recDir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := f.m.DeleteProfile(ctx, "google")
	_ = os.Chmod(recDir, 0o700)
	if err == nil {
		t.Fatal("DeleteProfile succeeded with an unwritable recordings folder")
	}
	if _, err := f.m.Profiles().Get(ctx, "google"); err != nil {
		t.Fatalf("the profile is gone after a failed delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profilesDir, "google")); err != nil {
		t.Fatalf("the profile folder is gone after a failed delete: %v", err)
	}
	if got := store.Sessions("google"); len(got) != 1 {
		t.Fatalf("recordings after a failed delete: %d", len(got))
	}

	if err := f.m.DeleteProfile(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sd); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the recording is still there: %v", err)
	}
	if got := store.Sessions("google"); len(got) != 0 {
		t.Fatal("the deleted profile's recordings are still listed")
	}
	if entries, _ := os.ReadDir(recDir); len(entries) != 0 {
		t.Fatalf("left in recordings/: %v", entries)
	}
}

func TestReconcileAndRecordings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.Profiles().Create(ctx, profiles.New("github")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"google", "github"} {
		p, _ := f.m.Profiles().Get(ctx, name)
		p.Record = true
		if _, err := f.m.Profiles().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	running, helped := f.start(f.m), f.startProfile("github")
	if _, err := f.m.SetNeedsHelp(helped.ID, true); err != nil {
		t.Fatal(err)
	}
	f.m.Close()
	gone := writeRecording(t, f.dir, "deleted", "aaaaaaaaaaaaaaaaaaaaaaaa")

	m := f.manager()
	rec := newFakeRecorder()
	m.SetRecorder(rec, recording.NewStore(filepath.Join(f.dir, "recordings"), slog.New(slog.DiscardHandler)))
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted profile's recording survived Reconcile: %v", err)
	}
	// Picked up: the running one records from the agent's next action...
	wantRecording(t, rec, running.ID, false, "")
	_, done, err := m.AgentInput(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	wantRecording(t, rec, running.ID, true, "")
	// ...the one with a person for a handoff doesn't.
	if _, _, err := m.AgentInput(ctx, helped.ID); err == nil {
		t.Fatal("agent input during a handoff")
	}
	if ok, _ := rec.state(helped.ID); ok {
		t.Fatal("recording a session in a handoff")
	}
}
