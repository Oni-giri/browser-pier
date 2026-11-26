package sessions

import (
	"context"
	"errors"
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
	return l.helpEpoch, true
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
		return handoff && (epoch == 0 || l.sess.State != NeedsHelp || l.helpEpoch != epoch)
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
	l.human = true
	// Recording stops here, under m.mu and before anything else: no
	// fragment the browser sends from now on is kept, and HumanInput,
	// which the live view needs before it forwards any input, can't see
	// human until it has.
	m.personTookLocked(l)
	l.generation++
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
// First it lets go of any key or mouse button the person still holds
// (a drag when a handoff expires), then empties the browser's clipboard
// and PRIMARY selection (PROJ-194), so no agent can paste what the
// person put there, copied or selected in the page: it waits for their
// input under way and a moment more (clipboardSettle), refuses more, and
// only then lets agents in. If the clipboard can't be emptied, that is
// logged and the browser handed back all the same. A browser that is
// stopping keeps its clipboard: the container takes it along, or, if the
// stop fails, runStop empties it before agents get the browser back.
// Recording (PROJ-23) stays off throughout, and the gate is asked again
// only as agents get the browser back. It returns once agents have the
// browser.
//
// It ends whoever's control it is: it is for what ends a takeover from
// outside it (a handoff, the profile page's Hand back). The person's own
// live view hands back with ReleaseControlGen.
func (m *Manager) ReleaseControl(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human {
		return
	}
	m.releaseLocked(l, id)
}

// ReleaseControlGen is ReleaseControl for the takeover of generation gen
// (TakeControlGen) alone: if that control has ended already, however, it
// does nothing and returns false, so a live view that missed the end of
// its control never ends, or lifts the keys or empties the clipboard of,
// the takeover of whoever took the browser since (PROJ-196). The check
// and the release are one step under m.mu, and no one else can take
// control until the release is over.
func (m *Manager) ReleaseControlGen(id string, gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human || l.humanGen != gen {
		return false
	}
	ended := !l.releasing // otherwise ended for them meanwhile: that hand-back finishes it
	m.releaseLocked(l, id)
	return ended
}

// releaseLocked ends l's person's control (see ReleaseControl); m.mu is
// held, and let go of meanwhile. Throughout, human stays set, so no one
// else can take control before the end.
func (m *Manager) releaseLocked(l *live, id string) {
	if l.releasing {
		// Another hand-back is ending this takeover: done when it has,
		// not when some later one has too (someone may take control the
		// moment it ends).
		gen := l.humanGen
		for m.findLocked(id) == l && l.human && l.humanGen == gen {
			m.inputsDone.Wait()
		}
		return
	}
	l.releasing = true
	// What the person's live view still has under way breaks off now, on
	// this side: it returns at once, and can't reach the browser any
	// more.
	l.takeCancel()
	gen, profile := l.humanGen, l.sess.Profile
	// And on the browser's: it fences this takeover, so whatever of it
	// is still on its way there, or held up in there, is refused from
	// now on; an action already under way there finishes first. Then it
	// lets go of whatever key or button the person still holds, so
	// whoever drives next doesn't inherit a pressed one. Tried until the
	// browser answers or the session ends: agents wait meanwhile. While
	// the browser runs, waits for help or is stopping (a stop may fail);
	// not once the session is over.
	fenced := false
	if l.sess.State == Running || l.sess.State == NeedsHelp || l.sess.State == Stopping {
		m.unlockedOpLocked(l, func(ctx context.Context) {
			_, fenced = m.fenceTakeover(ctx, id, profile, strconv.FormatUint(gen, 10))
		})
	}
	// The person's operations under way return, cancelled or refused.
	// One that doesn't (a bug) can do nothing to the browser once it has
	// fenced the takeover, so the hand-back goes on after inputWait; it
	// only waits for it if the browser didn't fence it, which happens
	// only as the session ends.
	if !m.waitBoundedLocked(inputWait, func() bool { return l.humanInputs > 0 && m.findLocked(id) == l }) {
		if fenced {
			m.log.Warn("a person's input was still under way "+inputWait.String()+" after their control began to end; the browser refuses it, so the hand-back goes on", "session", id)
		} else {
			for l.humanInputs > 0 && m.findLocked(id) == l {
				m.inputsDone.Wait()
			}
		}
	}
	switch l.sess.State {
	case Running, NeedsHelp:
		m.unlockedOpLocked(l, func(ctx context.Context) {
			// The person's last keys have reached the browser, but
			// Chromium takes the selection a moment after a Ctrl+C: let
			// it land, so it is emptied too.
			if settle(ctx) {
				m.emptyClipboard(ctx, id, profile)
			}
		})
	case Stopping:
		// The container takes the clipboard along; if the stop fails
		// instead, runStop empties it before agents are let back in.
		l.clipboardLeft = true
	}
	l.human, l.releasing = false, false
	l.lastActivity = m.now()
	// Only now, the clipboard emptied and agents let back in, may the
	// gate record again (PROJ-23): never while the person's control is
	// still ending. A session stopped meanwhile has nothing to record.
	if m.findLocked(id) == l {
		m.regateLocked(l)
	}
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

// emptyClipboard empties a running browser's clipboard, a person's
// control ending (PROJ-194). A failure is logged without what the
// clipboard held, which BrowserPier never sees here; not when the session
// ended meanwhile (ctx), its container taking the clipboard along.
func (m *Manager) emptyClipboard(ctx context.Context, sessionID, profile string) {
	if err := m.control(ctx, profile, http.MethodDelete, "/input/clipboard", nil, nil); err != nil && ctx.Err() == nil {
		m.log.Warn("couldn't empty the browser's clipboard as a person's control ended", "session", sessionID, "failure", controlFailure(err))
	}
}

// TakeoverHeader names, on each operation a person's live view sends the
// browser, the takeover it is for (its generation); FenceHeader, on the
// lift as a takeover ends, the takeover to fence, or "all": the browser
// refuses, from then on, anything from it or an earlier one, however
// long it was held up on its way (images/chromium/entrypoint/fence.go,
// PROJ-196).
const (
	TakeoverHeader = "X-BrowserPier-Takeover"
	FenceHeader    = "X-BrowserPier-Fence"
)

// fenceTakeover has the browser fence takeover (a generation, or "all")
// and let go of every key and button the person still holds (a key left
// down would be the next driver's: a modifier, a drag). It tries until
// the browser answers or ctx ends, waiting a little longer each time,
// and returns the fence the browser answered (0 if it didn't say). A
// failure is logged by its kind alone (controlFailure).
func (m *Manager) fenceTakeover(ctx context.Context, sessionID, profile, takeover string) (fence uint64, ok bool) {
	wait := liftRetry
	for try := 1; ; try++ {
		var out struct {
			Fence uint64 `json:"fence"`
		}
		err := m.controlWith(ctx, profile, http.MethodPost, "/input/release", map[string]string{FenceHeader: takeover}, nil, &out)
		if err == nil {
			return out.Fence, true
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
