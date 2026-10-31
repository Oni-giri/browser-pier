package sessions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
)

// Reconcile matches the database against the containers Docker has, once,
// before the manager is used. Browsers that kept running while the
// orchestrator was down are picked up where they were, unless an older
// BrowserPier started them (docker.Spec), in which case they are stopped
// the usual way, logins kept; sessions whose container is gone are failed;
// containers with no live session are stopped.
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

	// Browsers an older BrowserPier started (docker.Spec) are stopped
	// before anything attaches to them: running ones by session, starting
	// and already stopping ones by container.
	var outdated, outdatedStopping []string
	type starting struct {
		l   *live
		cid string
	}
	var outdatedStarting []starting
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
		case c.Spec != docker.Spec && s.State == Starting:
			outdatedStarting = append(outdatedStarting, starting{l, c.ID})
			continue
		case c.Spec != docker.Spec && (s.State == Running || s.State == NeedsHelp):
			outdated = append(outdated, s.ID)
		}
		l.lastActivity = m.now() // idle time restarts with the orchestrator
		m.live[s.Profile] = l
		m.log.Info("picked up a running browser", "session", s.ID, "profile", s.Profile, "state", s.State)
		m.watch(l, c.ID)
		switch s.State {
		case Starting:
			cid := c.ID
			m.spawn(func() { m.awaitReady(l, cid) })
		case Stopping:
			l.markReady()
			if c.Spec != docker.Spec {
				// Waited for below; the watcher ends the session.
				outdatedStopping = append(outdatedStopping, c.ID)
			} else {
				m.stopContainer(c.ID)
			}
		default:
			l.markReady()
		}
	}
	m.mu.Unlock()

	// They lack what BrowserPier has added since, such as the egress
	// filter, so they don't run on under this one. If one won't stop,
	// BrowserPier doesn't start either; the next start tries again.
	errs := make([]error, len(outdated)+len(outdatedStarting)+len(outdatedStopping))
	var old sync.WaitGroup
	for i, id := range outdated {
		m.log.Info("stopping a browser an older BrowserPier started", "session", id)
		old.Go(func() {
			ctx, cancel := context.WithTimeout(m.ctx, stopTimeout+30*time.Second)
			defer cancel()
			if _, err := m.Stop(ctx, id); err != nil {
				errs[i] = fmt.Errorf("stopping session %s, which an older BrowserPier started: %w", id, err)
			}
		})
	}
	for i, o := range outdatedStarting {
		m.log.Info("stopping a browser an older BrowserPier was starting", "session", o.l.sess.ID)
		old.Go(func() {
			ctx, cancel := context.WithTimeout(m.ctx, stopTimeout+30*time.Second)
			defer cancel()
			// Stopped first: removing a running container fails.
			if err := m.rt.Stop(ctx, o.cid); err != nil {
				errs[len(outdated)+i] = fmt.Errorf("stopping container %s, which an older BrowserPier started: %w", o.cid, err)
				return
			}
			m.abort(o.l, o.cid, "BrowserPier was updated before the browser started")
		})
	}
	for i, cid := range outdatedStopping {
		old.Go(func() {
			ctx, cancel := context.WithTimeout(m.ctx, stopTimeout+30*time.Second)
			defer cancel()
			if err := m.rt.Stop(ctx, cid); err != nil {
				errs[len(outdated)+len(outdatedStarting)+i] = fmt.Errorf("stopping container %s, which an older BrowserPier started: %w", cid, err)
			}
		})
	}
	old.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}

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
