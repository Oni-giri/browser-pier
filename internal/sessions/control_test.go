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
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while the agent clicks = %v", err)
	}
	advance(agentQuiet + time.Second)
	// So is one connected over CDP.
	_, release, err := f.m.Attach(ctx, s.ID, "t", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while an agent is connected = %v", err)
	}
	release()

	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Fatal("HumanControl not shown")
	}
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("a second person taking control = %v, want ErrHumanDriving", err)
	}
	if done, err := humanInput(f.m, s.ID); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
	// While a person drives, agents can only watch.
	if _, _, err := f.m.AgentInput(context.Background(), s.ID); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("agent input while a person drives = %v", err)
	}
	if _, _, err := f.m.Attach(ctx, s.ID, "t", "claude"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("CDP while a person drives = %v", err)
	}
	f.m.ReleaseControl(s.ID)
	if _, err := humanInput(f.m, s.ID); !errors.Is(err, ErrNotInControl) {
		t.Fatalf("human input after release = %v", err)
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID); err != nil {
		t.Fatalf("agent input after hand-back = %v", err)
	} else {
		done()
	}
}

// A handoff waits for agent input already on its way, and refuses more:
// no agent click lands while the person has the browser.
func TestHandoffWaitsForInputInFlight(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID)
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
	if _, _, err := f.m.AgentInput(context.Background(), s.ID); !errors.Is(err, ErrHumanDriving) {
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
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter - time.Second)
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take while the agent types = %v", err)
	}
	done()
	advance(agentQuiet - time.Second)
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take %v after the typing ended = %v", agentQuiet-time.Second, err)
	}
	advance(2 * time.Second)
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatalf("take after the quiet = %v", err)
	}
}

// One agent input runs at a time in a session: a second while one is
// under way is refused, and allowed once it ends.
func TestOneAgentInputAtATime(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.AgentInput(context.Background(), s.ID); !errors.Is(err, ErrInputBusy) {
		t.Fatalf("a second input while one runs = %v, want ErrInputBusy", err)
	}
	done()
	_, done, err = f.m.AgentInput(context.Background(), s.ID)
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
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter)
	took := make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID) }()
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
	if _, _, err := f.m.AgentInput(context.Background(), s.ID); !errors.Is(err, ErrHumanDriving) && !errors.Is(err, ErrInputBusy) {
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
	if _, _, err := f.m.AgentInput(context.Background(), s.ID); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("agent input after Take control = %v", err)
	}
}

// During a handoff's first moments, while the input it cancelled is still
// letting go, the person taking control waits for it too.
func TestTakeControlInAHandoffWaitsForInput(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = f.m.SetNeedsHelp(s.ID, true) }()
	<-ctx.Done()
	took := make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID) }()
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
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID)
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
	in, done, err := f.m.AgentInput(context.Background(), s.ID)
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
		if _, done, err := f.m.AgentInput(context.Background(), id); err == nil {
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
		if err := f.m.TakeControl(s.ID); err != nil {
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
		if _, done, err := f.m.AgentInput(context.Background(), s.ID); err != nil {
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
// they took (TakeControlGen): once it has ended without them and someone
// else has taken the browser, they neither reach it nor end that
// takeover; their own hand-back lifts what they held before the
// clipboard is emptied (PROJ-196).
func TestControlBelongsToItsTakeover(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var calls []string
	note := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls = append(calls, what)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
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

	first, err := f.m.TakeControlGen(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.m.ReleaseControl(s.ID) // ended from outside: a handoff, the profile page
	second, err := f.m.TakeControlGen(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("both takeovers are generation %d", first)
	}
	took()
	if _, _, err := f.m.HumanInput(s.ID, first); !errors.Is(err, ErrNotInControl) {
		t.Errorf("the first person's input during the second's control = %v, want ErrNotInControl", err)
	}
	if f.m.InControl(s.ID, first) || !f.m.InControl(s.ID, second) {
		t.Error("InControl names the wrong takeover")
	}
	if f.m.ReleaseControlGen(s.ID, first) {
		t.Error("the first person's hand-back ended the second's control")
	}
	if got, _ := f.m.Live("google"); !got.HumanControl {
		t.Fatal("the second person lost control")
	}
	if got := took(); got != "" {
		t.Errorf("the first person's hand-back reached the browser: %q", got)
	}
	if _, done, err := f.m.HumanInput(s.ID, second); err != nil {
		t.Fatalf("the second person's input = %v", err)
	} else {
		done()
	}
	if !f.m.ReleaseControlGen(s.ID, second) {
		t.Error("the second person's hand-back did nothing")
	}
	if got := took(); got != "lift empty" {
		t.Errorf("the second person's hand-back: the browser got %q, want lift, then empty", got)
	}
	if got, _ := f.m.Live("google"); got.HumanControl {
		t.Error("still under a person's control")
	}
	if f.m.ReleaseControlGen(s.ID, second) || took() != "" {
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
	m.mu.Lock()
	var gen uint64
	if l := m.findLocked(id); l != nil {
		gen = l.humanGen
	}
	m.mu.Unlock()
	_, done, err := m.HumanInput(id, gen)
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
			w.WriteHeader(http.StatusNoContent)
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
	if err := f.m.TakeControl(s.ID); err != nil {
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
// does: it doesn't wait out the next person's takeover (PROJ-196).
func TestConcurrentHandBackWaitsForItsOwnTakeover(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	first, err := f.m.TakeControlGen(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Another hand-back is ending it (let go of m.mu to empty the
	// clipboard).
	f.m.mu.Lock()
	l := f.m.findLocked(s.ID)
	l.releasing = true
	f.m.mu.Unlock()
	waited := make(chan bool, 1)
	go func() { waited <- f.m.ReleaseControlGen(s.ID, first) }()
	time.Sleep(50 * time.Millisecond) // it waits on the other
	// That one ends, and in the same step someone else takes control,
	// before the waiter gets m.mu back.
	f.m.mu.Lock()
	l.releasing = false
	l.generation++
	l.humanGen = l.generation
	f.m.inputsDone.Broadcast()
	f.m.mu.Unlock()
	select {
	case ended := <-waited:
		if ended {
			t.Error("the waiting hand-back says it ended the takeover")
		}
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
	ctx, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	advance(takeOverAfter)
	took := make(chan error, 1)
	go func() {
		_, err := f.m.TakeControlGen(s.ID)
		took <- err
	}()
	<-ctx.Done() // broken off: the take waits for it to end
	f.m.ReleaseControl(s.ID)
	done()
	select {
	case err := <-took:
		if !errors.Is(err, ErrNotInControl) {
			t.Fatalf("Take control handed back while it waited = %v, want ErrNotInControl", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned")
	}
	if got, _ := f.m.Live("google"); got.HumanControl {
		t.Error("still under a person's control")
	}
	if _, done, err := f.m.AgentInput(context.Background(), s.ID); err != nil {
		t.Fatalf("agent input after = %v", err)
	} else {
		done()
	}
}
