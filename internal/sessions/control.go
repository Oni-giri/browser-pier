package sessions

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// agentQuiet is how long after an agent's last click or keystroke a
// person can take control: within it, the agent counts as driving.
const agentQuiet = 5 * time.Second

// takeOverAfter is how long an agent's input action must have run before
// the person in the live view can take control by breaking it off: a
// click is over well within it, a long human-paced type isn't.
const takeOverAfter = 3 * time.Second

// AgentInput records an agent's click or keystroke. It fails while a
// person has control, and with ErrInputBusy while another is under way:
// one at a time per session, so an agent can't queue up minutes of input.
//
// It is a lease: call done once the browser has answered. A handoff
// (Needs help) or a stop cancels the context it returns, so an action
// that takes a while (typing at human pace, PROJ-113) breaks off at once,
// and waits for leases to end before the person gets the browser; it
// refuses new ones. The browser lets go of what the action held as it
// answers, so the person gets it at most tens of milliseconds after the
// break. A person can take control from the live view once one has run
// for takeOverAfter, which breaks it off the same way; not before, nor
// for agentQuiet after it ends.
func (m *Manager) AgentInput(ctx context.Context, id string) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return nil, nil, ErrNotRunning
	}
	if l.human || l.sess.State == NeedsHelp {
		return nil, nil, ErrHumanDriving
	}
	if len(l.inputs) > 0 {
		return nil, nil, ErrInputBusy
	}
	now := m.now()
	l.lastActivity, l.lastAgentInput = now, now
	cctx, cancel := context.WithCancelCause(ctx)
	in := &lease{cancel: cancel, started: now}
	if l.inputs == nil {
		l.inputs = map[*lease]struct{}{}
	}
	l.inputs[in] = struct{}{}
	m.agentActedLocked(l)
	return cctx, sync.OnceFunc(func() {
		cancel(context.Canceled)
		m.mu.Lock()
		delete(l.inputs, in)
		// The agent counts as driving for agentQuiet after its last
		// event, not after the action began.
		now := m.now()
		l.lastActivity, l.lastAgentInput = now, now
		m.agentActedLocked(l)
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	}), nil
}

// TakeControl gives a person in the live view the mouse and keyboard.
// It is refused while an agent is driving: connected over CDP, having
// clicked or typed in the last few seconds, with a page call (PageLease)
// under way or just ended, or with an input action, or a page call marked
// Breakable (act), under way for less than takeOverAfter. One that has
// run longer (a long human-paced type, or act's steps) is broken off, as
// a handoff breaks it off, and TakeControl waits for it to end. A page
// call not marked Breakable (a read, bounded by its own timeouts) is
// never broken off: Take control is refused while it runs. Meanwhile agents can only watch: their input and new CDP
// connections are refused. During a handoff (Needs help) the agent is
// paused, so it always succeeds. If the browser begins to stop while it
// waits, it gives up with ErrNotRunning, leaving the browser to agents
// should the stop fail.
func (m *Manager) TakeControl(id string) error {
	_, err := m.TakeControlGen(id)
	return err
}

// TakeControlGen is TakeControl, returning the generation this takeover
// has: the person's input (HumanInput) and their own hand-back
// (ReleaseControlGen) name it, so once their control has ended some other
// way and someone else has taken the browser, nothing they still send
// reaches it (PROJ-196).
func (m *Manager) TakeControlGen(id string) (gen uint64, err error) {
	return m.takeControl(id, false, 0)
}

// ErrHandoffEnded: the handoff a Take control was for has ended.
var ErrHandoffEnded = errors.New("the handoff has ended")

// HelpEpoch says which time the session went to Needs help, while it
// waits for help: a handoff's live view reads it while its handoff is
// open, and takes control with it (TakeControlInHandoff).
func (m *Manager) HelpEpoch(id string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || l.sess.State != NeedsHelp {
		return 0, false
	}
	return l.sess.helpEpoch, true
}

// TakeControlInHandoff is TakeControlGen for a handoff's live view: only
// while the session still waits for the help it asked for as epoch
// (HelpEpoch), checked in the same step as the take and again once
// whatever it waited for has ended; otherwise ErrHandoffEnded. A
// handoff's link that outlives its handoff, even by a moment, never
// takes the browser back from the agent it was handed back to.
func (m *Manager) TakeControlInHandoff(id string, epoch uint64) (gen uint64, err error) {
	return m.takeControl(id, true, epoch)
}

// takeControl is TakeControlGen, and for a handoff, TakeControlInHandoff:
// no epoch, 0 included, lets a handoff's take through once its Needs
// help is over.
func (m *Manager) takeControl(id string, handoff bool, epoch uint64) (gen uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return 0, ErrNotRunning
	}
	handoffOver := func() bool {
		return handoff && (epoch == 0 || l.sess.State != NeedsHelp || l.sess.helpEpoch != epoch)
	}
	if handoffOver() {
		return 0, ErrHandoffEnded
	}
	now := m.now()
	breakOff := false
	if l.sess.State == Running {
		if len(l.conns) > 0 {
			return 0, ErrAgentDriving
		}
		for ls := range l.leases {
			if !ls.breakable || now.Sub(ls.started) < takeOverAfter {
				return 0, ErrAgentDriving
			}
			breakOff = true
		}
		for in := range l.inputs {
			if now.Sub(in.started) < takeOverAfter {
				return 0, ErrAgentDriving
			}
			breakOff = true
		}
		if !breakOff && now.Sub(l.lastAgentInput) < agentQuiet {
			return 0, ErrAgentDriving
		}
	}
	if l.human {
		return 0, ErrHumanDriving // one person at a time
	}
	// The takeover's generation is saved before it is handed out, so a
	// restart always knows the highest one the browser may have seen:
	// it fences that, and numbers the next ones above it (PROJ-196).
	if err := saveTakeoverGen(context.Background(), m.db, l.sess.ID, l.generation+1); err != nil {
		return 0, fmt.Errorf("saving the takeover: %w", err)
	}
	l.generation++
	l.human = true
	// Recording stops here, under m.mu and before anything else: no
	// fragment the browser sends from now on is kept, and HumanInput,
	// which the live view needs before it forwards any input, can't see
	// human until it has.
	m.personTookLocked(l)
	l.humanGen = l.generation
	gen = l.generation
	l.takeCtx, l.takeCancel = context.WithCancel(l.opCtx)
	l.lastActivity = now
	if breakOff {
		// New agent input and page calls are refused from now on
		// (l.human); what is under way breaks off. Every lease left here
		// is Breakable and old enough, or TakeControl was refused above.
		for in := range l.inputs {
			in.cancel(ErrTookControl)
		}
		l.cancelLeasesLocked(ErrTookControl)
	}
	// What is still under way (that one, or during a handoff what the
	// handoff cancelled and is itself waiting for) ends before the person
	// gets the browser.
	if len(l.inputs) > 0 || len(l.leases) > 0 {
		stops := l.stops
		m.waitLeasesLocked(l)
		if m.findLocked(id) != l || (l.sess.State != Running && l.sess.State != NeedsHelp) || l.stops != stops {
			// A stop began meanwhile, even one that failed and put the
			// browser back before this woke: the take gives up
			// (PROJ-200), as it would have had it woken first.
			m.untakeLocked(l, gen)
			return 0, ErrNotRunning
		}
		if handoffOver() {
			m.untakeLocked(l, gen)
			return 0, ErrHandoffEnded
		}
		if !l.human || l.humanGen != gen || l.releasing {
			// Handed back meanwhile (the profile page's Hand back, a
			// handoff ending): that ended this takeover before the
			// person ever had it, and no generation of it is handed out.
			return 0, ErrNotInControl
		}
	}
	return gen, nil
}

// untakeLocked undoes a TakeControl that gave up, so that if the browser
// isn't stopped after all (the stop fails and runStop puts it back), it
// is the agents' again, with no control left set that no one holds
// (PROJ-200). It runs under m.mu, in the same step that decides to give
// up, and only if the control set is still that take's own (human, its
// generation, not ending): one already ended by a ReleaseControl
// meanwhile is that hand-back's to finish.
//
// The clipboard needn't be emptied: nothing the person did can be on it.
// The live view forwards input only once Take control has returned its
// generation, and HumanInput accepts only that generation; this take
// returns none.
//
// The generation stays as it is, never goes back: it was bumped as the
// agent's work under way was broken off, and a page call learning it
// changed only re-reads the page. Recording is asked again, as at a
// hand-back: it resumes at the agent's next action (the tail from before
// was dropped by personTookLocked, as for any takeover). runStop asks too
// as it puts the browser back.
func (m *Manager) untakeLocked(l *live, gen uint64) {
	if !l.human || l.humanGen != gen || l.releasing {
		return
	}
	l.human = false
	l.takeCancel()
	if m.live[l.sess.Profile] == l {
		m.regateLocked(l)
	}
}

// ErrNotInControl: the person's control ended (they handed back, or it
// was ended for them: a handoff ended, the profile page's Hand back), and
// whoever drives now, if anyone, took it afresh.
var ErrNotInControl = errors.New("you don't have control of this browser")

// HumanInput records that the person who took control as generation gen
// (TakeControlGen) used the browser. It fails with ErrNotInControl unless
// that takeover still holds control and isn't ending, and with
// ErrNotRunning while the browser isn't running or waiting for help (it
// is stopping, say). Call done once the input has reached the browser:
// ReleaseControl waits for input under way, so none lands (text put on
// the clipboard, say) after it has emptied the clipboard.
//
// Do all of it with ctx, from admission on: it ends the moment the
// takeover's hand-back begins (or the session ends), so whatever the
// operation hasn't sent by then never reaches the browser. The hand-back
// lets agents in, and the session lets go of its profile, only once done
// is called.
func (m *Manager) HumanInput(id string, gen uint64) (ctx context.Context, done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if err := humanRefusal(l, gen); err != nil {
		return nil, nil, err
	}
	l.lastActivity = m.now()
	l.humanInputs++
	return l.takeCtx, sync.OnceFunc(func() {
		m.mu.Lock()
		l.humanInputs--
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	}), nil
}

// humanRefusal says why the person who took control as generation gen
// can't use the browser now, if they can't. m.mu must be held.
func humanRefusal(l *live, gen uint64) error {
	switch {
	case l == nil:
		return ErrNotRunning
	case !l.human || l.humanGen != gen || l.releasing:
		return ErrNotInControl
	case l.sess.State != Running && l.sess.State != NeedsHelp, l.opCtx.Err() != nil:
		return ErrNotRunning
	}
	return nil
}

// InControl reports whether the takeover of generation gen holds the
// browser now, as HumanInput would admit its input: not while it is
// ending, nor while the browser is stopping or its session over.
func (m *Manager) InControl(id string, gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return humanRefusal(m.findLocked(id), gen) == nil
}

// ReleaseControl hands the browser back to agents, whatever ends the
// person's control: they hand back, a handoff ends, the live view goes.
// The browser fences the takeover first (fenceTakeover), refusing from
// then on whatever of it is still on its way, and lets go of any key or
// mouse button the person still holds (a drag when a handoff expires);
// then, a moment later (clipboardSettle), its clipboard and PRIMARY
// selection are emptied (PROJ-194), so no agent can paste what the
// person put there, copied or selected in the page; only then are agents
// let in. If the clipboard can't be emptied, that is logged and the
// browser handed back all the same. A browser that is stopping keeps its
// clipboard: the container takes it along, or, if the stop fails,
// runStop empties it before agents get the browser back. Recording
// (PROJ-23) stays off throughout, and the gate is asked again only as
// agents get the browser back.
//
// The hand-back runs on its own (beginReleaseLocked), tied to the
// session: ReleaseControl waits for it, until agents have the browser or
// the session has ended (a stop ends a hand-back the browser never
// answers). Callers that mustn't wait on a browser use HandBack, with
// their context, or EndHandoff.
//
// It ends whoever's control it is: it is for what ends a takeover from
// outside it (a handoff, the profile page's Hand back). The person's own
// live view hands back with ReleaseControlGen.
func (m *Manager) ReleaseControl(id string) {
	_ = m.HandBack(context.Background(), id)
}

// HandBack is ReleaseControl, waiting no longer than ctx: the hand-back
// goes on without the caller if ctx ends first, and ctx's error is
// returned.
func (m *Manager) HandBack(ctx context.Context, id string) error {
	m.mu.Lock()
	l := m.findLocked(id)
	if l == nil || !l.human {
		m.mu.Unlock()
		return nil
	}
	done := m.beginReleaseLocked(l, id)
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseControlGen starts handing back the takeover of generation gen
// (TakeControlGen) alone: if that control has ended already, however, it
// does nothing and returns nil, so a live view that missed the end of its
// control never ends, or lifts the keys or empties the clipboard of, the
// takeover of whoever took the browser since (PROJ-196). The check and
// the start of the hand-back are one step under m.mu, and no one else can
// take control until it is over. It doesn't wait: done is closed once
// agents have the browser. started is false if the hand-back had begun
// already (its control was ended for it meanwhile).
func (m *Manager) ReleaseControlGen(id string, gen uint64) (done <-chan struct{}, started bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human || l.humanGen != gen {
		return nil, false
	}
	started = !l.releasing
	return m.beginReleaseLocked(l, id), started
}

// beginReleaseLocked starts ending l's person's control, unless that has
// started already, and returns a channel closed once it has ended. From
// here on human stays set, so no one else can take control, and agents
// stay out, until runRelease is done. m.mu must be held.
func (m *Manager) beginReleaseLocked(l *live, id string) <-chan struct{} {
	if l.releasing {
		return l.released
	}
	l.releasing = true
	l.releaseStarted = m.now()
	l.released = make(chan struct{})
	// What the person's live view still has under way breaks off now, on
	// this side: it returns at once, and can't reach the browser any
	// more.
	l.takeCancel()
	done, gen := l.released, l.humanGen
	if !m.spawn(func() { m.runRelease(l, id, gen) }) {
		// Closing: the next BrowserPier fences this takeover as it picks
		// the browser up (Reconcile).
		m.endReleaseLocked(l, id)
	}
	return done
}

// runRelease is the hand-back beginReleaseLocked started, for takeover
// gen.
func (m *Manager) runRelease(l *live, id string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	profile := l.sess.Profile
	// The browser fences the takeover, so whatever of it is still on its
	// way there, queued there (in xdotool) or held up in there is
	// refused or has run, and lets go of whatever key or button the
	// person still holds, so whoever drives next doesn't inherit a
	// pressed one. Tried until the browser answers or the session ends:
	// agents wait meanwhile, and a session whose browser doesn't answer
	// can still be stopped, idle or not (idleLocked), which ends this.
	// While the browser runs, waits for help or is stopping (a stop may
	// fail); not once the session is over.
	fenced := false
	if l.sess.State == Running || l.sess.State == NeedsHelp || l.sess.State == Stopping {
		m.unlockedOpLocked(l, func(ctx context.Context) {
			_, fenced = m.fenceTakeover(ctx, id, profile, gen)
		})
	}
	// The person's operations under way return, cancelled or refused.
	// One that doesn't (a bug) can do nothing to the browser once it has
	// fenced the takeover, so the hand-back goes on after inputWait; it
	// only waits for it if the browser didn't fence it, which happens
	// only as the session ends.
	busy := func() bool { return l.humanInputs > 0 && m.findLocked(id) == l && m.ctx.Err() == nil }
	if !m.waitBoundedLocked(inputWait, busy) {
		if fenced {
			m.log.Warn("a person's input was still under way "+inputWait.String()+" after their control began to end; the browser refuses it, so the hand-back goes on", "session", id)
		} else {
			for busy() {
				m.inputsDone.Wait()
			}
		}
	}
	switch l.sess.State {
	case Running, NeedsHelp:
		if fenced {
			m.unlockedOpLocked(l, func(ctx context.Context) {
				// The person's last keys have reached the browser, but
				// Chromium takes the selection a moment after a Ctrl+C:
				// let it land, so it is emptied too.
				if settle(ctx) {
					m.emptyClipboard(ctx, id, profile, gen)
				}
			})
		}
	case Stopping:
		// The container takes the clipboard along; if the stop fails
		// instead, runStop empties it before agents are let back in.
		l.clipboardLeft, l.clipboardLeftGen = true, gen
	}
	m.endReleaseLocked(l, id)
}

// endReleaseLocked lets agents back in as a hand-back ends.
func (m *Manager) endReleaseLocked(l *live, id string) {
	l.human, l.releasing = false, false
	l.lastActivity = m.now()
	// Only now, the clipboard emptied and agents let back in, may the
	// gate record again (PROJ-23): never while the person's control is
	// still ending. A session stopped meanwhile has nothing to record.
	if m.findLocked(id) == l {
		m.regateLocked(l)
	}
	close(l.released)
	m.inputsDone.Broadcast()
}

// clipboardSettle is how long a hand-back waits, after the person's last
// input has reached the browser, before it empties the clipboard.
const clipboardSettle = 100 * time.Millisecond

// settle waits clipboardSettle, and reports false if ctx ended first.
func settle(ctx context.Context) bool {
	t := time.NewTimer(clipboardSettle)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// inputWait is how long a hand-back waits for the person's input under
// way, already cancelled and fenced: a cancelled call returns at once,
// so only one that never says done (a bug) makes it wait this long.
// Tests shorten it.
var inputWait = 30 * time.Second

// emptyClipboard empties a running browser's clipboard as takeover gen
// ends (PROJ-194): the browser does nothing if a later takeover has begun
// meanwhile, whose clipboard it would be (a call held up past its time,
// PROJ-196). A failure is logged without what the clipboard held, which
// BrowserPier never sees here; not when the session ended meanwhile
// (ctx), its container taking the clipboard along.
func (m *Manager) emptyClipboard(ctx context.Context, sessionID, profile string, gen uint64) {
	hdr := map[string]string{FenceHeader: strconv.FormatUint(gen, 10)}
	if err := m.controlWith(ctx, profile, http.MethodDelete, "/input/clipboard", hdr, nil, nil); err != nil && ctx.Err() == nil {
		m.log.Warn("couldn't empty the browser's clipboard as a person's control ended", "session", sessionID, "failure", controlFailure(err))
	}
}

// TakeoverHeader names, on each operation a person's live view sends the
// browser, the takeover it is for (its generation); FenceHeader, on the
// lift as a takeover ends, the takeover to fence: the browser refuses,
// from then on, anything from it or an earlier one, however long it was
// held up on its way, and has run what it queued of it. On the
// clipboard's emptying it names the takeover it is for, which the
// browser skips if a later one has begun (images/chromium/entrypoint/
// fence.go, PROJ-196).
const (
	TakeoverHeader = "X-BrowserPier-Takeover"
	FenceHeader    = "X-BrowserPier-Fence"
)

// fenceTakeover has the browser fence takeover want and let go of every
// key and button it, or an earlier one, still holds (a key left down
// would be the next driver's: a modifier, a drag). Only a 200 answer
// giving a fence at or above want counts: a browser that answers anything
// else (204, an empty or lower fence: an entrypoint from before the
// fence) hasn't fenced it. It tries until the browser does or ctx ends,
// waiting a little longer each time, and returns the fence answered. A
// failure is logged by its kind alone (controlFailure).
func (m *Manager) fenceTakeover(ctx context.Context, sessionID, profile string, want uint64) (fence uint64, ok bool) {
	wait := liftRetry
	hdr := map[string]string{FenceHeader: strconv.FormatUint(want, 10)}
	for try := 1; ; try++ {
		var out struct {
			Fence *uint64 `json:"fence"`
		}
		err := m.controlWith(ctx, profile, http.MethodPost, "/input/release", hdr, nil, &out)
		if err == nil {
			if out.Fence != nil && *out.Fence >= want {
				return *out.Fence, true
			}
			err = errNotFenced
		}
		if ctx.Err() != nil {
			return 0, false
		}
		if try == 1 || try%20 == 0 {
			m.log.Warn("the browser didn't fence and let go of a person's control as it ended; trying again, agents waiting", "session", sessionID, "tries", try, "failure", controlFailure(err))
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return 0, false
		}
		wait = min(2*wait, maxLiftRetry)
	}
}

// errNotFenced: the browser answered a fenced lift without the fence.
var errNotFenced = errors.New("the browser answered without fencing the takeover")

// liftRetry is how long fenceTakeover first waits before trying again;
// it doubles each time, up to maxLiftRetry.
const (
	liftRetry    = 100 * time.Millisecond
	maxLiftRetry = 5 * time.Second
)

// controlFailure describes a failed call on a browser's control socket
// for the log: the answer's status code, or what kind of failure it was,
// never the words of the answer. The container isn't trusted, and its
// words could carry what a clipboard held.
func controlFailure(err error) string {
	var ce *controlError
	switch {
	case errors.Is(err, errNotFenced):
		return "no fence in its answer"
	case errors.As(err, &ce):
		return "status " + strconv.Itoa(ce.status)
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer in time"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "the browser's control socket couldn't be reached"
	}
	return "the browser's answer couldn't be read"
}

// sockOpLocked admits one of BrowserPier's own calls on l's control
// socket that runs without m.mu (a hand-back's lift or clipboard
// emptying), for l's browser alone: it returns the context to make it
// with, and false once the session is ending. Call sockOpDoneLocked, with
// m.mu, once it is over: end waits for that before the profile, and its
// socket, can have a new browser.
func (m *Manager) sockOpLocked(l *live) (context.Context, bool) {
	if l.opCtx.Err() != nil {
		return nil, false
	}
	l.sockOps++
	return l.opCtx, true
}

func (m *Manager) sockOpDoneLocked(l *live) {
	l.sockOps--
	m.inputsDone.Broadcast()
}

// unlockedOpLocked runs f, a call on l's control socket, as sockOpLocked
// admits it, with m.mu let go meanwhile and held again after, panic or
// not. It reports false if the session is ending and f didn't run.
func (m *Manager) unlockedOpLocked(l *live, f func(ctx context.Context)) bool {
	ctx, ok := m.sockOpLocked(l)
	if !ok {
		return false
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.sockOpDoneLocked(l)
	}()
	f(ctx)
	return true
}

// waitBoundedLocked waits, with m.mu held (let go of meanwhile), while
// busy says so, for at most bound. It reports false if bound passed
// first.
func (m *Manager) waitBoundedLocked(bound time.Duration, busy func() bool) bool {
	if !busy() {
		return true
	}
	expired := false
	t := time.AfterFunc(bound, func() {
		m.mu.Lock()
		expired = true
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	})
	defer t.Stop()
	for busy() && !expired {
		m.inputsDone.Wait()
	}
	return !expired
}

// sockOpWait bounds how long a session's end waits for its calls on the
// control socket. Each has a deadline well within it (controlWait, the
// control helper's 5 s), and the session's end cancels them at once: it
// is only a backstop against a caller that never says done.
const sockOpWait = 30 * time.Second

// waitSockOpsLocked waits, with m.mu held, until no call admitted for
// l's browser on the control socket is under way: the person's input
// (HumanInput) and a hand-back's or runStop's own (sockOpLocked). l's
// session has ended, so no more are admitted.
func (m *Manager) waitSockOpsLocked(l *live) {
	if !m.waitBoundedLocked(sockOpWait, func() bool { return l.sockOps > 0 || l.humanInputs > 0 }) {
		m.log.Error("a session ended with calls to its browser still under way; its profile is let go all the same",
			"session", l.sess.ID, "calls", l.sockOps+l.humanInputs)
	}
}

// Watch records an open live view until release is called; a watched
// browser is never stopped as idle.
func (m *Manager) Watch(id string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.sess.State.Live() {
		return nil, ErrNotRunning
	}
	l.viewers++
	done := false
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !done {
			done = true
			l.viewers--
			l.lastActivity = m.now()
		}
	}, nil
}
