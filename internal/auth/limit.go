package auth

import (
	"sync"
	"time"
)

// Lockout counts failed attempts per key and locks a key out once it has
// failed too often within a window. Keys are, for example, "admin" or a
// client address. It is in memory: a restart clears it.
type Lockout struct {
	max     int
	window  time.Duration
	lockFor time.Duration
	now     func() time.Time

	mu   sync.Mutex
	keys map[string]*attempts
}

type attempts struct {
	failures    []time.Time // within the window, oldest first
	lockedUntil time.Time
}

// NewLockout locks a key for lockFor after max failures within window.
func NewLockout(max int, window, lockFor time.Duration) *Lockout {
	return &Lockout{max: max, window: window, lockFor: lockFor, now: time.Now, keys: map[string]*attempts{}}
}

// Check reports whether key may try now, and if not, for how long it is locked.
func (l *Lockout) Check(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.keys[key]
	if a == nil {
		return true, 0
	}
	if d := a.lockedUntil.Sub(l.now()); d > 0 {
		return false, d
	}
	return true, 0
}

// Fail records a failed attempt and returns how many are left before a
// lockout (0 means the key is now locked).
func (l *Lockout) Fail(key string) (left int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.keys[key]
	if a == nil {
		a = &attempts{}
		l.keys[key] = a
	}
	a.failures = append(prune(a.failures, now.Add(-l.window)), now)
	if len(a.failures) >= l.max {
		a.lockedUntil = now.Add(l.lockFor)
		a.failures = nil
		return 0
	}
	l.gc(now)
	return l.max - len(a.failures)
}

// Succeed clears a key's failures.
func (l *Lockout) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

func prune(ts []time.Time, after time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(after) {
		i++
	}
	return ts[i:]
}

// gc drops keys with nothing left to remember, so many client addresses
// can't grow the map without bound.
func (l *Lockout) gc(now time.Time) {
	if len(l.keys) < 1024 {
		return
	}
	for k, a := range l.keys {
		if a.lockedUntil.Before(now) && len(prune(a.failures, now.Add(-l.window))) == 0 {
			delete(l.keys, k)
		}
	}
}
