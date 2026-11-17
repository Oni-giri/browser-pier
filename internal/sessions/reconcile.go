package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
)

// Reconcile matches the database against the containers Docker has, once,
// before the manager is used. Browsers that kept running while the
// orchestrator was down are picked up where they were, unless an older
// BrowserPier started them (docker.Spec) or BrowserPier had typed a secret
// into their page (Session.HadFills: the values were in memory only, so
// answers would no longer hide them), in which case they are stopped the
// usual way, logins kept; sessions whose container is gone are failed;
// containers with no live session are stopped.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.removeTombstones()
	if m.recStore != nil {
		// Recordings of profiles that are gone go, with half-written
		// segments and those past the retention.
		ps, err := m.profiles.List(ctx)
		if err != nil {
			return err
		}
		names := make([]string, len(ps))
		for i, p := range ps {
			names[i] = p.Name
		}
		if err := m.recStore.Reconcile(names); err != nil {
			m.log.Warn("checking the recordings folder", "err", err)
		}
	}
	m.removeProxyTestDir()
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
	var outdatedStopping []string
	type stop struct {
		id, why string
		level   slog.Level
	}
	var outdated []stop
	type starting struct {
		l   *live
		cid string
	}
	var outdatedStarting []starting
	// Browsers picked up running: a person may have had control as the
	// orchestrator went, with text on the clipboard.
	type pickedUp struct{ id, profile string }
	var running []pickedUp
	m.mu.Lock()
	for _, s := range rows {
		c, ok := byID[s.ContainerID]
		if ok {
			delete(byID, c.ID)
		}
		l := newLive(s)
		if p, err := m.profiles.Get(ctx, s.Profile); err == nil {
			// Recorded again by the same rules: a Needs help session
			// isn't, and a running one only from the agent's next action.
			l.record = p.Record
		}
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
		case m.rt.Outdated(c.Spec) && s.State == Starting:
			outdatedStarting = append(outdatedStarting, starting{l, c.ID})
			continue
		case m.rt.Outdated(c.Spec) && (s.State == Running || s.State == NeedsHelp):
			outdated = append(outdated, stop{s.ID, "an older BrowserPier started", slog.LevelInfo})
		case s.HadFills && (s.State == Running || s.State == NeedsHelp):
			outdated = append(outdated, stop{s.ID, "had secrets typed into it, which went with the restart", slog.LevelWarn})
		case s.State == Running || s.State == NeedsHelp:
			running = append(running, pickedUp{s.ID, s.Profile})
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
			if m.rt.Outdated(c.Spec) {
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

	// A person's control ended with the restart: their clipboard is
	// emptied before agents use the browser again, as at any hand-back
	// (PROJ-194). Recording (PROJ-23) waits for an agent's next action,
	// which can't come before Reconcile has returned.
	var emptied sync.WaitGroup
	for _, p := range running {
		emptied.Go(func() { m.emptyClipboard(p.id, p.profile) })
	}
	emptied.Wait()

	// They lack what BrowserPier has added since, such as the egress
	// filter, so they don't run on under this one. If one won't stop,
	// BrowserPier doesn't start either; the next start tries again.
	errs := make([]error, len(outdated)+len(outdatedStarting)+len(outdatedStopping))
	var old sync.WaitGroup
	// A browser with secrets typed into it is stopped for the same reason:
	// its answers can no longer hide them (PROJ-125).
	for i, o := range outdated {
		m.log.Log(m.ctx, o.level, "stopping a browser that "+o.why+"; its logins are kept", "session", o.id)
		old.Go(func() {
			ctx, cancel := context.WithTimeout(m.ctx, stopTimeout+30*time.Second)
			defer cancel()
			if _, err := m.Stop(ctx, o.id); err != nil {
				errs[i] = fmt.Errorf("stopping session %s, which %s: %w", o.id, o.why, err)
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
