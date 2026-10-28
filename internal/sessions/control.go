package sessions

import "time"

// agentQuiet is how long after an agent's last click or keystroke a
// person can take control: within it, the agent counts as driving.
const agentQuiet = 5 * time.Second

// AgentInput records an agent's click or keystroke. It fails while a
// person has control.
func (m *Manager) AgentInput(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return ErrNotRunning
	}
	if l.human {
		return ErrHumanDriving
	}
	now := m.now()
	l.lastActivity, l.lastAgentInput = now, now
	return nil
}

// TakeControl gives a person in the live view the mouse and keyboard.
// It is refused while an agent is driving: connected over CDP, or having
// clicked or typed in the last few seconds. Meanwhile agents can only
// watch: their input and new CDP connections are refused. During a
// handoff (Needs help) the agent is paused, so it always succeeds.
func (m *Manager) TakeControl(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return ErrNotRunning
	}
	if l.sess.State == Running && (len(l.conns) > 0 || m.now().Sub(l.lastAgentInput) < agentQuiet) {
		return ErrAgentDriving
	}
	l.human = true
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
