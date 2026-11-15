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

// A long input action (typing at human pace) keeps a person from taking
// control from the live view while it lasts and for agentQuiet after its
// end, not its start.
func TestLongInputKeepsTheAgentDriving(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Minute)
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
