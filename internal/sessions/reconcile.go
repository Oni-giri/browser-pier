package sessions

import (
	"context"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
)

// Reconcile matches the database against the containers Docker has, once,
// before the manager is used. Browsers that kept running while the
// orchestrator was down are picked up where they were; sessions whose
// container is gone are failed; containers with no live session are stopped.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.removeTombstones()
	containers, err := m.rt.List(ctx)
	if err != nil {
		return err
	}
	rows, err := liveSessions(ctx, m.db)
	if err != nil {
		return err
	}
	byID := make(map[string]docker.Container, len(containers))
	for _, c := range containers {
		byID[c.ID] = c
	}

	m.mu.Lock()
	for _, s := range rows {
		c, ok := byID[s.ContainerID]
		if ok {
			delete(byID, c.ID)
		}
		l := newLive(s)
		switch {
		case !ok:
			m.mu.Unlock()
			m.end(l, "", func(s *Session) {
				s.State, s.Error = Failed, "the browser container was gone when BrowserPier restarted"
			})
			m.mu.Lock()
			continue
		case c.State == "created":
			m.mu.Unlock()
			m.abort(l, c.ID, "BrowserPier restarted before the browser started")
			m.mu.Lock()
			continue
		}
		m.live[s.Profile] = l
		m.log.Info("picked up a running browser", "session", s.ID, "profile", s.Profile, "state", s.State)
		m.watch(l, c.ID)
		switch s.State {
		case Starting:
			cid := c.ID
			m.spawn(func() { m.awaitReady(l, cid) })
		case Stopping:
			l.markReady()
			m.stopContainer(c.ID)
		default:
			l.markReady()
		}
	}
	m.mu.Unlock()

	// Strays are stopped before Reconcile returns, so their names are free
	// by the time the first Start comes in. A failure is logged, not fatal:
	// it only blocks the one profile whose name the stray holds.
	var strays sync.WaitGroup
	for _, c := range byID {
		m.log.Warn("stopping a browser container with no session", "container", c.Name)
		strays.Add(1)
		go func(c docker.Container) {
			defer strays.Done()
			ctx, cancel := context.WithTimeout(m.ctx, stopTimeout+30*time.Second)
			defer cancel()
			if c.State == "running" {
				if err := m.rt.Stop(ctx, c.ID); err != nil {
					m.log.Warn("stopping an orphaned browser container", "container", c.Name, "err", err)
					return
				}
			}
			if err := m.rt.Remove(ctx, c.ID); err != nil {
				m.log.Warn("removing an orphaned browser container", "container", c.Name, "err", err)
			}
		}(c)
	}
	strays.Wait()
	return nil
}
