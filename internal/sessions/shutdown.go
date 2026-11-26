package sessions

import (
	"context"
	"errors"
	"sync"
)

// StopAll stops every live browser, each through Stop so it ends as
// Stopped and closes gracefully, and refuses new ones from then on. It's
// for an orchestrator that takes its browsers down with it
// (BROWSERPIER_STOP_BROWSERS_ON_EXIT, as on Umbrel); call it before Close.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	m.draining = true
	ids := make([]string, 0, len(m.live))
	for _, l := range m.live {
		ids = append(ids, l.sess.ID)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Go(func() {
			_, errs[i] = m.StopFor(ctx, id, stopShutdown)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
