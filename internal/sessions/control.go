package sessions

import (
	"context"
	"sync"
	"time"
)

// agentQuiet is how long after an agent's last click or keystroke a
// person can take control: within it, the agent counts as driving.
const agentQuiet = 5 * time.Second

// AgentInput records an agent's click or keystroke. It fails while a
// person has control.
//
// It is a lease: call done once the browser has answered. A handoff
// (Needs help) or a stop cancels the context it returns, so an action
// that takes a while (typing at human pace, PROJ-113) breaks off at once,
// and waits for leases to end before the person gets the browser; it
// refuses new ones, so no agent input lands under them. A person can't
// take control from the live view while one is under way, nor for
// agentQuiet after it ends.
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
	now := m.now()
	l.lastActivity, l.lastAgentInput = now, now
	cctx, cancel := context.WithCancelCause(ctx)
	in := &lease{cancel: cancel}
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
// clicked or typed in the last few seconds, or with a page call (PageLease)
// under way or just ended. Meanwhile agents can only
// watch: their input and new CDP connections are refused. During a
// handoff (Needs help) the agent is paused, so it always succeeds.
func (m *Manager) TakeControl(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return ErrNotRunning
	}
	if l.sess.State == Running && (len(l.conns) > 0 || len(l.leases) > 0 || len(l.inputs) > 0 || m.now().Sub(l.lastAgentInput) < agentQuiet) {
		return ErrAgentDriving
	}
	if l.human {
		return ErrHumanDriving // one person at a time
	}
	l.human = true
	l.generation++
	l.lastActivity = m.now()
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
