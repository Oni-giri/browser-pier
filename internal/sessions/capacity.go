package sessions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Touch records that an agent or person just used the session's browser,
// so it isn't idle.
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.findLocked(id); l != nil {
		l.lastActivity = m.now()
	}
}

// idleLocked is how long the browser has gone unused. A browser with an
// agent connected, someone in live view, or one that is starting,
// stopping or with a person during a handoff, is never idle.
func (l *live) idleLocked(now time.Time) time.Duration {
	if len(l.conns) > 0 || l.viewers > 0 || l.human || l.sess.State != Running {
		return 0
	}
	return now.Sub(l.lastActivity)
}

// WatchIdle stops browsers left idle longer than their profile's
// auto-stop, checking every interval until ctx ends. Stopping is graceful,
// so logins are kept.
func (m *Manager) WatchIdle(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.stopIdle(ctx)
		}
	}
}

func (m *Manager) stopIdle(ctx context.Context) {
	type candidate struct {
		id, profile string
		idle        time.Duration
	}
	m.mu.Lock()
	now := m.now()
	var cands []candidate
	for _, l := range m.live {
		if idle := l.idleLocked(now); idle > 0 {
			cands = append(cands, candidate{l.sess.ID, l.sess.Profile, idle})
		}
	}
	m.mu.Unlock()
	for _, c := range cands {
		p, err := m.profiles.Get(ctx, c.profile)
		if err != nil || p.AutoStop <= 0 || c.idle < p.AutoStop {
			continue
		}
		limit := p.AutoStop
		_, err = m.stopIf(ctx, c.id, func(l *live) bool { return l.idleLocked(m.now()) >= limit })
		if errors.Is(err, errStillInUse) {
			continue // used again since we looked
		}
		m.log.Info("stopped an idle browser", "profile", c.profile, "session", c.id, "idle", c.idle.Round(time.Second))
		if err != nil {
			m.log.Warn("stopping an idle browser", "profile", c.profile, "err", err)
		}
	}
}

// SetMemoryLimit caps the memory all browsers may use together (0: no
// cap), expecting each new browser to need perBrowser. Call it before Start.
func (m *Manager) SetMemoryLimit(limit, perBrowser int64) {
	m.memLimit, m.memEach = limit, perBrowser
}

// ErrNoRoom is returned, wrapped in a *NoRoomError, when a new browser
// wouldn't fit under the memory limit and none could be stopped for it.
var ErrNoRoom = errors.New("not enough memory for another browser")

// NoRoomError says what is using the memory. Its Error names no
// profile; callers show each one only the browsers it may see.
type NoRoomError struct {
	Limit, Used int64
	Running     []Usage
}

// Usage is one live browser's share of memory.
type Usage struct {
	Profile string
	Bytes   int64
	Why     string // "in use by claude", "a person has it", "starting"…
}

func (u Usage) String() string { return fmt.Sprintf("%s (%s, %s)", u.Profile, gb(u.Bytes), u.Why) }

func (e *NoRoomError) Error() string {
	return fmt.Sprintf("%v: %s of the %s limit in use, and every running browser is in use", ErrNoRoom, gb(e.Used), gb(e.Limit))
}

func (e *NoRoomError) Unwrap() error { return ErrNoRoom }

// GB formats bytes for people.
func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }

// makeRoom makes sure a new browser fits under the memory limit, stopping
// the longest-idle browser no agent is using until it does. If none can
// be stopped, it refuses with what is running.
func (m *Manager) makeRoom(ctx context.Context) error {
	if m.memLimit <= 0 {
		return nil
	}
	for range 32 {
		used, running := m.memoryInUse(ctx)
		if used+m.memEach <= m.memLimit {
			return nil
		}
		victim, idle := m.longestIdle()
		if victim == "" {
			list := make([]string, len(running))
			for i, u := range running {
				list[i] = u.String()
			}
			m.log.Warn("refused a browser: no room", "used", gb(used), "limit", gb(m.memLimit), "running", strings.Join(list, "; "))
			return &NoRoomError{Limit: m.memLimit, Used: used, Running: running}
		}
		_, err := m.stopIf(ctx, victim, func(l *live) bool { return l.idleLocked(m.now()) > 0 })
		if errors.Is(err, errStillInUse) {
			continue // someone started using it; look again
		}
		if err != nil {
			return fmt.Errorf("making room: %w", err)
		}
		m.log.Info("stopped the longest-idle browser to make room", "session", victim, "idle", idle.Round(time.Second))
	}
	return ErrNoRoom
}

// memoryInUse adds up what the live browsers use, and describes each.
// A browser whose usage can't be read counts as a new one would.
func (m *Manager) memoryInUse(ctx context.Context) (int64, []Usage) {
	type entry struct {
		profile, cid string
		state        State
		agents       []string
	}
	m.mu.Lock()
	var entries []entry
	for _, l := range m.live {
		s := l.snapshotLocked()
		entries = append(entries, entry{s.Profile, s.ContainerID, s.State, s.InUseBy})
	}
	m.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].profile < entries[j].profile })
	var total int64
	var running []Usage
	for _, e := range entries {
		used := m.memEach
		if e.cid != "" {
			if n, err := m.rt.MemoryUsage(ctx, e.cid); err == nil {
				used = n
				// A browser still booting will grow: count at least what a
				// new one is expected to take.
				if e.state == Starting {
					used = max(n, m.memEach)
				}
			}
		}
		total += used
		why := string(e.state)
		if len(e.agents) > 0 {
			why = "in use by " + strings.Join(e.agents, ", ")
		} else if e.state == NeedsHelp {
			why = "a person has it"
		}
		running = append(running, Usage{e.profile, used, why})
	}
	return total, running
}

// longestIdle returns the running session idle longest with no agent
// connected, if any.
func (m *Manager) longestIdle() (string, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var best string
	var most time.Duration
	for _, l := range m.live {
		if idle := l.idleLocked(now); idle > most {
			best, most = l.sess.ID, idle
		}
	}
	return best, most
}
