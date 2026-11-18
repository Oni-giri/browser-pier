package sessions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeChoosers is a browser's control socket with one open chooser.
type fakeChoosers struct {
	mu       sync.Mutex
	restarts int
	answers  []string
}

func (f *fakeChoosers) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choosers":[{"id":"c1","multiple":true,"accept":["*.pdf"],"age_ms":5,"left_ms":299000},{"id":"../x","multiple":false}]}`)
	})
	mux.HandleFunc("POST /chooser/restart", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.restarts++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "c1" {
			http.Error(w, "no open file chooser with that id", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.answers = append(f.answers, string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (f *fakeChoosers) waitRestarts(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		got := f.restarts
		f.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d restarts, want %d", got, n)
		}
	}
}

func TestChoosersListAndAnswer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	fc := &fakeChoosers{}
	f.rt.Control = fc.mux()
	s := f.start(f.m)
	cs, err := f.m.Choosers(ctx, s.Profile)
	if err != nil || len(cs) != 1 || cs[0] != (Chooser{ID: "c1", Multiple: true, Left: 299 * time.Second}) {
		t.Fatalf("Choosers: %+v %v", cs, err)
	}
	if err := f.m.AnswerChooser(ctx, s.Profile, "c1", []string{"a.pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.AnswerChooser(ctx, s.Profile, "c1", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.m.AnswerChooser(ctx, s.Profile, "c2", nil); !errors.Is(err, ErrChooserClosed) {
		t.Fatalf("a closed chooser: %v", err)
	}
	if err := f.m.AnswerChooser(ctx, s.Profile, "../restart", nil); !errors.Is(err, ErrChooserClosed) {
		t.Fatalf("a bad id: %v", err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.answers) != 2 || fc.answers[0] != `{"files":["a.pdf"]}` || fc.answers[1] != `{"cancel":true}` {
		t.Fatalf("answers %q", fc.answers)
	}
}

// TestChooserTimerRestartsWhenControlChangesHands (PROJ-181): taking
// control, handing back, and a handoff's start and end each give the
// browser's open choosers their whole time limit again.
func TestChooserTimerRestartsWhenControlChangesHands(t *testing.T) {
	f := newFixture(t)
	fc := &fakeChoosers{}
	f.rt.Control = fc.mux()
	s := f.start(f.m)
	if f.m.HumanControl(s.ID) {
		t.Fatal("a person has control from the start")
	}
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	fc.waitRestarts(t, 1)
	if !f.m.HumanControl(s.ID) {
		t.Fatal("no person has control after Take control")
	}
	f.m.ReleaseControl(s.ID)
	fc.waitRestarts(t, 2)
	f.m.ReleaseControl(s.ID) // nobody had it: nothing changed hands
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	fc.waitRestarts(t, 3)
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	fc.waitRestarts(t, 4)
	if f.m.HumanControl(s.ID) {
		t.Fatal("a person has control after hand-back")
	}
}

// An image from before the portal has no /chooser: it says so as no
// portal, not as an error to log on every poll.
func TestChoosersOnAnImageWithoutThePortal(t *testing.T) {
	f := newFixture(t) // its control socket serves /healthz only
	s := f.start(f.m)
	if cs, err := f.m.Choosers(context.Background(), s.Profile); !errors.Is(err, ErrNoPortal) || len(cs) != 0 {
		t.Fatalf("Choosers: %+v %v", cs, err)
	}
}
