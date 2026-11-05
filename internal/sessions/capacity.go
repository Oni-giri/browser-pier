package sessions

import (
	"context"
	"errors"
	"fmt"
	"math"
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

// memLimits is the memory cap and what a new browser is expected to take.
type memLimits struct{ limit, each int64 }

// SetHostFree has a new browser fit in what the host has free, as well as
// under the memory limit. free reads it now, other apps included; when it
// can't, the host isn't checked. It must be called before any browser
// starts.
func (m *Manager) SetHostFree(free func() (int64, error)) { m.hostFree = free }

// SetMemoryLimit caps the memory all browsers may use together (0: no
// cap), expecting each new browser to need perBrowser. It can be called
// at any time; the new cap applies to the next start, and browsers
// already running aren't stopped for it.
func (m *Manager) SetMemoryLimit(limit, perBrowser int64) {
	m.mem.Store(&memLimits{limit, perBrowser})
}

// MemoryLimit is the current cap on the memory all browsers may use
// together; 0 means none.
func (m *Manager) MemoryLimit() int64 {
	limit, _ := m.memLimits()
	return limit
}

func (m *Manager) memLimits() (limit, each int64) {
	if l := m.mem.Load(); l != nil {
		return l.limit, l.each
	}
	return 0, 0
}

// MemoryReport is what the live browsers use against the memory limit,
// counted the way a new start is admitted.
type MemoryReport struct {
	Limit, Used, Each int64 // Limit is 0 when there is none
	Browsers          []Usage
}

// Room is how many more browsers fit under the limit, or -1 when there is
// no limit to count against.
func (r MemoryReport) Room() int {
	if r.Limit <= 0 || r.Each <= 0 {
		return -1
	}
	return int(max(r.Limit-r.Used, 0) / r.Each)
}

// Memory reports what the live browsers use, for the web UI.
func (m *Manager) Memory(ctx context.Context) MemoryReport {
	limit, each := m.memLimits()
	used, _, browsers := m.memoryInUse(ctx, each)
	return MemoryReport{Limit: limit, Used: used, Each: each, Browsers: browsers}
}

// ErrNoRoom is returned, wrapped in a *NoRoomError, when a new browser
// wouldn't fit under the memory limit, or in what the host has free, and
// none could be stopped for it.
var ErrNoRoom = errors.New("not enough memory for another browser")

// NoRoomError says what is using the memory. Its Error names no
// profile; callers show each one only the browsers it may see.
type NoRoomError struct {
	Limit, Used int64
	// Host is set when the host is what's short: it has Free left for a
	// browser expected to need Each, once browsers still booting have
	// their share (Booting).
	Host, Booting bool
	Free, Each    int64
	Running       []Usage
}

// Usage is one live browser's share of memory.
type Usage struct {
	Profile string
	Bytes   int64
	Why     string // "in use by claude", "a person has it", "starting"…
}

func (u Usage) String() string { return fmt.Sprintf("%s (%s, %s)", u.Profile, gb(u.Bytes), u.Why) }

func (e *NoRoomError) Error() string { return fmt.Sprintf("%v: %s", ErrNoRoom, e.Why()) }

// Why says what ran out, naming no profile.
func (e *NoRoomError) Why() string {
	why := fmt.Sprintf("%s of the %s limit in use", gb(e.Used), gb(e.Limit))
	if e.Host {
		// Rounded down: 0.96 GB shown as 1.0 GB would look like enough.
		free := math.Floor(float64(e.Free)/(1<<30)*10) / 10
		booting := ""
		if e.Booting {
			booting = " once browsers starting now have their share"
		}
		why = fmt.Sprintf("the host has %.1f GB free, other apps included,%s and a browser needs about %s", free, booting, gb(e.Each))
	}
	if len(e.Running) == 0 {
		return why + ", with no browser running to stop"
	}
	return why + ", and every running browser is in use"
}

func (e *NoRoomError) Unwrap() error { return ErrNoRoom }

// GB formats bytes for people.
func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }

// makeRoom makes sure a new browser fits under the memory limit and in
// what the host has free, stopping the longest-idle browser no agent is
// using until it does. If none can be stopped, it refuses with what is
// running.
func (m *Manager) makeRoom(ctx context.Context) error {
	limit, each := m.memLimits()
	if each <= 0 || (limit <= 0 && m.hostFree == nil) {
		return nil
	}
	for range 32 {
		used, growing, running := m.memoryInUse(ctx, each)
		underLimit := limit <= 0 || used+each <= limit
		// Browsers still booting haven't taken their share from the host
		// yet: what it has free must cover theirs too.
		free, inHost := int64(0), true
		if m.hostFree != nil {
			if n, err := m.hostFree(); err != nil {
				m.log.Warn("reading the host's free memory; starting without checking it", "err", err)
			} else {
				free = max(n-growing, 0)
				inHost = each <= free
			}
		}
		if underLimit && inHost {
			return nil
		}
		victim, idle := m.longestIdle()
		if victim == "" {
			list := make([]string, len(running))
			for i, u := range running {
				list[i] = u.String()
			}
			full := &NoRoomError{Limit: limit, Used: used, Host: underLimit, Booting: growing > 0, Free: free, Each: each, Running: running}
			m.log.Warn("refused a browser: no room", "why", full.Why(), "running", strings.Join(list, "; "))
			return full
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
// A browser whose usage can't be read counts as each, what a new one is
// expected to take. growing is what browsers still booting are counted
// for beyond what they use yet.
func (m *Manager) memoryInUse(ctx context.Context, each int64) (total, growing int64, running []Usage) {
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
	for _, e := range entries {
		used, read := each, int64(0)
		if e.cid != "" {
			if n, err := m.rt.MemoryUsage(ctx, e.cid); err == nil {
				used, read = n, n
				// A browser still booting will grow: count at least what a
				// new one is expected to take.
				if e.state == Starting {
					used = max(n, each)
				}
			}
		}
		if e.state == Starting {
			growing += used - read
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
	return total, growing, running
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
