package sessions

import (
	"context"
	"errors"
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
// paused, so it always succeeds.
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
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return 0, ErrNotRunning
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
			return 0, ErrNotRunning // stopped meanwhile
		}
	}
	return gen, nil
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
func (m *Manager) HumanInput(id string, gen uint64) (done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil {
		return nil, ErrNotRunning
	}
	if !l.human || l.humanGen != gen || l.releasing {
		return nil, ErrNotInControl
	}
	if l.sess.State != Running && l.sess.State != NeedsHelp {
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
	m.releaseLocked(l, id, nil)
}

// ReleaseControlGen is ReleaseControl for the takeover of generation gen
// (TakeControlGen) alone: if that control has ended already, however, it
// does nothing and returns false, so a live view that missed the end of
// its control never ends, or empties the clipboard of, the takeover of
// whoever took the browser since (PROJ-196). The check and the release
// are one step under m.mu, and no one else can take control until the
// release is over. lift, if set, runs once the takeover is known to be
// this one's and to be ending, the person's input under way done, before
// the clipboard is emptied: it lets go of keys and buttons the person
// held, never anyone else's.
func (m *Manager) ReleaseControlGen(id string, gen uint64, lift func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human || l.humanGen != gen {
		return false
	}
	if l.releasing {
		// Ended for them meanwhile (a handoff, the profile page): that
		// hand-back finishes it.
		m.releaseLocked(l, id, nil)
		return false
	}
	m.releaseLocked(l, id, lift)
	return true
}

// releaseLocked ends l's person's control (see ReleaseControl); m.mu is
// held, and let go of meanwhile.
func (m *Manager) releaseLocked(l *live, id string, lift func()) {
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
	if lift != nil {
		// Still this person's: human stays set, so no one can take
		// control, until the end below.
		m.mu.Unlock()
		lift()
		m.mu.Lock()
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
