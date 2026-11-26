package sessions

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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
	if done, err := f.m.HumanInput(s.ID, "person"); err != nil {
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
	if _, err := f.m.HumanInput(s.ID, "person"); !errors.Is(err, ErrNotRunning) {
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
	if done, err := f.m.HumanInput(s.ID, phone); err != nil {
		t.Fatal("the holder isn't in control")
	} else {
		done()
	}
	for _, other := range []string{admin, "", HandoffHolder(""), HandoffHolder("h2")} {
		if f.m.ControlledBy(s.ID, other) {
			t.Fatalf("%q counts as in control while %q holds it", other, phone)
		}
		if _, err := f.m.HumanInput(s.ID, other); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("input from %q = %v, want ErrNotRunning", other, err)
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
	for _, next := range []string{HandoffHolder("h1"), AdminHolder} {
		turn, err := f.m.TakeControlTurn(s.ID, next)
		if err != nil {
			t.Fatal(err)
		}
		if f.m.HoldsTurn(s.ID, old) || f.m.ReleaseTurn(s.ID, old) {
			t.Fatalf("an old turn held or released %q's", next)
		}
		if !f.m.ControlledBy(s.ID, next) || !f.m.HoldsTurn(s.ID, turn) {
			t.Fatalf("%q lost control to an old turn", next)
		}
		if !f.m.ReleaseTurn(s.ID, turn) || f.m.ControlledBy(s.ID, next) {
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
		return func(w http.ResponseWriter, _ *http.Request) {
			h := f.m.HoldsTurn(id, turn)
			mu.Lock()
			calls = append(calls, name)
			held = append(held, h)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}
	rec := record("release")
	mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, r *http.Request) {
		// The hand-back waits for the releases to run (PROJ-195).
		if r.URL.RawQuery != "drain=1" {
			t.Errorf("the hand-back's release: ?%s", r.URL.RawQuery)
		}
		rec(w, r)
	})
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
	if _, err := f.m.HumanInputTurn(s.ID, AdminHolder, old); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("input from the old turn = %v", err)
	}
	if done, err := f.m.HumanInputTurn(s.ID, AdminHolder, turn); err != nil {
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
		if done, err := f.m.HumanInput(id, "person"); err == nil {
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
			if !errors.Is(e.agentInput, ErrHumanDriving) || !errors.Is(e.attach, ErrHumanDriving) || !errors.Is(e.pageLease, ErrHumanDriving) || !errors.Is(e.humanInput, ErrNotRunning) {
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
	done, err := f.m.HumanInput(s.ID, "person")
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
