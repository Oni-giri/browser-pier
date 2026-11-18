package sessions

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPageLeasesAreNotConnections(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	ls, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.m.Live("google"); len(got.InUseBy) != 0 {
		t.Errorf("a page call shows as a connection: %v", got.InUseBy)
	}
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take control during a page call = %v", err)
	}
	ls.Release()
	ls.Release() // twice is fine
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take control just after a page call = %v", err)
	}
	advance(agentQuiet + time.Second)
	gen := ls.Generation
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.PageLease(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("a page call while a person drives = %v", err)
	}
	f.m.ReleaseControl(s.ID)
	ls, err = f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Release()
	if ls.Generation == gen {
		t.Error("the generation didn't change when a person took the browser")
	}
}

// leaseEnds waits for a lease's context to end and returns its cause.
func leaseEnds(t *testing.T, ls *Lease) error {
	t.Helper()
	select {
	case <-ls.Ctx.Done():
		return context.Cause(ls.Ctx)
	case <-time.After(5 * time.Second):
		t.Fatal("the lease wasn't cancelled")
		return nil
	}
}

func TestAHandoffCancelsPageCallsAndWaitsForThem(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ls, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan time.Time, 1)
	go func() {
		if err := leaseEnds(t, ls); !errors.Is(err, ErrHandedOver) {
			t.Errorf("cause = %v", err)
		}
		time.Sleep(100 * time.Millisecond) // an act letting go of its button
		released <- time.Now()
		ls.Release()
	}()
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	if at := <-released; opened.Before(at) {
		t.Error("the person got the browser before the page call ended")
	}
	if _, err := f.m.PageLease(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHandedOver) {
		t.Fatalf("a page call during a handoff = %v", err)
	}
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	ls2, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer ls2.Release()
	if ls2.Generation == ls.Generation {
		t.Error("the generation didn't change across the handoff")
	}
}

func TestRevokingATokenCancelsItsPageCalls(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ls, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Release()
	if got := f.m.ConnectedTokens(); len(got) != 1 || got[0] != "tok" {
		t.Fatalf("ConnectedTokens = %v; the revocation watcher wouldn't look at it", got)
	}
	f.m.DisconnectToken("other")
	select {
	case <-ls.Ctx.Done():
		t.Fatal("another token's revocation cancelled the call")
	default:
	}
	f.m.DisconnectToken("tok")
	if err := leaseEnds(t, ls); !errors.Is(err, ErrRevoked) {
		t.Fatalf("cause = %v", err)
	}
}

func TestASessionHoldRefusesAndCancelsPageCalls(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ls, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = leaseEnds(t, ls)
		ls.Release()
	}()
	release, err := f.m.HoldSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := context.Cause(ls.Ctx); !errors.Is(err, ErrSessionHeld) {
		t.Fatalf("cause = %v", err)
	}
	if _, err := f.m.PageLease(context.Background(), s.ID, "tok"); !errors.Is(err, ErrSessionHeld) {
		t.Fatalf("a page call during a hold = %v", err)
	}
	release()
	ls2, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatalf("after the hold: %v", err)
	}
	ls2.Release()
}

func TestStoppingCancelsPageCalls(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ls, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = leaseEnds(t, ls)
		ls.Release()
	}()
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if err := context.Cause(ls.Ctx); !errors.Is(err, ErrStopping) {
		t.Fatalf("cause = %v", err)
	}
	if _, err := f.m.PageLease(context.Background(), s.ID, "tok"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a page call after the stop = %v", err)
	}
}

// The browser exits while Stop waits for a page call to end: Stop then
// has nothing left to stop, and doesn't ask Docker to.
func TestABrowserThatExitsWhileStopWaitsIsntStoppedAgain(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	ls, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Session, 1)
	go func() {
		got, err := f.m.Stop(context.Background(), s.ID)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	_ = leaseEnds(t, ls) // Stop has cancelled it, and waits
	f.rt.Exit(s.ContainerID, 0)
	eventually(t, "the session to end", func() bool { _, live := f.m.Live("google"); return !live })
	ls.Release()
	select {
	case got := <-done:
		if got.State != Stopped {
			t.Errorf("Stop returned %s", got.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop didn't return")
	}
	f.rt.Mu.Lock()
	stops := f.rt.Stops
	f.rt.Mu.Unlock()
	if stops != 0 {
		t.Errorf("Docker was asked to stop an exited browser %d times", stops)
	}
}

func TestPageCallsTakeTurns(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	first, err := f.m.PageLease(context.Background(), s.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *Lease)
	go func() {
		ls, err := f.m.PageLease(context.Background(), s.ID, "b")
		if err != nil {
			t.Error(err)
		}
		got <- ls
	}()
	select {
	case <-got:
		t.Fatal("a second call started while the first was under way")
	case <-time.After(100 * time.Millisecond):
	}
	first.Release()
	second := <-got
	second.Release()
	// A caller that gives up waiting doesn't take its turn.
	held, _ := f.m.PageLease(context.Background(), s.ID, "a")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.m.PageLease(ctx, s.ID, "b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting with a deadline = %v", err)
	}
	held.Release()
	again, err := f.m.PageLease(context.Background(), s.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	again.Release()
}

// TestPageCallsKeepABrowserFromIdling is the idle auto-stop with and
// without page calls, on a fake clock.
func TestPageCallsKeepABrowserFromIdling(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	advance := f.clock()
	f.profile("used", 5*time.Minute)
	f.profile("unused", 5*time.Minute)
	used, _ := f.startProfile("used"), f.startProfile("unused")
	// A call every 3 minutes, one of them a long one across the check.
	for range 3 {
		advance(3 * time.Minute)
		ls, err := f.m.PageLease(ctx, used.ID, "tok")
		if err != nil {
			t.Fatal(err)
		}
		advance(3 * time.Minute)
		f.m.stopIdle(ctx) // mid-call: never idle
		ls.Release()
	}
	if _, ok := f.m.Live("used"); !ok {
		t.Fatal("a browser with page calls was stopped as idle")
	}
	if _, ok := f.m.Live("unused"); ok {
		t.Fatal("a browser with no calls for 18 minutes is still running")
	}
	advance(6 * time.Minute)
	f.m.stopIdle(ctx)
	if _, ok := f.m.Live("used"); ok {
		t.Fatal("still running 6 minutes after its last call")
	}
}

// A person can take control from a page call marked Breakable (act) once
// it has run for takeOverAfter, as from a long input action: it breaks
// off and they get the browser once it has ended. Before that, and for a
// call never marked (a read), Take control is refused.
func TestTakeControlBreaksOffABreakablePageCall(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)

	// A read, however long it runs, is never broken off.
	read, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	advance(time.Minute)
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take control during a long read = %v", err)
	}
	read.Release()

	act, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	act.Breakable()
	advance(takeOverAfter - time.Millisecond)
	if err := f.m.TakeControl(s.ID); !errors.Is(err, ErrAgentDriving) {
		t.Fatalf("take control in act's first %s = %v", takeOverAfter, err)
	}
	advance(time.Millisecond)
	took := make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID) }()
	if err := leaseEnds(t, act); !errors.Is(err, ErrTookControl) {
		t.Fatalf("act's lease ended for %v", err)
	}
	// The person gets the browser only once act has let go.
	select {
	case err := <-took:
		t.Fatalf("Take control returned (%v) while act was still under way", err)
	case <-time.After(200 * time.Millisecond):
	}
	act.Release()
	select {
	case err := <-took:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Take control never returned after act ended")
	}
	if _, err := f.m.PageLease(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("a page call after Take control = %v", err)
	}
}

// TestReadLeasesDontKeepAPersonOut: Visual mode's tree reads (PROJ-150)
// take turns with page calls, but neither start nor extend the seconds
// after an agent's input in which Take control is refused (PROJ-134),
// and Take control breaks one off at once.
func TestReadLeasesDontKeepAPersonOut(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	read, err := f.m.ReadLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	// It takes turns with a page call.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, err := f.m.PageLease(ctx, s.ID, "tok"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a page call during a read = %v, want it to wait", err)
	}
	cancel()
	took := make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID) }()
	if err := leaseEnds(t, read); !errors.Is(err, ErrTookControl) {
		t.Fatalf("the read ended for %v", err)
	}
	read.Release()
	if err := <-took; err != nil {
		t.Fatalf("Take control during a read = %v", err)
	}
	if _, err := f.m.ReadLease(context.Background(), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatalf("a read while a person drives = %v", err)
	}
	f.m.ReleaseControl(s.ID)

	// Reads after an agent's last input don't extend its quiet time.
	_, done, err := f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	for range 3 {
		advance(2 * time.Second)
		r, err := f.m.ReadLease(context.Background(), s.ID, "tok")
		if err != nil {
			t.Fatal(err)
		}
		r.Release()
	}
	if err := f.m.TakeControl(s.ID); err != nil {
		t.Fatalf("take control %s after the last input, with reads since = %v", 6*time.Second, err)
	}
	f.m.ReleaseControl(s.ID)

	// A read under way doesn't cut the quiet time short either: right
	// after an input, Take control is refused, and the read goes on.
	_, done, err = f.m.AgentInput(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	done()
	advance(time.Second)
	read, err = f.m.ReadLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	took = make(chan error, 1)
	go func() { took <- f.m.TakeControl(s.ID) }()
	select {
	case err := <-took:
		if !errors.Is(err, ErrAgentDriving) {
			t.Fatalf("take control 1 s after an input, with a read under way = %v", err)
		}
	case <-time.After(2 * time.Second):
		read.Release()
		t.Fatal("take control 1 s after an input broke the read off and waited for it, instead of being refused")
	}
	if read.Ctx.Err() != nil {
		t.Fatal("the refused Take control broke the read off")
	}
	read.Release()

	// A handoff and the token's revocation end a read as a page call.
	read, err = f.m.ReadLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = f.m.SetNeedsHelp(s.ID, true) }()
	if err := leaseEnds(t, read); !errors.Is(err, ErrHandedOver) {
		t.Fatalf("a handoff ended the read for %v", err)
	}
	read.Release()
}
