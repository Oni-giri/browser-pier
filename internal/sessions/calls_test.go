package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidCallID(t *testing.T) {
	for _, id := range []string{"a", strings.Repeat("x", 64), "submit-order_2", "A-Z_09"} {
		if !ValidCallID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", strings.Repeat("x", 65), "a b", "é", "a/b", "a.b", "a\n", "a+b"} {
		if ValidCallID(id) {
			t.Errorf("%q taken", id)
		}
	}
}

func TestFingerprint(t *testing.T) {
	fp := func(tool, body string) Fingerprint { return FingerprintOf(tool, []byte(body)) }
	base := fp("act", `{"steps":[{"action":"click","ref":"a1"},{"action":"press","keys":"Enter"}],"n":1}`)
	if got := fp("act", "{\n  \"n\": 1,\n  \"steps\": [ {\"ref\": \"a1\", \"action\": \"click\"}, {\"keys\": \"Enter\", \"action\": \"press\"} ]\n}\n"); !got.equal(base) {
		t.Error("key order and whitespace change the fingerprint")
	}
	for what, other := range map[string]Fingerprint{
		"another route":       fp("navigate", `{"steps":[{"action":"click","ref":"a1"},{"action":"press","keys":"Enter"}],"n":1}`),
		"another value":       fp("act", `{"steps":[{"action":"click","ref":"a2"},{"action":"press","keys":"Enter"}],"n":1}`),
		"1 against 1.0":       fp("act", `{"steps":[{"action":"click","ref":"a1"},{"action":"press","keys":"Enter"}],"n":1.0}`),
		"another array order": fp("act", `{"steps":[{"action":"press","keys":"Enter"},{"action":"click","ref":"a1"}],"n":1}`),
		"another field":       fp("act", `{"steps":[{"action":"click","ref":"a1"},{"action":"press","keys":"Enter"}],"n":1,"x":null}`),
	} {
		if other.equal(base) {
			t.Errorf("%s doesn't change the fingerprint", what)
		}
	}
	// A body that isn't JSON counts as its bytes; an empty one as nothing.
	if !fp("act", "not json").equal(fp("act", "not json")) || fp("act", "not json").equal(fp("act", "not  json")) {
		t.Error("a body that isn't JSON")
	}
	if !fp("act", "").equal(fp("act", " \n")) || fp("act", "").equal(fp("act", "{}")) {
		t.Error("an empty body")
	}
	// Never printed: the body can hold typed text.
	for _, s := range []string{fmt.Sprint(base), fmt.Sprintf("%v %+v %#v %s %x %q", base, base, base, base, base, base)} {
		if strings.ContainsAny(strings.ReplaceAll(s, "[fingerprint]", ""), "0123456789abcdef") {
			t.Errorf("a fingerprint prints as %q", s)
		}
	}
}

// take takes a receipt and fails the test unless it is new.
func take(t *testing.T, m *Manager, sessionID, tokenID, id string, fp Fingerprint) *Call {
	t.Helper()
	c, prev, err := m.TakeCall(sessionID, tokenID, "claude", "act", id, fp)
	if err != nil || prev != nil || c == nil {
		t.Fatalf("TakeCall(%s) = %v, %+v, %v", id, c, prev, err)
	}
	return c
}

// admit runs the call's lease as a page call does, ending it ok.
func admit(t *testing.T, m *Manager, sessionID string, c *Call, ok bool) {
	t.Helper()
	ls, err := m.PageLease(WithCall(context.Background(), c), sessionID, c.TokenID())
	if err != nil {
		t.Fatal(err)
	}
	ls.Release()
	c.End(ok)
}

func TestReceipts(t *testing.T) {
	f := newFixture(t)
	f.clock()
	s := f.start(f.m)
	fp, other := FingerprintOf("act", []byte(`{"a":1}`)), FingerprintOf("act", []byte(`{"a":2}`))

	c := take(t, f.m, s.ID, "tokA", "buy", fp)
	if r, ok := f.m.CallReceipt(s.ID, "tokA", "buy"); !ok || r.State != CallWaiting || r.Tool != "act" || r.Agent != "claude" {
		t.Fatalf("taken: %+v %t", r, ok)
	}
	// Two tokens using the same id never meet.
	cb := take(t, f.m, s.ID, "tokB", "buy", other)
	if _, ok := f.m.CallReceipt(s.ID, "tokA", "nope"); ok {
		t.Fatal("a receipt for an id never taken")
	}
	ls, err := f.m.PageLease(WithCall(context.Background(), c), s.ID, "tokA")
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.m.CallReceipt(s.ID, "tokA", "buy"); r.State != CallRunning {
		t.Fatalf("admitted: %+v", r)
	}
	if _, prev, err := f.m.TakeCall(s.ID, "tokA", "claude", "act", "buy", fp); err != nil || prev == nil || prev.State != CallRunning {
		t.Fatalf("a repeat while running = %+v, %v", prev, err)
	}
	ls.Release()
	c.End(true)
	c.End(false) // once only
	r, _ := f.m.CallReceipt(s.ID, "tokA", "buy")
	if r.State != CallOK || r.Ended.IsZero() || r.CallID != "buy" {
		t.Fatalf("ended: %+v", r)
	}
	if _, prev, err := f.m.TakeCall(s.ID, "tokA", "claude", "act", "buy", fp); err != nil || prev == nil || prev.State != CallOK {
		t.Fatalf("a repeat = %+v, %v", prev, err)
	}
	if _, prev, err := f.m.TakeCall(s.ID, "tokA", "claude", "act", "buy", other); !errors.Is(err, ErrOtherCall) || prev != nil {
		t.Fatalf("the id with other arguments = %+v, %v", prev, err)
	}
	if _, _, err := f.m.TakeCall(s.ID, "tokA", "claude", "navigate", "buy", fp); !errors.Is(err, ErrOtherCall) {
		t.Fatalf("the id with another tool = %v", err)
	}
	// B's buy is its own, and still waiting.
	if r, _ := f.m.CallReceipt(s.ID, "tokB", "buy"); r.State != CallWaiting {
		t.Fatalf("B's buy: %+v", r)
	}
	admit(t, f.m, s.ID, cb, false)
	if r, _ := f.m.CallReceipt(s.ID, "tokB", "buy"); r.State != CallError {
		t.Fatalf("B's buy after an error: %+v", r)
	}
	if r, _ := f.m.CallReceipt(s.ID, "tokA", "buy"); r.State != CallOK {
		t.Fatalf("B's call changed A's: %+v", r)
	}
}

// A call refused before its lease gives its receipt back.
func TestAReceiptRefusedBeforeItsLeaseIsGivenBack(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	fp := FingerprintOf("act", []byte(`{}`))

	// Refused: a person drives.
	advance(time.Minute)
	if err := f.m.TakeControl(s.ID, "person"); err != nil {
		t.Fatal(err)
	}
	c := take(t, f.m, s.ID, "tok", "x", fp)
	if _, err := f.m.PageLease(WithCall(context.Background(), c), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatal(err)
	}
	if _, _, err := f.m.AgentInput(WithCall(context.Background(), c), s.ID, "tok"); !errors.Is(err, ErrHumanDriving) {
		t.Fatal(err)
	}
	c.End(false)
	if r, ok := f.m.CallReceipt(s.ID, "tok", "x"); ok {
		t.Fatalf("a refused call kept its receipt: %+v", r)
	}
	f.m.ReleaseControl(s.ID)

	// Refused: another input is under way.
	_, done, err := f.m.AgentInput(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c = take(t, f.m, s.ID, "tok", "x", fp)
	if _, _, err := f.m.AgentInput(WithCall(context.Background(), c), s.ID, "tok"); !errors.Is(err, ErrInputBusy) {
		t.Fatal(err)
	}
	c.End(false)
	done()
	if _, ok := f.m.CallReceipt(s.ID, "tok", "x"); ok {
		t.Fatal("a busy refusal kept the receipt")
	}

	// The caller went away while waiting for its turn.
	first, err := f.m.PageLease(context.Background(), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c = take(t, f.m, s.ID, "tok", "x", fp)
	ctx, cancel := context.WithCancel(WithCall(context.Background(), c))
	waited := make(chan error, 1)
	go func() {
		_, err := f.m.PageLease(ctx, s.ID, "tok")
		waited <- err
	}()
	if got := f.m.RunningCalls(s.ID, "tok"); len(got) != 1 || got[0].CallID != "x" || got[0].State != CallWaiting {
		t.Fatalf("waiting for its turn: %+v", got)
	}
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c.End(false)
	first.Release()
	if _, ok := f.m.CallReceipt(s.ID, "tok", "x"); ok {
		t.Fatal("a call that never got its turn kept its receipt")
	}

	// The id can be sent again, and runs.
	c = take(t, f.m, s.ID, "tok", "x", fp)
	admit(t, f.m, s.ID, c, true)
	if r, _ := f.m.CallReceipt(s.ID, "tok", "x"); r.State != CallOK {
		t.Fatalf("after the refusals: %+v", r)
	}
}

// Two copies of a call sent together make one call.
func TestTwoCopiesTakeOneReceipt(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	fp := FingerprintOf("act", []byte(`{"a":1}`))
	for round := range 20 {
		id := fmt.Sprint("c", round)
		var wg sync.WaitGroup
		var mu sync.Mutex
		taken, repeats := 0, 0
		start := make(chan struct{})
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c, prev, err := f.m.TakeCall(s.ID, "tok", "claude", "act", id, fp)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					t.Error(err)
				case c != nil:
					taken++
				case prev != nil:
					repeats++
				}
			}()
		}
		close(start)
		wg.Wait()
		if taken != 1 || repeats != 7 {
			t.Fatalf("round %d: %d took the id, %d were repeats", round, taken, repeats)
		}
	}
}

// A lease cut short ends its call interrupted, whatever cut it.
func TestACutLeaseEndsItsCallInterrupted(t *testing.T) {
	cases := map[string]func(f *fixture, s Session, cancel context.CancelFunc, advance func(time.Duration)){
		"a handoff": func(f *fixture, s Session, _ context.CancelFunc, _ func(time.Duration)) {
			if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
				f.t.Fatal(err)
			}
		},
		"take control": func(f *fixture, s Session, _ context.CancelFunc, advance func(time.Duration)) {
			advance(takeOverAfter + time.Second)
			if err := f.m.TakeControl(s.ID, "person"); err != nil {
				f.t.Fatal(err)
			}
		},
		"a stop": func(f *fixture, s Session, _ context.CancelFunc, _ func(time.Duration)) {
			if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
				f.t.Fatal(err)
			}
		},
		"a revocation": func(f *fixture, _ Session, _ context.CancelFunc, _ func(time.Duration)) {
			f.m.DisconnectToken("tok")
		},
		"a hold": func(f *fixture, s Session, _ context.CancelFunc, _ func(time.Duration)) {
			release, err := f.m.HoldSession(s.ID)
			if err != nil {
				f.t.Fatal(err)
			}
			release()
		},
		"the caller going away": func(_ *fixture, _ Session, cancel context.CancelFunc, _ func(time.Duration)) {
			cancel()
		},
	}
	for name, cut := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			advance := f.clock()
			s := f.start(f.m)
			c := take(t, f.m, s.ID, "tok", "x", FingerprintOf("act", nil))
			ctx, cancel := context.WithCancel(WithCall(context.Background(), c))
			defer cancel()
			ls, err := f.m.PageLease(ctx, s.ID, "tok")
			if err != nil {
				t.Fatal(err)
			}
			ls.Breakable()
			released := make(chan struct{})
			go func() {
				<-ls.Ctx.Done()
				ls.Release()
				close(released)
			}()
			cut(f, s, cancel, advance)
			<-released
			c.End(false)
			f.m.mu.Lock()
			state := c.rec.state
			f.m.mu.Unlock()
			if state != CallInterrupted {
				t.Fatalf("state = %s", state)
			}
		})
	}
}

// The same for a Visual-mode input.
func TestACutInputEndsItsCallInterrupted(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	c := take(t, f.m, s.ID, "tok", "x", FingerprintOf("type", nil))
	ctx, done, err := f.m.AgentInput(WithCall(context.Background(), c), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.m.RunningCalls(s.ID, "tok"); len(got) != 1 || got[0].Tool != "act" || got[0].CallID != "x" || got[0].State != CallRunning {
		t.Fatalf("running: %+v", got)
	}
	if got := f.m.RunningCalls(s.ID, "other"); len(got) != 0 {
		t.Fatalf("another token sees %+v", got)
	}
	go func() {
		<-ctx.Done()
		done()
	}()
	if _, err := f.m.SetNeedsHelp(s.ID, true); err != nil {
		t.Fatal(err)
	}
	c.End(false)
	if r, _ := f.m.CallReceipt(s.ID, "tok", "x"); r.State != CallInterrupted {
		t.Fatalf("state = %+v", r)
	}
	// One that ends of itself, with an error, keeps it as error.
	if _, err := f.m.SetNeedsHelp(s.ID, false); err != nil {
		t.Fatal(err)
	}
	c = take(t, f.m, s.ID, "tok", "y", FingerprintOf("type", nil))
	_, done, err = f.m.AgentInput(WithCall(context.Background(), c), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	done()
	c.End(false)
	if r, _ := f.m.CallReceipt(s.ID, "tok", "y"); r.State != CallError {
		t.Fatalf("state = %+v", r)
	}
	// A panic in its handler ends it interrupted too.
	c = take(t, f.m, s.ID, "tok", "z", FingerprintOf("type", nil))
	_, done, err = f.m.AgentInput(WithCall(context.Background(), c), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	done()
	c.EndPanicked()
	if r, _ := f.m.CallReceipt(s.ID, "tok", "z"); r.State != CallInterrupted {
		t.Fatalf("state after a panic = %+v", r)
	}
}

func TestReceiptsAreCapped(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	fp := FingerprintOf("act", nil)
	for i := range maxReceipts {
		c := take(t, f.m, s.ID, "tok", fmt.Sprint("c", i), fp)
		admit(t, f.m, s.ID, c, true)
		advance(time.Millisecond)
	}
	// The 1,001st drops the oldest ended one.
	take(t, f.m, s.ID, "tok", "new", fp)
	if _, ok := f.m.CallReceipt(s.ID, "tok", "c0"); ok {
		t.Fatal("the oldest receipt is still there")
	}
	if _, ok := f.m.CallReceipt(s.ID, "tok", "c1"); !ok {
		t.Fatal("the second oldest went too")
	}

	// Full of calls under way: a new id is refused, and none is dropped.
	f2 := newFixture(t)
	s2 := f2.start(f2.m)
	for i := range maxReceipts {
		c := take(t, f2.m, s2.ID, "tok", fmt.Sprint("c", i), fp)
		if i%2 == 0 { // running, or still waiting
			if _, _, err := f2.m.AgentInput(WithCall(context.Background(), c), s2.ID, "tok"); err != nil && !errors.Is(err, ErrInputBusy) {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := f2.m.TakeCall(s2.ID, "tok", "claude", "act", "one-more", fp); !errors.Is(err, ErrTooManyCalls) {
		t.Fatalf("a new id with 1000 under way = %v", err)
	}
	f2.m.mu.Lock()
	n := len(f2.m.findLocked(s2.ID).calls)
	f2.m.mu.Unlock()
	if n != maxReceipts {
		t.Fatalf("%d receipts left", n)
	}
}

func TestReceiptsGoWithTheSession(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	fp := FingerprintOf("act", nil)
	c := take(t, f.m, s.ID, "tok", "x", fp)
	admit(t, f.m, s.ID, c, true)
	ended := make(chan string, 1)
	f.m.OnEnd(func(id string) { ended <- id })
	if _, err := f.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if id := <-ended; id != s.ID {
		t.Fatalf("OnEnd for %s", id)
	}
	if _, ok := f.m.CallReceipt(s.ID, "tok", "x"); ok {
		t.Fatal("a stopped session still answers for its calls")
	}
	if _, _, err := f.m.TakeCall(s.ID, "tok", "claude", "act", "x", fp); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a call id on a stopped session = %v", err)
	}
	if got := f.m.RunningCalls(s.ID, "tok"); got != nil {
		t.Fatalf("running calls on a stopped session: %+v", got)
	}
	// The next session of the profile starts with none.
	s2 := f.start(f.m)
	take(t, f.m, s2.ID, "tok", "x", fp)
}

func TestRecentCalls(t *testing.T) {
	f := newFixture(t)
	advance := f.clock()
	s := f.start(f.m)
	mine := take(t, f.m, s.ID, "tok", "buy", FingerprintOf("act", nil))
	admit(t, f.m, s.ID, mine, true)
	f.m.NoteFor(WithCall(context.Background(), mine), s.ID, "claude", "Clicked a7e12")
	advance(time.Second)
	f.m.NoteDownloadFor(WithCall(context.Background(), mine), s.ID, "claude", "Downloaded x.csv (1 kB)")
	advance(time.Second)
	f.m.NoteFor(WithCall(context.Background(), f.m.NewCall("other", "helper", "click")), s.ID, "helper", "Clicked at 1, 2")
	f.m.NoteFor(WithCall(context.Background(), f.m.NewCall("tok", "claude", "snapshot")), s.ID, "claude", "Read the page")
	f.m.Note(s.ID, "", "Handed back")
	got := f.m.RecentCalls(s.ID, "tok", 20)
	if len(got) != 3 {
		t.Fatalf("recent: %+v", got)
	}
	if got[0].What != "Read the page" || got[0].CallID != "" || got[0].Tool != "snapshot" || got[0].State != "" {
		t.Errorf("newest: %+v", got[0])
	}
	if got[1].What != "Downloaded x.csv (1 kB)" || !got[1].FromPage || got[1].CallID != "buy" || got[1].State != CallOK {
		t.Errorf("download: %+v", got[1])
	}
	if got[2].What != "Clicked a7e12" || got[2].FromPage || got[2].Tool != "act" || got[2].State != CallOK {
		t.Errorf("oldest: %+v", got[2])
	}
	if got := f.m.RecentCalls(s.ID, "tok", 1); len(got) != 1 {
		t.Errorf("capped at 1: %+v", got)
	}
	if got := f.m.RecentCalls(s.ID, "", 20); len(got) != 0 {
		t.Errorf("entries without a token: %+v", got)
	}
	// The admin's log is as it was.
	if a := f.m.Activity(s.ID); len(a) != 5 || a[0].What != "Handed back" || a[2].Agent != "helper" {
		t.Errorf("activity: %+v", a)
	}
}

// A caller gone by the time its turn comes is never admitted, though the
// turn may win the race with its cancellation: its id is given back and
// the turn isn't kept.
func TestACancelledCallerIsNeverAdmitted(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	fp := FingerprintOf("act", []byte(`{}`))
	// With the turn free and the context already done, both of
	// PageLease's select cases are ready, and either can be picked: try
	// it often enough that the turn wins some.
	for i := range 64 {
		c := take(t, f.m, s.ID, "tok", "gone", fp)
		ctx, cancel := context.WithCancel(WithCall(context.Background(), c))
		cancel()
		if ls, err := f.m.PageLease(ctx, s.ID, "tok"); !errors.Is(err, context.Canceled) {
			if ls != nil {
				ls.Release()
			}
			t.Fatalf("round %d: PageLease = %v", i, err)
		}
		if _, done, err := f.m.AgentInput(ctx, s.ID, "tok"); !errors.Is(err, context.Canceled) {
			if done != nil {
				done()
			}
			t.Fatalf("round %d: AgentInput = %v", i, err)
		}
		c.End(false)
		if r, ok := f.m.CallReceipt(s.ID, "tok", "gone"); ok {
			t.Fatalf("round %d: a cancelled call kept its receipt: %+v", i, r)
		}
	}
	// The turn was given back each time.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ls, err := f.m.PageLease(ctx, s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ls.Release()
}

// A call with an id still running after its lease (a click looking for
// its download) shows in RunningCalls until it ends.
func TestACallRunningAfterItsLeaseIsListed(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.m)
	c := take(t, f.m, s.ID, "tok", "dl", FingerprintOf("click", nil))
	_, done, err := f.m.AgentInput(WithCall(context.Background(), c), s.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.m.RunningCalls(s.ID, "tok"); len(got) != 1 || got[0].CallID != "dl" || got[0].State != CallRunning {
		t.Fatalf("holding its lease: %+v", got)
	}
	done()
	if got := f.m.RunningCalls(s.ID, "tok"); len(got) != 1 || got[0].CallID != "dl" || got[0].State != CallRunning || got[0].Tool != "act" {
		t.Fatalf("after its lease: %+v", got)
	}
	c.End(true)
	if got := f.m.RunningCalls(s.ID, "tok"); len(got) != 0 {
		t.Fatalf("ended: %+v", got)
	}
}
