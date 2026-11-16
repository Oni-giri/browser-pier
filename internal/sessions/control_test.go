package sessions

import (
	"context"
	"errors"
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
	if err := f.m.HumanInput(s.ID); err != nil {
		t.Fatal(err)
	}
	// While a person drives, agents can only watch.
	if _, _, err := f.m.AgentInput(context.Background(), s.ID); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("agent input while a person drives = %v", err)
	}
	if _, _, err := f.m.Attach(ctx, s.ID, "t", "claude"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("CDP while a person drives = %v", err)
	}
	f.m.ReleaseControl(s.ID)
	if err := f.m.HumanInput(s.ID); !errors.Is(err, ErrNotRunning) {
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
