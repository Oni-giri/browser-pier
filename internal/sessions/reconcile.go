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
	type pickedUp struct {
		l           *live
		id, profile string
		gen         uint64
		ctx         context.Context
	}
	var running []pickedUp
	m.mu.Lock()
	for _, s := range rows {
		c, ok := byID[s.ContainerID]
		if ok {
			delete(byID, c.ID)
		}
		l := newLive(m.ctx, s)
		// Takeovers go on from the highest one handed out, saved before
		// it was (PROJ-196): the browser may have seen any of them.
		l.generation = s.takeoverGen
		if s.State == NeedsHelp {
			if l.sess.helpEpoch == 0 {
				// Waiting for help since before help epochs were saved:
				// one of its own, never 0 (HelpEpoch), saved.
				l.sess.helpEpoch = 1
				if err := updateSession(ctx, m.db, l.sess); err != nil {
					m.log.Error("saving session state", "session", s.ID, "err", err)
				}
			}
			// The open handoff names this help, so its end ends it.
			if _, err := m.db.ExecContext(ctx, `UPDATE handoffs SET help_epoch = ? WHERE session_id = ? AND status = 'pending'`, l.sess.helpEpoch, s.ID); err != nil {
				m.log.Error("saving the open handoff's help epoch", "session", s.ID, "err", err)
			}
		}
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
			// Admitted for this browser alone (sockOpLocked): if it
			// ended meanwhile, the call can't reach the next one.
			if ctx, ok := m.sockOpLocked(l); ok {
				running = append(running, pickedUp{l, s.ID, s.Profile, l.generation, ctx})
			}
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

	// A person's control ended with the restart: whatever key or button
	// they held is let go, and their clipboard emptied, before agents use
	// the browser again, as at any hand-back (PROJ-194). Recording
	// (PROJ-23) waits for an agent's next action, which can't come before
	// Reconcile has returned.
	var emptied sync.WaitGroup
	var unfencedMu sync.Mutex
	var unfenced []stop
	for _, p := range running {
		emptied.Go(func() {
			defer func() {
				m.mu.Lock()
				m.sockOpDoneLocked(p.l)
				m.mu.Unlock()
			}()
			// Every takeover the BrowserPier before this one handed out is
			// fenced, up to the highest it saved (before handing it out),
			// whatever it left under way in the browser, still in its
			// control socket or queued in xdotool; held keys are let go.
			// This BrowserPier numbers its takeovers above it. A browser
			// that doesn't fence it within reconcileFenceWait is stopped
			// below rather than handed to agents unfenced.
			fctx, cancel := context.WithTimeout(p.ctx, reconcileFenceWait)
			_, ok := m.fenceTakeover(fctx, p.id, p.profile, p.gen)
			cancel()
			if !ok {
				unfencedMu.Lock()
				unfenced = append(unfenced, stop{p.id, "didn't answer as BrowserPier restarted, so what a person's control may have left under way in it couldn't be fenced", slog.LevelWarn})
				unfencedMu.Unlock()
				return
			}
			// As ReleaseControl does: a copy pressed just before the
			// restart lands first, and is emptied too.
			if settle(p.ctx) {
				m.emptyClipboard(p.ctx, p.id, p.profile, p.gen)
			}
			// A browser left waiting for help whose handoff has ended (the
			// restart came between the two) is the agents' again, now
			// that it is fenced.
			m.mu.Lock()
			defer m.mu.Unlock()
			if p.l.sess.State != NeedsHelp {
				return
			}
			if open, err := pendingHandoff(context.Background(), m.db, p.id); err == nil && !open {
				p.l.sess.State = Running
				p.l.sess.UpdatedAt = m.now()
				if err := updateSession(context.Background(), m.db, p.l.sess); err != nil {
					m.log.Error("saving session state", "session", p.id, "err", err)
				}
				m.log.Info("a browser left waiting for a handoff that had ended is the agents' again", "session", p.id)
			}
		})
	}
	emptied.Wait()
	outdated = append(outdated, unfenced...)

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

// reconcileFenceWait bounds Reconcile's fencing of each browser it picks
// up (fenceTakeover). Tests shorten it.
var reconcileFenceWait = 10 * time.Second
