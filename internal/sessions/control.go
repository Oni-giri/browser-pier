package sessions

import (
	"context"
	"net/http"
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
	in := &lease{cancel: cancel, started: now, call: CallFrom(ctx)}
	if in.call != nil {
		// Granted: the call is admitted (its receipt stays, PROJ-156).
		in.call.admitLocked()
	}
	if l.inputs == nil {
		l.inputs = map[*lease]struct{}{}
	}
	l.inputs[in] = struct{}{}
	m.agentActedLocked(l)
	return cctx, sync.OnceFunc(func() {
		// Ended before done: a handoff, Take control, a stop or the
		// caller going away cut it short.
		// (A cut that comes after the browser answered, just before this,
		// still counts: the receipt then errs on the safe side.)
		cut := cctx.Err() != nil
		cancel(context.Canceled)
		m.mu.Lock()
		if cut && in.call != nil {
			in.call.cut = true
		}
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
// paused, so it always succeeds.
func (m *Manager) TakeControl(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return ErrNotRunning
	}
	now := m.now()
	breakOff := false
	if l.sess.State == Running {
		if len(l.conns) > 0 {
			return ErrAgentDriving
		}
		for ls := range l.leases {
			if !ls.breakable || now.Sub(ls.started) < takeOverAfter {
				return ErrAgentDriving
			}
			breakOff = true
		}
		for in := range l.inputs {
			if now.Sub(in.started) < takeOverAfter {
				return ErrAgentDriving
			}
			breakOff = true
		}
		if !breakOff && now.Sub(l.lastAgentInput) < agentQuiet {
			return ErrAgentDriving
		}
	}
	if l.human {
		return ErrHumanDriving // one person at a time
	}
	l.human = true
	// Recording stops here, under m.mu and before anything else: no
	// fragment the browser sends from now on is kept, and HumanInput,
	// which the live view needs before it forwards any input, can't see
	// human until it has.
	m.personTookLocked(l)
	l.generation++
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
		m.waitLeasesLocked(l)
		if m.findLocked(id) != l || !l.sess.State.Live() {
			return ErrNotRunning // stopped meanwhile
		}
	}
	return nil
}

// HumanInput records that the person in control used the browser. It
// fails unless they hold control. Call done once the input has reached
// the browser: ReleaseControl waits for input under way, so none lands
// (text put on the clipboard, say) after it has emptied the clipboard.
func (m *Manager) HumanInput(id string) (done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human || l.releasing || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return nil, ErrNotRunning
	}
	l.lastActivity = m.now()
	l.humanInputs++
	return sync.OnceFunc(func() {
		m.mu.Lock()
		l.humanInputs--
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	}), nil
}

// ReleaseControl hands the browser back to agents, whatever ends the
// person's control: they hand back, a handoff ends, the live view goes.
// First it empties the browser's clipboard and PRIMARY selection
// (PROJ-194), so no agent can paste what the person put there, copied or
// selected in the page: it waits for their input under way and a moment
// more (clipboardSettle), refuses more, and only then lets agents in. If
// the clipboard can't be emptied, that is logged and the browser handed
// back all the same. A browser that is stopping keeps its clipboard: the
// container takes it along, or, if the stop fails, runStop empties it
// before agents get the browser back. Recording (PROJ-23) stays off
// throughout, and the
// gate is asked again only as agents get the browser back. It returns
// once agents have the browser.
func (m *Manager) ReleaseControl(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human {
		return
	}
	if l.releasing {
		// Another hand-back is emptying the clipboard: done when it is.
		for m.findLocked(id) == l && l.human {
			m.inputsDone.Wait()
		}
		return
	}
	l.releasing = true
	for l.humanInputs > 0 {
		m.inputsDone.Wait()
	}
	switch l.sess.State {
	case Running, NeedsHelp:
		profile := l.sess.Profile
		m.mu.Unlock()
		// The person's last keys have reached the browser, but Chromium
		// takes the selection a moment after a Ctrl+C: let it land, so it
		// is emptied too.
		time.Sleep(clipboardSettle)
		m.emptyClipboard(id, profile)
		m.mu.Lock()
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

// emptyClipboard empties a running browser's clipboard, a person's
// control ending (PROJ-194). A failure is logged without what the
// clipboard held, which BrowserPier never sees here.
func (m *Manager) emptyClipboard(sessionID, profile string) {
	if err := m.control(m.ctx, profile, http.MethodDelete, "/input/clipboard", nil, nil); err != nil {
		m.log.Warn("couldn't empty the browser's clipboard as a person's control ended", "session", sessionID, "err", err)
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
