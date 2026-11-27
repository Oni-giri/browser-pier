package sessions

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Errors a page lease's context ends with (see context.Cause), saying why
// BrowserPier's own call into the page was cut short.
var (
	// ErrHandedOver: a person took the browser (a handoff opened).
	ErrHandedOver = errors.New("a person has taken over this browser for a handoff; wait for it to finish")
	// ErrStopping: the browser is stopping.
	ErrStopping = errors.New("the browser is stopping")
	// ErrTookControl: the person in the live view took control (an agent
	// input, or a page call marked Breakable, that had run for
	// takeOverAfter or more).
	ErrTookControl = errors.New("a person took control of this browser in the live view; wait until they hand it back")
	// ErrRevoked: the caller's token was revoked.
	ErrRevoked = errors.New("the agent token was revoked")
	// ErrPersonHadIt: a person had the browser (a handoff or Take
	// control) since the read an agent's action was aimed by
	// (AgentInputAt).
	ErrPersonHadIt = errors.New("a person had this browser since the element was read; its refs are stale: take a new visual_snapshot")
	// ErrSessionHeld: something holds the session (HoldSession) and
	// agents' page calls wait until it's done.
	ErrSessionHeld = errors.New("BrowserPier is busy with this browser; try again shortly")
)

// lease is one call of BrowserPier's page tools into a session's browser,
// or an agent's input action (AgentInput).
type lease struct {
	// tokenID is the agent token the call came with.
	tokenID string
	cancel  context.CancelCauseFunc
	// started is when an agent input action (AgentInput) or a page call
	// (PageLease) began.
	started time.Time
	// breakable: a page call the person in the live view may break off
	// once it has run for takeOverAfter (Lease.Breakable).
	breakable bool
	// yields: a read (ReadLease) that the person in the live view breaks
	// off at once, and that doesn't count as the agent driving.
	yields bool
	// call is the agent call holding the lease (CallFrom), if known.
	call *Call
}

// Lease is a call into a session's page, held from PageLease until
// Release. Ctx ends, with a cause from the errors above, when the call
// must stop: a handoff, a hold, a stop or the token's revocation.
type Lease struct {
	Ctx context.Context
	// Generation changes each time a person takes the browser (a handoff
	// or the live view's Take control): whatever a call learnt about the
	// page under an older generation is out of date.
	Generation uint64
	release    func()
	breakable  func()
}

// Release ends the lease; it is safe to call more than once.
func (l *Lease) Release() { l.release() }

// Breakable lets the person in the live view take control by breaking
// this call off once it has run for takeOverAfter, counted from when the
// lease was taken, as for an agent input action (AgentInput): Ctx then
// ends with ErrTookControl. Before that, and for a lease never marked,
// Take control is refused. act marks its lease, since its human-paced
// steps can run for minutes (PROJ-113); the short reads (snapshot, find,
// page content…) don't, as their own timeouts bound them.
func (l *Lease) Breakable() { l.breakable() }

// PageLease starts one of BrowserPier's own calls into a running
// session's page (the API-mode page tools). Leases are not connections
// (l.conns): they don't show the agent as connected, and they let the
// browser go idle between calls. But like an agent's input, they keep a
// person from taking control while one is held and for a few seconds
// after (TakeControl), unless marked Breakable and held for
// takeOverAfter, and a handoff, a hold, a stop or the token's
// revocation cancels them and waits for them to end.
//
// Calls on one session take turns: PageLease waits (until ctx ends) while
// another lease is held, so two calls never interleave.
func (m *Manager) PageLease(ctx context.Context, id, tokenID string) (*Lease, error) {
	return m.pageLease(ctx, id, tokenID, false)
}

// ReadLease is PageLease for a read that is not the agent driving:
// Visual mode's element tree (PROJ-150). It takes turns with page calls
// and is cancelled by a handoff, a hold, a stop or the token's
// revocation as they are, but it doesn't keep a person out: it doesn't
// start or extend the few seconds after an agent's input in which Take
// control is refused (PROJ-134), and Take control breaks it off at once.
func (m *Manager) ReadLease(ctx context.Context, id, tokenID string) (*Lease, error) {
	return m.pageLease(ctx, id, tokenID, true)
}

func (m *Manager) pageLease(ctx context.Context, id, tokenID string, read bool) (*Lease, error) {
	m.mu.Lock()
	l := m.findLocked(id)
	if err := leaseRefusal(l); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if l.turn == nil {
		l.turn = make(chan struct{}, 1)
	}
	turn := l.turn
	// A call waiting for its turn is already its token's: revoking the
	// token (DisconnectToken) ends the wait, as it ends a lease.
	cctx, cancel := context.WithCancelCause(ctx)
	waiting := &lease{tokenID: tokenID, cancel: cancel}
	if l.queued == nil {
		l.queued = map[*lease]struct{}{}
	}
	l.queued[waiting] = struct{}{}
	m.mu.Unlock()

	// One call at a time per session.
	select {
	case turn <- struct{}{}:
	case <-cctx.Done():
		m.mu.Lock()
		delete(l.queued, waiting)
		m.mu.Unlock()
		cancel(context.Canceled)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.Cause(cctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(l.queued, waiting)
	err := leaseRefusal(l)
	if err == nil && m.findLocked(id) != l {
		err = ErrNotRunning
	}
	if err == nil && cctx.Err() != nil {
		// Revoked, or the caller gone, as the turn came.
		if err = ctx.Err(); err == nil {
			err = context.Cause(cctx)
		}
	}
	if err != nil {
		<-turn
		cancel(context.Canceled)
		return nil, err
	}
	now := m.now()
	ls := &lease{tokenID: tokenID, cancel: cancel, started: now, yields: read}
	if !read {
		ls.call = CallFrom(ctx)
	}
	if ls.call != nil {
		// Granted: the call is admitted (its receipt stays, PROJ-156).
		// A read never admits one: a click by ref reads the element
		// first, and its call is admitted only by its input
		// (AgentInputAt), so a click refused after the read gives its
		// receipt back.
		ls.call.admitLocked()
	}
	if l.leases == nil {
		l.leases = map[*lease]struct{}{}
	}
	l.leases[ls] = struct{}{}
	l.lastActivity = now
	if !read {
		l.lastAgentInput = now
	}
	m.agentActedLocked(l)
	var once sync.Once
	return &Lease{Ctx: cctx, Generation: l.generation, breakable: func() {
		m.mu.Lock()
		ls.breakable = true
		m.mu.Unlock()
	}, release: func() {
		once.Do(func() {
			// Ended before it let go: a handoff, a hold, a stop, the
			// token's revocation or the caller going away cut it short.
			// (A cut that comes after the browser answered, just before this,
			// still counts: the receipt then errs on the safe side.)
			cut := cctx.Err() != nil
			cancel(context.Canceled)
			m.mu.Lock()
			if cut && ls.call != nil {
				ls.call.cut = true
			}
			delete(l.leases, ls)
			now := m.now()
			l.lastActivity = now
			if !read {
				l.lastAgentInput = now
			}
			m.agentActedLocked(l)
			m.inputsDone.Broadcast()
			m.mu.Unlock()
			<-turn
		})
	}}, nil
}

// leaseRefusal says why no page call may start now, if one may not.
// m.mu must be held.
func leaseRefusal(l *live) error {
	switch {
	case l == nil:
		return ErrNotRunning
	case l.sess.State == NeedsHelp:
		return ErrHandedOver
	case l.sess.State != Running:
		return ErrNotRunning
	case l.human:
		return ErrHumanDriving
	case l.holds > 0:
		return ErrSessionHeld
	}
	return nil
}

// cancelLeasesLocked cancels every page call under way, for why.
func (l *live) cancelLeasesLocked(why error) {
	for ls := range l.leases {
		ls.cancel(why)
	}
}

// cancelAllLocked cancels every page call and agent input action under
// way, for why: the agent loses the browser (a handoff or a stop).
func (l *live) cancelAllLocked(why error) {
	l.cancelLeasesLocked(why)
	for in := range l.inputs {
		in.cancel(why)
	}
}

// waitLeasesLocked waits, with m.mu held (Wait lets go of it meanwhile),
// until no page call or agent input is under way.
func (m *Manager) waitLeasesLocked(l *live) {
	for len(l.inputs) > 0 || len(l.leases) > 0 {
		m.inputsDone.Wait()
	}
}

// HoldSession keeps BrowserPier's page calls (PageLease) off a running
// session until release is called: calls under way are cancelled and
// waited for, and new ones are refused with ErrSessionHeld. It is for
// work that needs the page to itself, such as a check that must see
// nothing else attached (PROJ-57). Holds nest.
func (m *Manager) HoldSession(id string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.sess.State.Live() {
		return nil, ErrNotRunning
	}
	l.holds++
	l.cancelLeasesLocked(ErrSessionHeld)
	for len(l.leases) > 0 {
		m.inputsDone.Wait()
	}
	return sync.OnceFunc(func() {
		m.mu.Lock()
		l.holds--
		m.mu.Unlock()
	}), nil
}
