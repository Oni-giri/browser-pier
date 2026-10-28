package sessions

import (
	"errors"
	"sync"
)

// ErrProfileHeld is returned while a profile's folder is being exported
// or imported: its browser can't start, nor the profile be deleted.
var ErrProfileHeld = errors.New("the profile is being exported or imported; try again when that's done")

// Hold keeps a stopped profile's browser from starting, and the profile
// from being deleted, until release is called: its folder is being read
// or written whole. The profile need not exist yet (an import).
func (m *Manager) Hold(profile string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.live[profile]; ok {
		return nil, ErrProfileBusy
	}
	if m.held[profile] {
		return nil, ErrProfileHeld
	}
	m.held[profile] = true
	return sync.OnceFunc(func() {
		m.mu.Lock()
		delete(m.held, profile)
		m.mu.Unlock()
	}), nil
}
