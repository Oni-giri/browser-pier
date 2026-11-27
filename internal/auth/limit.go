package auth

import (
	"sync"
	"time"
)

// Lockout counts failed attempts per key and locks a key out once it has
// failed too often within a window. Keys are, for example, "admin" or a
// client address. It is in memory, so a restart clears it, unless save
// keeps it somewhere restore can read it back from.
//
// An attempt counts from the moment it begins, so requests sent in
// parallel can't all get through before the first failure is recorded.
type Lockout struct {
	max     int
	window  time.Duration
	lockFor time.Duration
	now     func() time.Time

	mu      sync.Mutex
	keys    map[string]*attempts
	changes uint64 // under mu: numbers each change, for save's order

	// save, if set, is given a key's failures and lock after every change
	// to them, outside mu, newest last: a slower call with an older state
	// never overwrites a newer one. Attempts in flight aren't saved.
	save   func(key string, failures []time.Time, lockedUntil time.Time)
	saveMu sync.Mutex
	saved  uint64 // under saveMu
}

// restore puts back what save was given for key.
func (l *Lockout) restore(key string, failures []time.Time, lockedUntil time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.keys[key]
	if a == nil {
		a = &attempts{}
		l.keys[key] = a
	}
	a.failures = append([]time.Time(nil), failures...)
	a.lockedUntil = lockedUntil
}

// changedLocked numbers a change to a and returns what saves it, to call
// once mu is released.
func (l *Lockout) changedLocked(key string, a *attempts) func() {
	if l.save == nil {
		return func() {}
	}
	l.changes++
	n := l.changes
	failures, until := append([]time.Time(nil), a.failures...), a.lockedUntil
	return func() {
		l.saveMu.Lock()
		defer l.saveMu.Unlock()
		if n <= l.saved {
			return
		}
		l.saved = n
		l.save(key, failures, until)
	}
}

type attempts struct {
	failures    []time.Time // within the window, oldest first
	pending     int         // begun, not yet finished
	lockedUntil time.Time
}

// NewLockout locks a key for lockFor after max failures within window.
func NewLockout(max int, window, lockFor time.Duration) *Lockout {
	return &Lockout{max: max, window: window, lockFor: lockFor, now: time.Now, keys: map[string]*attempts{}}
}

// Locked returns how long key stays locked; 0 if it isn't. Use it, with
// Begin and Fail only on failure, where the secret can't be guessed anyway
// (agent tokens) and counting attempts in flight would only get in the
// way of busy clients.
func (l *Lockout) Locked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.keys[key]; a != nil {
		if d := a.lockedUntil.Sub(l.now()); d > 0 {
			return d
		}
	}
	return 0
}

// Attempt is one try against a Lockout; finish it with exactly one of
// Fail, Succeed or Cancel.
type Attempt struct {
	l   *Lockout
	key string
}

// Begin starts an attempt, or refuses and says how long to wait: while
// the key is locked, or while recent failures plus attempts in flight
// already reach the limit.
func (l *Lockout) Begin(key string) (*Attempt, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.keys[key]
	if a == nil {
		a = &attempts{}
		l.keys[key] = a
	}
	if d := a.lockedUntil.Sub(now); d > 0 {
		return nil, d
	}
	a.failures = prune(a.failures, now.Add(-l.window))
	if len(a.failures)+a.pending >= l.max {
		return nil, time.Second
	}
	a.pending++
	l.gc(now)
	return &Attempt{l: l, key: key}, 0
}

// Fail records the attempt as failed and returns how many are left before
// a lockout; 0 means the key is now locked.
func (t *Attempt) Fail() (left int) {
	l := t.l
	l.mu.Lock()
	now := l.now()
	a := l.end(t.key)
	a.failures = append(prune(a.failures, now.Add(-l.window)), now)
	left = l.max - len(a.failures)
	if left <= 0 {
		a.lockedUntil = now.Add(l.lockFor)
		a.failures = nil
		left = 0
	}
	save := l.changedLocked(t.key, a)
	l.mu.Unlock()
	save()
	return left
}

// Succeed clears the key's failures.
func (t *Attempt) Succeed() {
	l := t.l
	l.mu.Lock()
	a := l.end(t.key)
	a.failures = nil
	save := l.changedLocked(t.key, a)
	l.mu.Unlock()
	save()
}

// Cancel ends the attempt without counting it, for when it never got to
// check anything (another limit refused it, say).
func (t *Attempt) Cancel() {
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	t.l.end(t.key)
}

func (l *Lockout) end(key string) *attempts {
	a := l.keys[key]
	if a == nil { // forgotten by gc while in flight; can't happen with pending > 0
		a = &attempts{}
		l.keys[key] = a
	}
	if a.pending > 0 {
		a.pending--
	}
	return a
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
		if a.pending == 0 && a.lockedUntil.Before(now) && len(prune(a.failures, now.Add(-l.window))) == 0 {
			delete(l.keys, k)
		}
	}
}
