package sessions

import (
	"context"
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
	return cctx, sync.OnceFunc(func() {
		cancel(context.Canceled)
		m.mu.Lock()
		delete(l.inputs, in)
		// The agent counts as driving for agentQuiet after its last
		// event, not after the action began.
		now := m.now()
		l.lastActivity, l.lastAgentInput = now, now
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
// fails unless they hold control.
func (m *Manager) HumanInput(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return ErrNotRunning
	}
	l.lastActivity = m.now()
	return nil
}

// ReleaseControl hands the browser back to agents.
func (m *Manager) ReleaseControl(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.findLocked(id); l != nil && l.human {
		l.human = false
		l.lastActivity = m.now()
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
