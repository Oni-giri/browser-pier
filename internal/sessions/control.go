package sessions

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// agentQuiet is how long after an agent's last click or keystroke a
// person can take control: within it, the agent counts as driving.
const agentQuiet = 5 * time.Second

// takeOverAfter is how long an agent's input action must have run before
// the person in the live view can take control by breaking it off: a
// click is over well within it, a long human-paced type isn't.
const takeOverAfter = 3 * time.Second

// AgentInput records an agent's click or keystroke. It fails while a
// person has control, and with ErrInputBusy while another is under way:
// one at a time per session, so an agent can't queue up minutes of input.
//
// It is a lease, held by the token tokenID: call done once the browser
// has answered. A handoff (Needs help), a stop or the token's revocation
// (DisconnectToken, with ErrRevoked) cancels the context it returns, so
// an action that takes a while (typing at human pace, PROJ-113) breaks
// off at once. A handoff or a stop also waits for leases to end before
// the person gets the browser, and refuses new ones; a revocation doesn't
// wait. The browser lets go of what the action held as it answers, so the
// person gets it at most tens of milliseconds after the break. A person can take control from the live view once one has run
// for takeOverAfter, which breaks it off the same way; not before, nor
// for agentQuiet after it ends.
func (m *Manager) AgentInput(ctx context.Context, id, tokenID string) (context.Context, func(), error) {
	return m.agentInput(ctx, id, tokenID, nil)
}

// AgentInputAt is AgentInput for an action aimed by what a read under
// lease generation gen learnt about the page (a click by a Visual-mode
// ref, whose point the element tree gave). It fails with ErrPersonHadIt
// if a person has had the browser since: what the read learnt is out of
// date, and the action must not land on the page they left.
func (m *Manager) AgentInputAt(ctx context.Context, id, tokenID string, gen uint64) (context.Context, func(), error) {
	return m.agentInput(ctx, id, tokenID, &gen)
}

func (m *Manager) agentInput(ctx context.Context, id, tokenID string, gen *uint64) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return nil, nil, ErrNotRunning
	}
	if l.human || l.sess.State == NeedsHelp {
		return nil, nil, ErrHumanDriving
	}
	if gen != nil && *gen != l.generation {
		return nil, nil, ErrPersonHadIt
	}
	if len(l.inputs) > 0 {
		return nil, nil, ErrInputBusy
	}
	if err := ctx.Err(); err != nil {
		// The caller went away before its turn: it never began, so it
		// isn't admitted.
		return nil, nil, err
	}
	now := m.now()
	l.lastActivity, l.lastAgentInput = now, now
	cctx, cancel := context.WithCancelCause(ctx)
	// tokenID: DisconnectToken cancels it with ErrRevoked.
	in := &lease{cancel: cancel, started: now, call: CallFrom(ctx), tokenID: tokenID}
	if in.call != nil {
		// Granted: the call is admitted (its receipt stays, PROJ-156).
		in.call.admitLocked()
	}
	if l.inputs == nil {
		l.inputs = map[*lease]struct{}{}
	}
	l.inputs[in] = struct{}{}
	m.agentActedLocked(l)
	return cctx, sync.OnceFunc(func() {
		// Ended before done: a handoff, Take control, a stop or the
		// caller going away cut it short.
		// (A cut that comes after the browser answered, just before this,
		// still counts: the receipt then errs on the safe side.)
		cut := cctx.Err() != nil
		cancel(context.Canceled)
		m.mu.Lock()
		if cut && in.call != nil {
			in.call.cut = true
		}
		delete(l.inputs, in)
		// The agent counts as driving for agentQuiet after its last
		// event, not after the action began.
		now := m.now()
		l.lastActivity, l.lastAgentInput = now, now
		m.agentActedLocked(l)
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	}), nil
}

// TakeControl gives a person in the live view the mouse and keyboard.
// It is refused while an agent is driving: connected over CDP, having
// clicked or typed in the last few seconds, with a page call (PageLease)
// under way or just ended, or with an input action, or a page call marked
// Breakable (act), under way for less than takeOverAfter. One that has
// run longer (a long human-paced type, or act's steps) is broken off, as
// a handoff breaks it off, and TakeControl waits for it to end. A page
// call not marked Breakable (the page tools' reads, bounded by their own
// timeouts) is never broken off: Take control is refused while it runs.
// A read lease (ReadLease, Visual mode's tree) is broken off at once, but
// only once the agent's quiet time is over: a read neither starts,
// extends nor cuts short those seconds. Meanwhile agents can only watch: their input and new CDP
// connections are refused. During a handoff (Needs help) the agent is
// paused, so it always succeeds.
//
// holder names who takes it, opaque here: the live view's viewer passes
// the admin's (AdminHolder) or the handoff link's (PROJ-184), and what only
// the person in control may do (HumanInput, ControlledBy) checks it. An
// empty holder can't take control (ErrNoHolder).
func (m *Manager) TakeControl(id, holder string) error {
	_, err := m.TakeControlTurn(id, holder)
	return err
}

// TakeControlTurn is TakeControl, and returns this turn's number, for
// HoldsTurn and ReleaseTurn: a live view releases only its own turn.
func (m *Manager) TakeControlTurn(id, holder string) (uint64, error) {
	return m.takeControlTurn(id, holder, tookControl)
}

// TakeControlByLink is TakeControl for the person a handoff link gave
// the browser to (webrtc.OfferOptions.Control); the log says so
// (PROJ-158).
func (m *Manager) TakeControlByLink(id, holder string) error {
	_, err := m.TakeControlTurnByLink(id, holder)
	return err
}

// TakeControlTurnByLink is TakeControlTurn for a handoff link's holder.
func (m *Manager) TakeControlTurnByLink(id, holder string) (uint64, error) {
	return m.takeControlTurn(id, holder, tookControlByLink)
}

// takeControlTurn is TakeControlTurn; entry is what the log says.
func (m *Manager) takeControlTurn(id, holder, entry string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder == "" {
		return 0, ErrNoHolder
	}
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return 0, ErrNotRunning
	}
	now := m.now()
	breakOff, breakReads := false, false
	if l.sess.State == Running {
		if len(l.conns) > 0 {
			return 0, ErrAgentDriving
		}
		for ls := range l.leases {
			if ls.yields {
				breakReads = true // a read: the person goes first, after the quiet time
				continue
			}
			if !ls.breakable || now.Sub(ls.started) < takeOverAfter {
				return 0, ErrAgentDriving
			}
			breakOff = true
		}
		for in := range l.inputs {
			if now.Sub(in.started) < takeOverAfter {
				return 0, ErrAgentDriving
			}
			breakOff = true
		}
		if !breakOff && now.Sub(l.lastAgentInput) < agentQuiet {
			return 0, ErrAgentDriving
		}
	}
	if l.human {
		return 0, ErrHumanDriving // one person at a time
	}
	l.human, l.holder = true, holder
	// Recording stops here, under m.mu and before anything else: no
	// fragment the browser sends from now on is kept, and HumanInput,
	// which the live view needs before it forwards any input, can't see
	// human until it has.
	m.personTookLocked(l)
	l.controlTurn++
	turn := l.controlTurn
	l.generation++
	l.lastActivity = now
	m.restartChoosers(l.sess.Profile)
	l.noteLocked(now, KindControl, entry)
	if breakOff || breakReads {
		// New agent input and page calls are refused from now on
		// (l.human); what is under way breaks off. Every lease left here
		// is a read, or Breakable and old enough, or TakeControl was
		// refused above.
		for in := range l.inputs {
			in.cancel(ErrTookControl)
		}
		l.cancelLeasesLocked(ErrTookControl)
	}
	// What is still under way (that one, or during a handoff what the
	// handoff cancelled and is itself waiting for) ends before the person
	// gets the browser.
	if len(l.inputs) > 0 || len(l.leases) > 0 {
		m.waitLeasesLocked(l)
		if m.findLocked(id) != l || (l.sess.State != Running && l.sess.State != NeedsHelp) {
			return 0, ErrNotRunning // stopped, or stopping, meanwhile
		}
		if !l.human || l.controlTurn != turn || l.releasing {
			// Handed back meanwhile (the profile page's Hand back, a
			// handoff's end), and perhaps taken by someone else since.
			return 0, ErrControlEnded
		}
	}
	return turn, nil
}

// HumanInput records that the person in control used the browser. It
// fails unless holder is the one holding control. Call done once the
// input has reached the browser: ReleaseControl waits for input under
// way, so none lands (text put on the clipboard, say) after it has
// emptied the clipboard.
func (m *Manager) HumanInput(id, holder string) (done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || l.releasing || !l.controlledByLocked(holder) {
		return nil, ErrNotRunning
	}
	return m.humanInputLocked(l), nil
}

// HumanInputTurn is HumanInput for the live view that took turn (from
// TakeControlTurn): the admin's views share one holder, so a view whose
// turn ended can't send input into the next one's, or read its clipboard,
// even when the same admin took control again from another view.
func (m *Manager) HumanInputTurn(id, holder string, turn uint64) (done func(), err error) {
	_, done, err = m.HumanInputTurnContext(id, holder, turn)
	return done, err
}

// HumanInputTurnContext is HumanInputTurn with a context that ends as the
// person's control starts to end (a hand-back, a handoff's end, the live
// view going), so a long input under way breaks off instead of holding
// the hand-back up (PROJ-195).
func (m *Manager) HumanInputTurnContext(id, holder string, turn uint64) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || l.releasing || l.controlTurn != turn || !l.controlledByLocked(holder) {
		return nil, nil, ErrNotRunning
	}
	done := m.humanInputLocked(l)
	if l.humanCancel == nil {
		l.humanCtx, l.humanCancel = context.WithCancel(m.ctx)
	}
	return l.humanCtx, done, nil
}

func (m *Manager) humanInputLocked(l *live) func() {
	l.lastActivity = m.now()
	l.humanInputs++
	return sync.OnceFunc(func() {
		m.mu.Lock()
		l.humanInputs--
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	})
}

// ReleaseControl hands the browser back to agents, whoever held it: a
// handoff's end, or the profile page's Hand back (releaseLocked). It
// returns once agents have the browser.
func (m *Manager) ReleaseControl(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.findLocked(id); l != nil && l.human {
		m.releaseLocked(id, l)
	}
}

// ReleaseTurn hands the browser back to agents if turn (from
// TakeControlTurn) still has it, and reports whether it did. A live view
// whose control ended behind its back (a handoff finished, Hand back from
// another page) releases nothing of whoever took it next.
func (m *Manager) ReleaseTurn(id string, turn uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.human || l.controlTurn != turn {
		return false
	}
	m.releaseLocked(id, l)
	return true
}

// HoldsTurn reports whether turn (from TakeControlTurn) still has control.
func (m *Manager) HoldsTurn(id string, turn uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	return l != nil && l.human && !l.releasing && l.controlTurn == turn && (l.sess.State == Running || l.sess.State == NeedsHelp)
}

// releaseLocked ends a person's control, whatever ends it: they hand
// back, a handoff ends, the live view goes. First it lets go of any key
// or button the live view still holds, then it empties the browser's
// clipboard and PRIMARY selection (PROJ-194), so no agent can
// paste what the person put there, copied or selected in the page: it
// waits for their input under way and a moment more (clipboardSettle),
// refuses more, and only then lets agents in. If the clipboard can't be
// emptied, that is logged and the browser handed back all the same. A
// browser that is stopping keeps its clipboard: the container takes it
// along, or, if the stop fails, runStop empties it before agents get the
// browser back. Recording (PROJ-23) stays off throughout, and the gate is
// asked again only as agents get the browser back. It drops m.mu while
// it empties the clipboard, and returns once agents have the browser. The
// log says so (PROJ-158).
func (m *Manager) releaseLocked(id string, l *live) {
	if l.releasing {
		// Another hand-back is emptying the clipboard: done when it is,
		// not when a turn taken after it ends.
		turn := l.controlTurn
		for m.findLocked(id) == l && l.human && l.controlTurn == turn {
			m.inputsDone.Wait()
		}
		return
	}
	l.releasing = true
	if l.humanCancel != nil {
		l.humanCancel()
		l.humanCtx, l.humanCancel = nil, nil
	}
	for l.humanInputs > 0 {
		m.inputsDone.Wait()
	}
	switch l.sess.State {
	case Running, NeedsHelp:
		profile := l.sess.Profile
		m.mu.Unlock()
		// Their input has all reached the browser: what it still holds
		// goes up, here rather than in the live view, so a view whose
		// control ended behind its back leaves nothing held for agents.
		m.liftLiveInput(id, profile)
		// The person's last keys have reached the browser, but Chromium
		// takes the selection a moment after a Ctrl+C: let it land, so it
		// is emptied too.
		time.Sleep(clipboardSettle)
		m.emptyClipboard(id, profile)
		m.mu.Lock()
	case Stopping:
		// The container takes the clipboard along; if the stop fails
		// instead, runStop empties it before agents are let back in.
		l.clipboardLeft = true
	}
	l.human, l.holder, l.releasing = false, "", false
	l.lastActivity = m.now()
	m.restartChoosers(l.sess.Profile)
	// Only now, the clipboard emptied and agents let back in, may the
	// gate record again (PROJ-23): never while the person's control is
	// still ending. A session stopped meanwhile has nothing to record.
	if m.findLocked(id) == l {
		l.noteLocked(l.lastActivity, KindControl, controlHandedBack)
		m.regateLocked(l)
	}
	m.inputsDone.Broadcast()
}

// clipboardSettle is how long a hand-back waits, after the person's last
// input has reached the browser, before it empties the clipboard.
const clipboardSettle = 100 * time.Millisecond

// liftLiveInput lets go of any key or button the live view's input still
// holds in a running browser, a person's control ending.
func (m *Manager) liftLiveInput(sessionID, profile string) {
	// drain: answered once the releases have run, not only been queued
	// (PROJ-195), so an agent's next click can't meet a held Ctrl.
	if err := m.control(m.ctx, profile, http.MethodPost, "/input/release?drain=1", nil, nil); err != nil {
		m.log.Warn("couldn't let go of the live view's keys as a person's control ended", "session", sessionID, "err", err)
	}
}

// emptyClipboard empties a running browser's clipboard, a person's
// control ending (PROJ-194). A failure is logged without what the
// clipboard held, which BrowserPier never sees here.
func (m *Manager) emptyClipboard(sessionID, profile string) {
	if err := m.control(m.ctx, profile, http.MethodDelete, "/input/clipboard", nil, nil); err != nil {
		m.log.Warn("couldn't empty the browser's clipboard as a person's control ended", "session", sessionID, "err", err)
	}
}

// Watch records an open live view until release is called; a watched
// browser is never stopped as idle. The log says when the first view
// opens and when the last one goes, never who watches; a view that
// opens again within unwatchGrace of the last one going takes that
// entry back instead (a reload, a reconnection).
func (m *Manager) Watch(id string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || !l.sess.State.Live() {
		return nil, ErrNotRunning
	}
	l.viewers++
	if l.viewers == 1 {
		m.firstViewerLocked(l)
	}
	done := false
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !done {
			done = true
			l.viewers--
			l.lastActivity = m.now()
			if l.viewers == 0 && m.findLocked(id) == l {
				l.noteLocked(l.lastActivity, KindWatch, nobodyWatching)
				l.unwatchedAt = l.lastActivity
			}
		}
	}, nil
}

// firstViewerLocked notes that someone started watching, or takes back
// the "Nobody is watching" entry if the last view went within
// unwatchGrace.
func (m *Manager) firstViewerLocked(l *live) {
	now := m.now()
	gone := l.unwatchedAt
	l.unwatchedAt = time.Time{}
	if gone.IsZero() || now.Sub(gone) >= unwatchGrace {
		l.noteLocked(now, KindWatch, startedWatching)
		return
	}
	for i := len(l.activity) - 1; i >= 0; i-- {
		if a := l.activity[i]; a.Kind == KindWatch && a.What == nobodyWatching && a.At.Equal(gone) {
			l.removeLocked(i)
			return
		}
	}
}

// ControlledBy reports whether holder holds control of the session (the
// live view's Take control, or a handoff link's), without counting as
// their use of it.
func (m *Manager) ControlledBy(id, holder string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	return l != nil && l.controlledByLocked(holder)
}

func (l *live) controlledByLocked(holder string) bool {
	return l.human && holder != "" && l.holder == holder && (l.sess.State == Running || l.sess.State == NeedsHelp)
}

// AdminHolder is the holder of control for the admin's live view, in any
// of the admin's signed-in browsers: one account, so a browser signed in
// again while its live view stays open keeps answering its file
// choosers. Which view holds control is the turn's (TakeControlTurn).
const AdminHolder = "admin"

// HandoffHolder is the holder of control for a handoff link's holder.
func HandoffHolder(handoff string) string {
	if handoff == "" {
		return ""
	}
	return "handoff " + handoff
}
