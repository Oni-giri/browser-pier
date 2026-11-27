package sessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

func TestTakeAndReleaseControl(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	advance := f.clock()
	s := f.start(f.m)

	// An agent clicking just now is driving.
	_, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	done()
	if err := f.m.TakeControl(s.ID, "person"); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while the agent clicks = %v", err)
	}
	advance(agentQuiet + time.Second)
	// So is one connected over CDP.
	_, release, err := f.m.Attach(ctx, s.ID, "t", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControl(s.ID, "person"); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while an agent is connected = %v", err)
	}
	release()

	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Fatal("HumanControl not shown")
	}
	if err := f.m.TakeControl(s.ID, "person"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("a second person taking control = %v, want ErrHumanDriving", err)
	}
	if done, err := humanInput(f.m, s.ID); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
	// While a person drives, agents can only watch.
	if _, _, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("agent input while a person drives = %v", err)
	}
	if _, _, err := f.m.Attach(ctx, s.ID, "t", "claude"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("CDP while a person drives = %v", err)
	}
	f.m.ReleaseControl(s.ID)
	if _, err := humanInput(f.m, s.ID); !errors.Is(err, ErrNotInControl) {
		t.Fatalf("human input after release = %v", err)
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input after hand-back = %v", err)
	} else {
		done()
	}
}

// TestControlHasOneHolder (PROJ-184): control belongs to the holder that
// took it. Another holder (the admin's live view while a handoff link
// drives, or the reverse) neither counts as in control nor can use it,
// and once it is handed back nobody holds it.
func TestControlHasOneHolder(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	phone, admin := HandoffHolder("h1"), AdminHolder
	if err := f.m.TakeControl(s.ID, ""); err == nil {
		t.Fatal("control taken with no holder")
	}
	if err := f.m.TakeControl(s.ID, phone); err != nil {
		t.Fatal(err)
	}
	if !f.m.ControlledBy(s.ID, phone) {
		t.Fatal("the holder isn't in control")
	}
	if _, _, done, err := f.m.HumanInput(s.ID, phone); err != nil {
		t.Fatal("the holder isn't in control")
	} else {
		done()
	}
	for _, other := range []string{admin, "", HandoffHolder(""), HandoffHolder("h2")} {
		if f.m.ControlledBy(s.ID, other) {
			t.Fatalf("%q counts as in control while %q holds it", other, phone)
		}
		if _, _, _, err := f.m.HumanInput(s.ID, other); !errors.Is(err, ErrNotInControl) {
			t.Fatalf("input from %q = %v, want ErrNotInControl", other, err)
		}
	}
	if err := f.m.TakeControl(s.ID, admin); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("the admin taking control from the phone = %v", err)
	}
	f.m.ReleaseControl(s.ID)
	if f.m.ControlledBy(s.ID, phone) {
		t.Fatal("still in control after hand-back")
	}
	if err := f.m.TakeControl(s.ID, admin); err != nil {
		t.Fatal(err)
	}
	if !f.m.ControlledBy(s.ID, admin) || f.m.ControlledBy(s.ID, phone) {
		t.Fatal("control didn't move to the admin alone")
	}
}

// TestReleaseTurnIsOnlyItsOwn (PROJ-184, review S1): a turn released
// behind a live view's back (a handoff finished) and taken by the next
// person, even with the same holder, is no longer that view's: it can't
// release the next one's.
func TestReleaseTurnIsOnlyItsOwn(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	if _, err := f.m.TakeControlTurn(s.ID, ""); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("no holder: %v", err)
	}
	old, err := f.m.TakeControlTurn(s.ID, AdminHolder)
	if err != nil {
		t.Fatal(err)
	}
	if !f.m.HoldsTurn(s.ID, old) {
		t.Fatal("the turn isn't held")
	}
	f.m.ReleaseControl(s.ID) // a handoff's end, or Hand back
	release := func(turn uint64) bool {
		ended, err := f.m.ReleaseTurn(context.Background(), s.ID, turn)
		if err != nil {
			t.Fatal(err)
		}
		return ended
	}
	for _, next := range []string{HandoffHolder("h1"), AdminHolder} {
		turn, err := f.m.TakeControlTurn(s.ID, next)
		if err != nil {
			t.Fatal(err)
		}
		if f.m.HoldsTurn(s.ID, old) || release(old) {
			t.Fatalf("an old turn held or released %q's", next)
		}
		if !f.m.ControlledBy(s.ID, next) || !f.m.HoldsTurn(s.ID, turn) {
			t.Fatalf("%q lost control to an old turn", next)
		}
		if !release(turn) || f.m.ControlledBy(s.ID, next) {
			t.Fatalf("%q couldn't release its own turn", next)
		}
	}
}

// A turn handed back while Take control waited for an agent's input to
// let go (the profile page's Hand back, a handoff's end) isn't reported as
// taken: the live view would think it drives a browser nobody gave it.
func TestATurnHandedBackWhileItWaitsIsNotTaken(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = f.m.SetNeedsHelp(s.ID, true) }()
	<-ctx.Done()
	took := make(chan error, 1)
	go func() {
		_, err := f.m.TakeControlTurn(s.ID, AdminHolder)
		took <- err
	}()
	for deadline := time.Now().Add(2 * time.Second); !f.m.ControlledBy(s.ID, AdminHolder); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Take control never started waiting")
		}
	}
	f.m.ReleaseControl(s.ID)
	done()
	select {
	case err := <-took:
		if !errors.Is(err, ErrControlEnded) {
			t.Fatalf("a turn handed back while it waited = %v, want ErrControlEnded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned after the input ended")
	}
}

// Input, a lift and the hand-back's own wait are bound to a turn, not to
// the holder alone: the admin's views share one holder, so a view whose
// turn ended can't type into the next one's, a turn being handed back
// isn't held any more, and the hand-back lets go of held keys itself.
func TestInputIsOnlyThisTurns(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var calls []string
	var held []bool
	var turn uint64
	var id string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h := f.m.HoldsTurn(id, turn)
			mu.Lock()
			calls = append(calls, name)
			held = append(held, h)
			mu.Unlock()
			if name == "release" {
				sessionstest.AnswerLift(w, r) // the takeover fenced
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /input/release", record("release"))
	mux.HandleFunc("DELETE /input/clipboard", record("empty"))
	f.rt.Control = mux
	s := f.start(f.m)
	id = s.ID
	old, err := f.m.TakeControlTurn(s.ID, AdminHolder)
	if err != nil {
		t.Fatal(err)
	}
	turn = old
	f.m.ReleaseControl(s.ID) // Hand back from another page
	mu.Lock()
	if strings.Join(calls, " ") != "release empty" || held[0] || held[1] {
		t.Fatalf("the hand-back: %v, the turn held meanwhile %v", calls, held)
	}
	mu.Unlock()
	if turn, err = f.m.TakeControlTurn(s.ID, AdminHolder); err != nil { // the same admin, from another view
		t.Fatal(err)
	}
	if _, _, err := f.m.HumanInputTurn(s.ID, AdminHolder, old); !errors.Is(err, ErrNotInControl) {
		t.Fatalf("input from the old turn = %v", err)
	}
	if _, done, err := f.m.HumanInputTurn(s.ID, AdminHolder, turn); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
}

// A handoff waits for agent input already on its way, and refuses more:
// no agent click lands while the person has the browser.
func TestHandoffWaitsForInputInFlight(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	helped := make(chan struct{})
	go func() {
		_, _ = f.m.SetNeedsHelp(s.ID, true)
		close(helped)
	}()
	// The input breaks off at once (a long human-paced type, say)...
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrHandedOver) {
			t.Fatalf("input cancelled for %v", context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handoff didn't cancel the input in flight")
	}
	// ...and the handoff waits for it to end.
	select {
	case <-helped:
		t.Fatal("the handoff began while an agent's click was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	if _, _, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("new input while a handoff waits = %v", err)
	}
	done()
	select {
	case <-helped:
	case <-time.After(2 * time.Second):
		t.Fatal("the handoff never began after the click ended")
	}
}

// An input action keeps a person from taking control from the live view
// for its first takeOverAfter, and for agentQuiet after its end, not its
// start.
func TestLongInputKeepsTheAgentDriving(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	_, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter - time.Second)
	if err := f.m.TakeControl(s.ID, "person"); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while the agent types = %v", err)
	}
	done()
	advance(agentQuiet - time.Second)
	if err := f.m.TakeControl(s.ID, "person"); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take %v after the typing ended = %v", agentQuiet-time.Second, err)
	}
	advance(2 * time.Second)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatalf("take after the quiet = %v", err)
	}
}

// One agent input runs at a time in a session: a second while one is
// under way is refused, and allowed once it ends.
func TestOneAgentInputAtATime(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	_, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrInputBusy) {
		t.Fatalf("a second input while one runs = %v, want ErrInputBusy", err)
	}
	done()
	_, done, err = f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatalf("an input after the first ended = %v", err)
	}
	done()
}

// The person in the live view can take control from an input that has
// run for takeOverAfter (a long human-paced type): it breaks off, as for
// a handoff, and they get the browser once it has ended.
func TestTakeControlBreaksOffALongInput(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter)
	took := make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID, "person") }()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrTookControl) {
			t.Fatalf("input cancelled for %v", context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control didn't break off the input")
	}
	// The person gets the browser only once the input has let go.
	select {
	case err := <-took:
		t.Fatalf("Take control returned (%v) while the input was still under way", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, _, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) && !errors.Is(err, ErrInputBusy) {
		t.Fatalf("new input while Take control waits = %v", err)
	}
	done()
	select {
	case err := <-took:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned after the input ended")
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Fatal("HumanControl not shown")
	}
	if _, _, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("agent input after Take control = %v", err)
	}
}

// During a handoff's first moments, while the input it cancelled is still
// letting go, the person taking control waits for it too.
func TestTakeControlInAHandoffWaitsForInput(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = f.m.SetNeedsHelp(s.ID, true) }()
	<-ctx.Done()
	took := make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID, "person") }()
	select {
	case err := <-took:
		t.Fatalf("Take control returned (%v) while the input was still under way", err)
	case <-time.After(200 * time.Millisecond):
	}
	done()
	select {
	case err := <-took:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned after the input ended")
	}
}

// Stopping a browser cancels input under way.
func TestStopCancelsInput(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	go func() { _, _ = f.m.Stop(context.Background(), s.ID) }()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a stop didn't cancel the input under way")
	}
}

// A browser in the middle of a long agent input (a human-paced type)
// isn't idle, however long ago the input began.
func TestBrowserWithInputUnderWayIsNeverIdle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	advance := f.clock()
	f.profile("w", 5*time.Minute)
	s := f.startProfile("w")
	in, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-in.Done(); done() }() // a stop would cancel it
	advance(time.Hour)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("w"); !ok {
		t.Fatal("stopped as idle with an input under way")
	}
	done()
	advance(6 * time.Minute)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("w"); ok {
		t.Fatal("not stopped once the input ended and the auto-stop passed")
	}
}

func TestWatchedBrowserIsNeverIdle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	advance := f.clock()
	f.profile("w", 5*time.Minute)
	s := f.startProfile("w")
	release, err := f.m.Watch(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	advance(time.Hour)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("w"); !ok {
		t.Fatal("stopped while someone watched")
	}
	release()
	release() // twice is harmless
	advance(6 * time.Minute)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("w"); ok {
		t.Fatal("not stopped once the viewer left and the auto-stop passed")
	}
}

func TestOnEndHearsEachEnd(t *testing.T) {
	f := newFixture(t)
	ended := make(chan string, 4)
	f.m.OnEnd(func(id string) { ended <- id })
	s := f.start(f.m)
	select {
	case id := <-ended:
		t.Fatalf("%s ended before the stop", id)
	default:
	}
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-ended:
		if id != s.ID {
			t.Fatalf("OnEnd got %q, want %q", id, s.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnEnd wasn't called after the stop")
	}
}

// Whatever ends a person's control, the browser's clipboard is emptied
// before agents get the browser back: after the person's input under way
// has reached it, with no more let in meanwhile (PROJ-194).
func TestReleaseControlEmptiesTheClipboardFirst(t *testing.T) {
	f := newFixture(t)
	var logs syncBuffer
	f.m.log = slog.New(slog.NewTextHandler(&logs, nil))
	type emptying struct {
		agentInput, attach, pageLease, humanInput error
	}
	emptied := make(chan emptying, 10)
	var id string
	fail := false
	var lastEmptied time.Time
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, _ *http.Request) {
		// While it empties, agents are still kept out, and so is more of
		// the person's input.
		var e emptying
		if _, done, err := f.m.AgentInput(context.Background(), id, "tok"); err == nil {
			done()
		} else {
			e.agentInput = err
		}
		if _, release, err := f.m.Attach(context.Background(), id, "t", "claude"); err == nil {
			release()
		} else {
			e.attach = err
		}
		if l, err := f.m.PageLease(context.Background(), id, "t"); err == nil {
			l.Release()
		} else {
			e.pageLease = err
		}
		if done, err := humanInput(f.m, id); err == nil {
			done()
		} else {
			e.humanInput = err
		}
		emptied <- e
		mu.Lock()
		defer mu.Unlock()
		lastEmptied = time.Now()
		if fail {
			http.Error(w, "couldn't empty the clipboard: it kept its text", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	f.rt.Control = mux
	advance := f.clock()
	s := f.start(f.m)
	id = s.ID
	take := func() {
		t.Helper()
		if err := f.m.TakeControl(s.ID, "person"); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what string) {
		t.Helper()
		select {
		case e := <-emptied:
			if !errors.Is(e.agentInput, ErrHumanDriving) || !errors.Is(e.attach, ErrHumanDriving) || !errors.Is(e.pageLease, ErrHumanDriving) || !errors.Is(e.humanInput, ErrNotInControl) {
				t.Errorf("%s: while emptying, agent input %v, CDP %v, a page call %v, the person's input %v", what, e.agentInput, e.attach, e.pageLease, e.humanInput)
			}
		default:
			t.Fatalf("%s: the clipboard wasn't emptied", what)
		}
		if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
			t.Fatalf("%s: agent input after = %v", what, err)
		} else {
			done()
		}
		advance(agentQuiet + time.Second) // so the person can take control again
	}

	// A plain hand-back.
	take()
	f.m.ReleaseControl(s.ID)
	check("hand back")

	// The person's input under way is waited for.
	take()
	done, err := humanInput(f.m, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(released) }()
	select {
	case <-emptied:
		t.Fatal("emptied with the person's input still under way")
	case <-released:
		t.Fatal("handed back with the person's input still under way")
	case <-time.After(100 * time.Millisecond):
	}
	inputDone := time.Now()
	done()
	<-released
	mu.Lock()
	if waited := lastEmptied.Sub(inputDone); waited < clipboardSettle {
		t.Errorf("emptied %v after the person's last input, before a copy could land", waited)
	}
	mu.Unlock()
	check("input under way")

	// Two hand-backs at once (the live view going as a handoff ends):
	// emptied once, and neither returns before agents have the browser.
	take()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			f.m.ReleaseControl(s.ID)
			if got, _ := f.m.Live("google"); got.HumanControl {
				t.Error("a hand-back returned before the browser was handed back")
			}
		})
	}
	wg.Wait()
	check("two at once")
	if len(emptied) != 0 {
		t.Errorf("emptied %d more times", len(emptied))
	}
	// Without control, nothing to empty.
	f.m.ReleaseControl(s.ID)
	if len(emptied) != 0 {
		t.Error("emptied without a person in control")
	}

	// A browser that can't empty it: logged, and handed back all the same.
	mu.Lock()
	fail = true
	mu.Unlock()
	take()
	f.m.ReleaseControl(s.ID)
	check("a failed emptying")
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "clipboard") {
		t.Errorf("not logged: %s", logs.String())
	}
}

// A person's input and their own hand-back count only for the takeover
// they took (TakeControlTurn): once it has ended without them and someone
// else has taken the browser, they neither reach it nor end that
// takeover; their own hand-back lifts what they held before the
// clipboard is emptied (PROJ-196).
func TestControlBelongsToItsTakeover(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var calls []string
	note := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, what)
			mu.Unlock()
			sessionstest.AnswerLift(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", note("lift"))
	mux.HandleFunc("DELETE /input/clipboard", note("empty"))
	f.rt.Control = mux
	took := func() string {
		mu.Lock()
		defer mu.Unlock()
		out := strings.Join(calls, " ")
		calls = nil
		return out
	}
	s := f.start(f.m)

	first, err := f.m.TakeControlTurn(s.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID) // ended from outside: a handoff, the profile page
	second, err := f.m.TakeControlTurn(s.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("both takeovers are generation %d", first)
	}
	took()
	if _, _, err := f.m.HumanInputTurn(s.ID, "person", first); !errors.Is(err, ErrNotInControl) {
		t.Errorf("the first person's input during the second's control = %v, want ErrNotInControl", err)
	}
	if f.m.HoldsTurn(s.ID, first) || !f.m.HoldsTurn(s.ID, second) {
		t.Error("HoldsTurn names the wrong takeover")
	}
	if ended, _ := f.m.ReleaseTurn(context.Background(), s.ID, first); ended {
		t.Error("the first person's hand-back ended the second's control")
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Fatal("the second person lost control")
	}
	if got := took(); got != "" {
		t.Errorf("the first person's hand-back reached the browser: %q", got)
	}
	if _, done, err := f.m.HumanInputTurn(s.ID, "person", second); err != nil {
		t.Fatalf("the second person's input = %v", err)
	} else {
		done()
	}
	if ended, err := f.m.ReleaseTurn(context.Background(), s.ID, second); !ended || err != nil {
		t.Errorf("the second person's hand-back = %v, %v", ended, err)
	}
	if got := took(); got != "lift empty" {
		t.Errorf("the second person's hand-back: the browser got %q, want lift, then empty", got)
	}
	if got, _ := f.m.Live("google"); got.HumanControl {
		t.Error("still under a person's control")
	}
	if ended, _ := f.m.ReleaseTurn(context.Background(), s.ID, second); ended || took() != "" {
		t.Error("handed back twice")
	}
}

// syncBuffer is a bytes.Buffer for a logger written from several
// goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// humanInput is HumanInput for whoever holds control now, as the live
// view of the person who took it would send it.
func humanInput(m *Manager, id string) (func(), error) {
	return humanInputAs(m, id)
}

// holderOf is who holds control of session id now, "" for no one.
func holderOf(m *Manager, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.findLocked(id); l != nil {
		return l.holder
	}
	return ""
}

// takeByLink is TakeControlByLink as a handoff's live view does it, with
// the help epoch the browser waits for now.
func takeByLink(m *Manager, id, holder string) error {
	epoch, _ := m.HelpEpoch(id)
	_, err := m.TakeControlTurnByLink(id, holder, epoch)
	return err
}

// humanInputAs is humanInput's body.
func humanInputAs(m *Manager, id string) (func(), error) {
	m.mu.Lock()
	var gen uint64
	if l := m.findLocked(id); l != nil {
		gen = l.humanGen
	}
	m.mu.Unlock()
	_, done, err := m.HumanInputTurn(id, holderOf(m, id), gen)
	return done, err
}

// A hand-back admitted for one session never reaches the next session on
// the same profile, whose browser listens on the same control socket:
// the session's end waits for it, and breaks it off, before the profile
// can start again (PROJ-196).
func TestHandBackNeverReachesTheNextSession(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var first *http.Server // the first session's browser
	var reachedNext []string
	server := func(r *http.Request) *http.Server {
		s, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
		return s
	}
	record := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if server(r) != first {
				reachedNext = append(reachedNext, what)
			}
			mu.Unlock()
			sessionstest.AnswerLift(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if first == nil {
			first = server(r)
		}
		mu.Unlock()
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /input/release", record("lift"))
	mux.HandleFunc("DELETE /input/clipboard", record("empty"))
	f.rt.Control = mux
	s := f.start(f.m)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}

	// A hand-back gets under way: past its checks, with m.mu let go.
	released := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(released) }()
	for {
		f.m.mu.Lock()
		l := f.m.findLocked(s.ID)
		releasing := l != nil && l.releasing
		f.m.mu.Unlock()
		if releasing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Meanwhile the browser stops, and the profile starts again.
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	next := f.start(f.m)
	if next.ID == s.ID {
		t.Fatal("no new session")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the hand-back never returned")
	}
	time.Sleep(3 * clipboardSettle) // anything late would have landed by now
	mu.Lock()
	defer mu.Unlock()
	if len(reachedNext) != 0 {
		t.Errorf("the first session's hand-back reached the next session's browser: %v", reachedNext)
	}
}

// A hand-back waiting on another that is ending the same takeover returns
// once that one has ended, even if someone takes control the moment it
// does: it waits for that takeover's hand-back, not for the next
// person's to end (PROJ-196).
func TestConcurrentHandBackWaitsForItsOwnTakeover(t *testing.T) {
	f := newFixture(t)
	gate := make(chan struct{})
	var lifts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		if lifts.Add(1) == 1 {
			<-gate // the first hand-back is held up here
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fence":` + r.Header.Get(FenceHeader) + `}`))
	})
	f.rt.Control = mux
	s := f.start(f.m)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	first := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(first) }() // a handoff ends, say
	for lifts.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(second) }() // and the live view goes
	time.Sleep(20 * time.Millisecond)
	close(gate)
	<-first
	// The moment the first ends, someone takes control.
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting hand-back is waiting out the next person's takeover")
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Error("the next person lost control")
	}
}

// A Take control waiting for the agent's broken-off input, whose takeover
// the profile page hands back meanwhile, fails: the person never gets a
// generation for control that has already ended (PROJ-196).
func TestTakeControlHandedBackWhileItWaits(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter)
	took := make(chan error, 1)
	go func() {
		_, err := f.m.TakeControlTurn(s.ID, "person")
		took <- err
	}()
	<-ctx.Done() // broken off: the take waits for it to end
	f.m.ReleaseControl(s.ID)
	done()
	select {
	case err := <-took:
		if !errors.Is(err, ErrControlEnded) {
			t.Fatalf("Take control handed back while it waited = %v, want ErrControlEnded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned")
	}
	if got, _ := f.m.Live("google"); got.HumanControl {
		t.Error("still under a person's control")
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input after = %v", err)
	} else {
		done()
	}
}

// A Take control waiting for the agent's broken-off input gives up if a
// stop began meanwhile, even one that failed and put the browser back
// Running before the take woke: as if it had woken first (PROJ-200).
func TestTakeControlGivesUpOnAStopThatFailedBeforeItWoke(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter)
	took := make(chan error, 1)
	go func() {
		_, err := f.m.TakeControlTurn(s.ID, "person")
		took <- err
	}()
	<-ctx.Done() // broken off: the take waits for it to end
	f.rt.Mu.Lock()
	f.rt.StopErr = errors.New("the runtime refused to stop it")
	f.rt.Mu.Unlock()
	if _, err := f.m.Stop(context.Background(), s.ID); err == nil {
		t.Fatal("the stop didn't fail")
	}
	if snap, _ := f.m.Live("google"); snap.State != Running {
		t.Fatalf("after the failed stop: %s", snap.State)
	}
	done() // only now does the take wake
	select {
	case err := <-took:
		if !errors.Is(err, ErrNotRunning) {
			t.Fatalf("Take control across a failed stop = %v, want ErrNotRunning", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned")
	}
	if snap, _ := f.m.Live("google"); snap.HumanControl {
		t.Error("left under a person's control")
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input after = %v", err)
	} else {
		done()
	}
}

// HoldsTurn answers as HumanInput admits: not while the browser is
// stopping, though the takeover hasn't ended (PROJ-196).
func TestInControlWhileStopping(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	gen, err := f.m.TakeControlTurn(s.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	if !f.m.HoldsTurn(s.ID, gen) {
		t.Fatal("not in control after taking it")
	}
	f.rt.Mu.Lock()
	f.rt.StopErr = errors.New("the runtime refused to stop it")
	f.rt.BeforeStop = func(string) {
		_, _, inputErr := f.m.HumanInputTurn(s.ID, "person", gen)
		if f.m.HoldsTurn(s.ID, gen) || inputErr == nil {
			t.Errorf("while stopping: HoldsTurn %v, input %v", f.m.HoldsTurn(s.ID, gen), inputErr)
		}
	}
	f.rt.Mu.Unlock()
	if _, err := f.m.Stop(context.Background(), s.ID); err == nil {
		t.Fatal("the stop didn't fail")
	}
	if !f.m.HoldsTurn(s.ID, gen) {
		t.Error("not in control once the failed stop put the browser back")
	}
}

// A hand-back whose first lift the browser fails tries once more before
// it empties the clipboard: a key or button left down would be the next
// driver's (PROJ-196).
func TestHandBackTriesTheLiftAgain(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var calls []string
	lifts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		lifts++
		calls = append(calls, "lift")
		if lifts == 1 {
			http.Error(w, "xdotool isn't taking input", http.StatusInternalServerError)
			return
		}
		sessionstest.AnswerLift(w, r)
	})
	mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls = append(calls, "empty")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.rt.Control = mux
	s := f.start(f.m)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(calls, " "); got != "lift lift empty" {
		t.Errorf("the browser got %q, want the lift tried again before the clipboard is emptied", got)
	}
}

// A hand-back's warnings say how the browser failed by status alone,
// never in its words, which a faulty or hostile container could fill
// with what its clipboard held (PROJ-196).
func TestHandBackWarningsKeepTheBrowsersWordsOut(t *testing.T) {
	f := newFixture(t)
	var logs syncBuffer
	f.m.log = slog.New(slog.NewTextHandler(&logs, nil))
	const secret = "clip-Secret-91"
	refuse := func(w http.ResponseWriter, _ *http.Request) {
		// The reason phrase and the body both carry it.
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		body := "the clipboard held " + secret
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 500 %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", secret, len(body), body)
		_ = buf.Flush()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	// The lift is refused once (then answered: the hand-back tries until
	// the browser fences the takeover), the emptying every time.
	var lifts atomic.Int32
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		if lifts.Add(1) == 1 {
			refuse(w, r)
			return
		}
		sessionstest.AnswerLift(w, r)
	})
	mux.HandleFunc("DELETE /input/clipboard", refuse)
	f.rt.Control = mux
	s := f.start(f.m)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID)
	out := logs.String()
	if strings.Contains(out, secret) {
		t.Errorf("the browser's words reached the log:\n%s", out)
	}
	if strings.Count(out, "status 500") != 2 {
		t.Errorf("want the lift and the emptying logged by status:\n%s", out)
	}
}

// An operation the person's live view admitted (HumanInput) and that
// paused before sending anything can't reach the browser once their
// control has begun to end: its context ends with the hand-back's start.
// Agents aren't let in until it has returned, and nothing it sends lands
// after (or before) the clipboard is emptied (PROJ-196).
func TestAdmittedInputCantOutliveTheHandBack(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var calls []string
	note := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, what)
			mu.Unlock()
			sessionstest.AnswerLift(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/clipboard", note("set"))
	mux.HandleFunc("POST /input/release", note("lift"))
	mux.HandleFunc("DELETE /input/clipboard", note("empty"))
	f.rt.Control = mux
	s := f.start(f.m)
	gen, err := f.m.TakeControlTurn(s.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	// A clipboard set, admitted, then held up before it posts.
	ctx, done, err := f.m.HumanInputTurn(s.ID, "person", gen)
	if err != nil {
		t.Fatal(err)
	}
	resume := make(chan struct{})
	var resumeOnce sync.Once
	goOn := func() { resumeOnce.Do(func() { close(resume) }) }
	defer goOn()
	sent := make(chan error, 1)
	go func() {
		defer done()
		<-resume
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://control/input/clipboard", strings.NewReader(`{"text":"a secret"}`))
		resp, err := controlClient(f.m.layout.ControlSocket("google")).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		sent <- err
	}()
	released := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(released) }()
	for {
		f.m.mu.Lock()
		releasing := f.m.findLocked(s.ID).releasing
		f.m.mu.Unlock()
		if releasing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		if err == nil {
			done()
		}
		t.Fatalf("agent input while the admitted input is still alive = %v", err)
	}
	select {
	case <-released:
		t.Fatal("handed back with the admitted input still alive")
	default:
	}
	goOn() // it goes on
	if err := <-sent; err == nil {
		t.Error("the admitted input reached the browser after the hand-back began")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the hand-back never finished")
	}
	mu.Lock()
	got := strings.Join(calls, " ")
	mu.Unlock()
	if got != "lift empty" {
		t.Errorf("the browser got %q, want only the hand-back's lift and emptying", got)
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input after = %v", err)
	} else {
		done()
	}
}

// A handoff's Take control names the Needs help it was opened in: once
// that has ended, it takes nothing, in the same step, and an agent's
// long action goes on (PROJ-196).
func TestTakeControlInAnEndedHandoff(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	epoch, ok := f.m.HelpEpoch(s.ID)
	if !ok || epoch == 0 {
		t.Fatalf("HelpEpoch = %d, %v", epoch, ok)
	}
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil { // the handoff ends
		t.Fatal(err)
	}
	if _, ok := f.m.HelpEpoch(s.ID); ok {
		t.Error("a help epoch while Running")
	}
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	advance(takeOverAfter) // long enough to be broken off by a take
	if _, err := f.m.TakeControlTurnByLink(s.ID, "person", epoch); !errors.Is(err, ErrHandoffEnded) {
		t.Fatalf("a take for the ended handoff = %v, want ErrHandoffEnded", err)
	}
	if ctx.Err() != nil {
		t.Error("the agent's action was broken off")
	}
	if got, _ := f.m.Live("google"); got.HumanControl {
		t.Error("taken")
	}
	// A later handoff has another epoch: the old one's take fails there too.
	done()
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.TakeControlTurnByLink(s.ID, "person", epoch); !errors.Is(err, ErrHandoffEnded) {
		t.Errorf("the old handoff's take in a new one = %v, want ErrHandoffEnded", err)
	}
	next, _ := f.m.HelpEpoch(s.ID)
	if _, err := f.m.TakeControlTurnByLink(s.ID, "person", next); err != nil {
		t.Errorf("the open handoff's take = %v", err)
	}
}

// A hand-back lets agents in only once the browser has fenced the
// takeover: a lift it fails is tried again, a little later each time,
// and agents wait meanwhile (PROJ-196).
func TestHandBackWaitsForTheFence(t *testing.T) {
	f := newFixture(t)
	var lifts atomic.Int32
	answer := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		lifts.Add(1)
		if r.Header.Get(FenceHeader) == "" {
			t.Error("a hand-back's lift without a fence")
		}
		select {
		case <-answer:
			sessionstest.AnswerLift(w, r)
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	})
	f.rt.Control = mux
	s := f.start(f.m)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(released) }()
	for deadline := time.Now().Add(5 * time.Second); lifts.Load() < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		if err == nil {
			done()
		}
		t.Fatalf("agent input before the fence = %v", err)
	}
	select {
	case <-released:
		t.Fatal("handed back without the fence")
	default:
	}
	close(answer)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the hand-back never finished")
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input after = %v", err)
	} else {
		done()
	}
}

// An operation of the person's that never says done doesn't hold the
// browser for good: once the browser has fenced the takeover, it can do
// nothing there, and the hand-back goes on after inputWait, without
// stopping anything (PROJ-196).
func TestHandBackGoesOnOnceFenced(t *testing.T) {
	was := inputWait
	inputWait = 200 * time.Millisecond
	t.Cleanup(func() { inputWait = was })
	f := newFixture(t)
	s := f.start(f.m)
	gen, err := f.m.TakeControlTurn(s.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.HumanInputTurn(s.ID, "person", gen); err != nil { // and never done
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { f.m.ReleaseControl(s.ID); close(released) }()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the hand-back is held by an input that never ends")
	}
	if got, _ := f.m.Live("google"); got.State != Running || got.HumanControl {
		t.Fatalf("after: %s, person in control %v", got.State, got.HumanControl)
	}
	f.rt.Mu.Lock()
	stops := f.rt.Stops
	f.rt.Mu.Unlock()
	if stops != 0 {
		t.Errorf("the browser was stopped %d times", stops)
	}
}

// After BrowserPier restarts, a browser waiting for help keeps its help
// epoch, never 0, so its handoff's view can only take it while that help
// lasts; the browser is fenced up to the highest takeover saved before
// the restart, not "whatever it has seen", and the takeovers after it
// number above that (PROJ-196).
func TestReconcileFencesAndNumbersAbove(t *testing.T) {
	f := newFixture(t)
	var fences []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fences = append(fences, r.Header.Get(FenceHeader))
		mu.Unlock()
		sessionstest.AnswerLift(w, r)
	})
	f.rt.Control = mux
	s := f.start(f.m)
	epoch, err := f.m.OpenHelp(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	openHandoffRow(t, f.m, s, epoch)
	last, err := f.m.TakeControlTurnByLink(s.ID, "person", epoch)
	if err != nil {
		t.Fatal(err)
	}
	f.m.Close() // with the person's control under way

	m := f.manager()
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if want := strconv.FormatUint(last, 10); len(fences) == 0 || fences[len(fences)-1] != want {
		t.Errorf("the restart's lift fenced %v, want %s", fences, want)
	}
	mu.Unlock()
	got, ok := m.HelpEpoch(s.ID)
	if !ok || got != epoch {
		t.Fatalf("HelpEpoch after the restart = %d, %v, want %d", got, ok, epoch)
	}
	if _, err := m.TakeControlTurnByLink(s.ID, "person", 0); !errors.Is(err, ErrHandoffEnded) {
		t.Errorf("a handoff's take with no epoch = %v, want ErrHandoffEnded", err)
	}
	gen, err := m.TakeControlTurnByLink(s.ID, "person", epoch)
	if err != nil {
		t.Fatal(err)
	}
	if gen <= last {
		t.Errorf("the first takeover after the restart is %d, at or below the last one before it, %d", gen, last)
	}
}

// openHandoffRow records an open handoff for s, as internal/handoff does,
// which opened Needs help as epoch.
func openHandoffRow(t *testing.T, m *Manager, s Session, epoch uint64) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := m.db.Exec(`INSERT INTO handoffs (id, session_id, profile, agent, reason, detail, origin, policy, status, token_hash, created_at, expires_at, help_epoch)
		VALUES (?, ?, ?, 'bot', 'login', '', '', 'link', 'pending', ?, ?, ?, ?)`, "h_"+s.ID, s.ID, s.Profile, "t_"+s.ID, now, now+900000, epoch); err != nil {
		t.Fatal(err)
	}
}

// A browser left waiting for help whose handoff had ended as BrowserPier
// went (between the two) is the agents' again after the restart, once it
// is fenced (PROJ-196).
func TestReconcileEndsHelpNoHandoffAsksFor(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	if _, err := f.m.OpenHelp(s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.Close() // and the handoff's row says it ended, or never was

	m := f.manager()
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Live("google"); got.State != Running {
		t.Fatalf("after the restart: %s", got.State)
	}
	if _, done, err := m.AgentInput(context.Background(), s.ID, "tok"); err != nil {
		t.Fatalf("agent input = %v", err)
	} else {
		done()
	}
}

// A handoff's end ends the control of the person helping, taken while
// the browser waited for help, in the same step as the browser stops
// waiting; never a takeover begun after that (an admin's on the profile
// page), even by an end that comes late (PROJ-196).
func TestEndHandoffEndsOnlyTheHelpersControl(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	epoch, err := f.m.OpenHelp(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.OpenHelp(s.ID); !errors.Is(err, ErrHelpOpen) {
		t.Errorf("a second handoff's help = %v, want ErrHelpOpen", err)
	}
	if err := f.m.TakeControl(s.ID, "person"); err != nil { // the person helping
		t.Fatal(err)
	}
	done, err := f.m.EndHandoff(s.ID, epoch)
	if err != nil || done == nil {
		t.Fatalf("EndHandoff = %v, %v", done, err)
	}
	<-done
	if got, _ := f.m.Live("google"); got.State != Running || got.HumanControl {
		t.Fatalf("after the handoff: %s, person in control %v", got.State, got.HumanControl)
	}
	// Someone takes control once the browser is the agents' again; the
	// handoff's end, come again late, leaves them be.
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	if done, err := f.m.EndHandoff(s.ID, epoch); done != nil || !errors.Is(err, ErrHandoffEnded) {
		t.Errorf("the late end = %v, %v, want ErrHandoffEnded", done, err)
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Error("a takeover begun after the handoff was ended by it")
	}
	// Nor does another handoff's end touch this one's help.
	f.m.ReleaseControl(s.ID)
	next, err := f.m.OpenHelp(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.EndHandoff(s.ID, epoch); !errors.Is(err, ErrHandoffEnded) {
		t.Errorf("the old handoff's end in a new one = %v, want ErrHandoffEnded", err)
	}
	if got, _ := f.m.Live("google"); got.State != NeedsHelp {
		t.Errorf("the new handoff's help ended by the old one's: %s", got.State)
	}
	if _, err := f.m.EndHandoff(s.ID, next); err != nil {
		t.Fatal(err)
	}
}

// A browser picked up after BrowserPier restarts that doesn't fence its
// takeovers in time is stopped, not handed to agents with whatever a
// person's control left under way in it (PROJ-196).
func TestReconcileStopsABrowserItCantFence(t *testing.T) {
	was := reconcileFenceWait
	reconcileFenceWait = 300 * time.Millisecond
	t.Cleanup(func() { reconcileFenceWait = was })
	f := newFixture(t)
	var refuse atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		sessionstest.AnswerLift(w, r)
	})
	f.rt.Control = mux
	s := f.start(f.m)
	f.m.Close()
	refuse.Store(true)

	m := f.manager()
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Live("google"); ok {
		t.Fatalf("the unfenced browser runs on: %s", got.State)
	}
	if got := f.stored(s.ID); got.State != Stopped {
		t.Errorf("saved as %s", got.State)
	}
}

// Only a real fence counts: a browser that answers a fenced lift with
// 204, an empty answer or a lower fence (an entrypoint from before the
// fence) hasn't fenced the takeover, so agents stay out, and the
// hand-back goes on trying; a restart stops such a browser (PROJ-196).
func TestFenceNeedsARealAnswer(t *testing.T) {
	for _, answer := range []string{"204", "{}", `{"fence":0}`} {
		t.Run(answer, func(t *testing.T) {
			f := newFixture(t)
			var real atomic.Bool
			var lifts atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
			mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
				lifts.Add(1)
				switch {
				case real.Load():
					sessionstest.AnswerLift(w, r)
				case answer == "204":
					w.WriteHeader(http.StatusNoContent)
				default:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(answer))
				}
			})
			f.rt.Control = mux
			s := f.start(f.m)
			if err := f.m.TakeControl(s.ID, "person"); err != nil {
				t.Fatal(err)
			}
			released := make(chan struct{})
			go func() { f.m.ReleaseControl(s.ID); close(released) }()
			for deadline := time.Now().Add(5 * time.Second); lifts.Load() < 3 && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case <-released:
				t.Fatalf("handed back on a lift answered %s", answer)
			default:
			}
			if _, done, err := f.m.AgentInput(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
				if err == nil {
					done()
				}
				t.Fatalf("agent input with the takeover unfenced = %v", err)
			}
			real.Store(true)
			select {
			case <-released:
			case <-time.After(10 * time.Second):
				t.Fatal("the hand-back never finished")
			}

			// A restart with the browser answering the same way stops it.
			real.Store(false)
			f.m.Close()
			was := reconcileFenceWait
			reconcileFenceWait = 300 * time.Millisecond
			t.Cleanup(func() { reconcileFenceWait = was })
			m := f.manager()
			if err := m.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got, ok := m.Live("google"); ok {
				t.Errorf("a browser that doesn't fence runs on after the restart: %s", got.State)
			}
		})
	}
}

// Every cleanup names the takeover it is for: the lift and the
// clipboard's emptying alike, so the browser can leave a later takeover
// alone (PROJ-196); and the takeover's number is saved before it is
// handed out.
func TestCleanupNamesItsTakeover(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	got := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got["lift"] = r.Header.Get(FenceHeader)
		mu.Unlock()
		sessionstest.AnswerLift(w, r)
	})
	mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got["empty"] = r.Header.Get(FenceHeader)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.rt.Control = mux
	s := f.start(f.m)
	gen, err := f.m.TakeControlTurn(s.ID, "person")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := getSession(context.Background(), f.m.db, s.ID)
	if err != nil || stored.takeoverGen != gen {
		t.Fatalf("saved takeover %d, %v; handed out %d", stored.takeoverGen, err, gen)
	}
	f.m.ReleaseControl(s.ID)
	want := strconv.FormatUint(gen, 10)
	mu.Lock()
	defer mu.Unlock()
	if got["lift"] != want || got["empty"] != want {
		t.Errorf("the lift named %q and the emptying %q, want %s", got["lift"], got["empty"], want)
	}
}

// A hand-back the browser never answers doesn't keep the browser from
// being stopped: by the admin, or as idle, whoever watches it; stopping
// it ends the hand-back (PROJ-196).
func TestStuckHandBackCanBeStopped(t *testing.T) {
	for _, how := range []string{"admin", "idle"} {
		t.Run(how, func(t *testing.T) {
			f := newFixture(t)
			advance := f.clock()
			f.profile("wedged", 5*time.Minute)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
			mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "wedged", http.StatusServiceUnavailable)
			})
			f.rt.Control = mux
			s := f.startProfile("wedged")
			if _, err := f.m.Watch(s.ID); err != nil { // someone watches
				t.Fatal(err)
			}
			if err := f.m.TakeControl(s.ID, "person"); err != nil {
				t.Fatal(err)
			}
			released := make(chan struct{})
			go func() { f.m.ReleaseControl(s.ID); close(released) }()
			time.Sleep(100 * time.Millisecond)
			switch how {
			case "admin":
				if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
					t.Fatal(err)
				}
			case "idle":
				f.m.mu.Lock() // the hand-back reads the clock meanwhile
				advance(6 * time.Minute)
				f.m.mu.Unlock()
				f.m.stopIdle(context.Background())
			}
			select {
			case <-released:
			case <-time.After(10 * time.Second):
				t.Fatal("the hand-back outlived the stop")
			}
			if got, ok := f.m.Live("wedged"); ok {
				t.Errorf("still running: %s", got.State)
			}
		})
	}
}

// A browser waiting for help since before help epochs were saved (0)
// gets one after a restart, saved, and its open handoff's row is given
// the same, so that handoff's end ends it (PROJ-196).
func TestReconcileGivesTheOpenHandoffItsEpoch(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	if _, err := f.m.OpenHelp(s.ID); err != nil {
		t.Fatal(err)
	}
	openHandoffRow(t, f.m, s, 0)
	if _, err := f.m.db.Exec(`UPDATE sessions SET help_epoch = 0 WHERE id = ?`, s.ID); err != nil {
		t.Fatal(err)
	}
	f.m.Close()

	m := f.manager()
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	epoch, ok := m.HelpEpoch(s.ID)
	if !ok || epoch == 0 {
		t.Fatalf("HelpEpoch after the restart = %d, %v", epoch, ok)
	}
	var row uint64
	if err := m.db.QueryRow(`SELECT help_epoch FROM handoffs WHERE session_id = ? AND status = 'pending'`, s.ID).Scan(&row); err != nil || row != epoch {
		t.Fatalf("the open handoff's epoch %d, %v; the browser's %d", row, err, epoch)
	}
	if _, err := m.EndHandoff(s.ID, row); err != nil {
		t.Fatalf("ending the handoff = %v", err)
	}
	if got, _ := m.Live("google"); got.State != Running {
		t.Errorf("after the handoff: %s", got.State)
	}
}

// Revoking a token breaks off its input action under way (a long
// human-paced type), as it does its page calls; another token's
// revocation leaves it be (PROJ-166).
func TestRevokingATokenCancelsItsInput(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID, "A")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if got := f.m.ConnectedTokens(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("ConnectedTokens = %v; the revocation watcher wouldn't look at it", got)
	}
	f.m.DisconnectToken("B")
	select {
	case <-ctx.Done():
		t.Fatal("another token's revocation cancelled the input")
	default:
	}
	f.m.DisconnectToken("A")
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the revocation didn't cancel the input")
	}
	if err := context.Cause(ctx); !errors.Is(err, ErrRevoked) {
		t.Fatalf("cause = %v", err)
	}
}
