package sessions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Oni-giri/browser-pier/internal/recording"
)

// Recorder records sessions' screens (PROJ-23); *recording.Recorder is
// the real one.
type Recorder interface {
	// Set says whether session id may be recorded now. It is called with
	// m.mu held and must not block: when allow is false, nothing the
	// browser sends after it returns may be kept.
	Set(id, profile string, allow bool, why recording.Reason)
	// End says the session is over.
	End(id string)
}

// SetRecorder gives the manager its recorder and the store recordings
// are kept in (for DeleteProfile and Reconcile). Call it before Reconcile.
func (m *Manager) SetRecorder(rec Recorder, store *recording.Store) {
	m.rec, m.recStore = rec, store
}

// RecordStream opens the profile's browser's recording stream (GET
// /record on its control socket), for the recorder.
func (m *Manager) RecordStream(ctx context.Context, profile string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/record", nil)
	if err != nil {
		return nil, err
	}
	// The host is the profile's Unix socket; the path is fixed.
	resp, err := controlClient(m.layout.ControlSocket(profile)).Do(req) //nolint:gosec // see above
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// The status alone: the container's words don't go to the log.
		_ = resp.Body.Close()
		return nil, fmt.Errorf("recording stream: %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// regateLocked asks the gate whether the session's screen may be
// recorded now and tells the recorder if the answer changed. It is the
// only way to the recorder: every change to what the gate reads (the
// state, human, the agent's connections, calls and inputs, the profile's
// setting, a pause) calls it, under m.mu, before the change takes effect
// for anyone else. Recording is not activity: it never touches
// lastActivity, so a recorded browser stops as idle as any other.
func (m *Manager) regateLocked(l *live) {
	if m.rec == nil {
		return
	}
	now := m.now()
	acting := len(l.conns) > 0 || len(l.leases) > 0 || len(l.inputs) > 0
	tail := !acting && !l.agentAt.IsZero() && now.Sub(l.agentAt) < recording.Tail
	allow, why := recording.Gate{
		Enabled:     l.record,
		Running:     l.sess.State == Running,
		NeedsHelp:   l.sess.State == NeedsHelp,
		Human:       l.human,
		Paused:      l.recPauses > 0,
		AgentActive: acting || tail,
	}.Decide()
	if allow != l.recAllow || why != l.recWhy {
		l.recAllow, l.recWhy = allow, why
		// Until the first yes there is nothing to stop, and no gap to note.
		if allow || l.recEver {
			l.recEver = true
			m.rec.Set(l.sess.ID, l.sess.Profile, allow, why)
		}
	}
	if l.recTimer != nil {
		l.recTimer.Stop()
		l.recTimer = nil
	}
	if allow && tail {
		// The tail ends without anything happening: look again then.
		l.recTimer = time.AfterFunc(l.agentAt.Add(recording.Tail).Sub(now)+10*time.Millisecond, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.live[l.sess.Profile] == l {
				m.regateLocked(l)
			}
		})
	}
}

// agentActedLocked notes that an agent began or ended an action, for the
// recording's tail, and asks the gate again. One that ends while a person
// has the browser (broken off by Take control or a handoff) doesn't
// count: after hand-back, recording waits for the agent's next action.
func (m *Manager) agentActedLocked(l *live) {
	if !l.human && l.sess.State == Running {
		l.agentAt = m.now()
	}
	m.regateLocked(l)
}

// personTookLocked: a person has the browser. Recording stops now, and
// after hand-back waits for the agent's next action: the tail before
// doesn't carry over.
func (m *Manager) personTookLocked(l *live) {
	l.agentAt = time.Time{}
	m.regateLocked(l)
}

// PauseRecording holds recording of a running session off until resume
// is called, for work whose screen mustn't be kept, such as typing a
// secret into a visible field (PROJ-57 decides). Pauses nest.
func (m *Manager) PauseRecording(id string) (resume func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.sess.State.Live() {
		return nil, ErrNotRunning
	}
	l.recPauses++
	m.regateLocked(l)
	done := false
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !done {
			done = true
			l.recPauses--
			if m.live[l.sess.Profile] == l {
				m.regateLocked(l)
			}
		}
	}, nil
}

// RefreshRecording applies the profile's Record sessions setting to its
// running session at once, after the admin changed it. The setting is
// read under m.mu, as Reconcile reads it, so two saves close together
// apply in order: the last one applied is the one read last.
func (m *Manager) RefreshRecording(ctx context.Context, profile string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The save has happened: a request cancelled while it waited for the
	// lock must not leave the browser on the old setting.
	p, err := m.profiles.Get(context.WithoutCancel(ctx), profile)
	if err != nil {
		return err
	}
	if l, ok := m.live[profile]; ok {
		l.record = p.Record
		m.regateLocked(l)
	}
	return nil
}

// Recording reports whether the session's screen is being recorded now.
func (m *Manager) Recording(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	return l != nil && l.recAllow
}
