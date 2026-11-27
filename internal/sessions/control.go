package sessions

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
// extends nor cuts short those seconds. Meanwhile agents can only watch:
// their input and new CDP connections are refused. During a handoff
// (Needs help) the agent is paused, so it always succeeds. If the
// browser begins to stop while it waits, it gives up with ErrNotRunning,
// leaving the browser to agents should the stop fail (PROJ-200).
//
// holder names who takes it, opaque here: the live view's viewer passes
// the admin's (AdminHolder) or the handoff link's (PROJ-184), and what only
// the person in control may do (HumanInput, ControlledBy) checks it. An
// empty holder can't take control (ErrNoHolder).
func (m *Manager) TakeControl(id, holder string) error {
	_, err := m.TakeControlTurn(id, holder)
	return err
}

// TakeControlTurn is TakeControl, and returns this turn's number: the
// takeover's generation. The person's input (HumanInputTurn), HoldsTurn
// and their own hand-back (ReleaseTurn) name it, so once their control
// has ended some other way and someone else has taken the browser,
// nothing they still send reaches it, and the browser itself refuses
// what of it is still on its way (TakeoverHeader, PROJ-196).
func (m *Manager) TakeControlTurn(id, holder string) (uint64, error) {
	return m.takeControlTurn(id, holder, tookControl, false, 0)
}

// ErrHandoffEnded: the handoff a Take control was for has ended.
var ErrHandoffEnded = errors.New("the handoff has ended")

// HelpEpoch says which time the session went to Needs help, while it
// waits for help: a handoff's live view reads it while its handoff is
// open, and takes control with it (TakeControlTurnByLink).
func (m *Manager) HelpEpoch(id string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || l.sess.State != NeedsHelp {
		return 0, false
	}
	return l.sess.helpEpoch, true
}

// WaitsForHelp reports whether session id still waits for the help it
// asked for as epoch: in Needs help with that epoch, or stopping from it
// (a stop that fails puts it back).
func (m *Manager) WaitsForHelp(id string, epoch uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if l == nil || epoch == 0 || l.sess.helpEpoch != epoch {
		return false
	}
	return l.sess.State == NeedsHelp || (l.sess.State == Stopping && l.stop != nil && l.stopPrev == NeedsHelp)
}

// TakeControlTurnByLink is TakeControlTurn for the person a handoff link
// gave the browser to (webrtc.OfferOptions.Control); the log says so
// (PROJ-158). It takes control only while the session still waits for
// the help it asked for as epoch (HelpEpoch), checked in the same step as
// the take and again once whatever it waited for has ended; otherwise
// ErrHandoffEnded. A handoff's link that outlives its handoff, even by a
// moment, never takes the browser back from the agent it was handed back
// to (PROJ-196).
func (m *Manager) TakeControlTurnByLink(id, holder string, epoch uint64) (uint64, error) {
	return m.takeControlTurn(id, holder, tookControlByLink, true, epoch)
}

// takeControlTurn is TakeControlTurn, and for a handoff's link,
// TakeControlTurnByLink: no epoch, 0 included, lets a link's take through
// once its Needs help is over. entry is what the log says.
func (m *Manager) takeControlTurn(id, holder, entry string, byLink bool, epoch uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder == "" {
		return 0, ErrNoHolder
	}
	l := m.findLocked(id)
	if l == nil || (l.sess.State != Running && l.sess.State != NeedsHelp) {
		return 0, ErrNotRunning
	}
	handoffOver := func() bool {
		return byLink && (epoch == 0 || l.sess.State != NeedsHelp || l.sess.helpEpoch != epoch)
	}
	if handoffOver() {
		return 0, ErrHandoffEnded
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
	// The takeover's generation is saved before it is handed out, so a
	// restart always knows the highest one the browser may have seen:
	// it fences that, and numbers the next ones above it (PROJ-196).
	if err := saveTakeoverGen(context.Background(), m.db, l.sess.ID, l.generation+1); err != nil {
		return 0, fmt.Errorf("saving the takeover: %w", err)
	}
	l.generation++
	l.human, l.holder = true, holder
	// Recording stops here, under m.mu and before anything else: no
	// fragment the browser sends from now on is kept, and HumanInput,
	// which the live view needs before it forwards any input, can't see
	// human until it has.
	m.personTookLocked(l)
	l.humanGen = l.generation
	turn := l.humanGen
	l.takeCtx, l.takeCancel = context.WithCancel(l.opCtx)
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
		stops := l.stops
		m.waitLeasesLocked(l)
		if m.findLocked(id) != l || (l.sess.State != Running && l.sess.State != NeedsHelp) || l.stops != stops {
			// A stop began meanwhile, even one that failed and put the
			// browser back before this woke: the take gives up
			// (PROJ-200), as it would have had it woken first.
			m.untakeLocked(l, turn)
			return 0, ErrNotRunning
		}
		if handoffOver() {
			m.untakeLocked(l, turn)
			return 0, ErrHandoffEnded
		}
		if !l.human || l.humanGen != turn || l.releasing {
			// Handed back meanwhile (the profile page's Hand back, a
			// handoff's end), and perhaps taken by someone else since:
			// that ended this takeover before the person ever had it,
			// and its turn is never handed out.
			return 0, ErrControlEnded
		}
	}
	return turn, nil
}

// untakeLocked undoes a TakeControl that gave up, so that if the browser
// isn't stopped after all (the stop fails and runStop puts it back), it
// is the agents' again, with no control left set that no one holds
// (PROJ-200). It runs under m.mu, in the same step that decides to give
// up, and only if the control set is still that take's own (human, its
// turn, not ending): one already ended by a ReleaseControl meanwhile is
// that hand-back's to finish.
//
// The clipboard needn't be emptied: nothing the person did can be on it.
// The live view forwards input only once Take control has returned its
// turn, and HumanInput accepts only that turn; this take returns none.
//
// The generation stays as it is, never goes back: it was bumped as the
// agent's work under way was broken off, and a page call learning it
// changed only re-reads the page. Recording is asked again, as at a
// hand-back: it resumes at the agent's next action (the tail from before
// was dropped by personTookLocked, as for any takeover). runStop asks too
// as it puts the browser back. The log says control went back, as the
// take noted it.
func (m *Manager) untakeLocked(l *live, turn uint64) {
	if !l.human || l.humanGen != turn || l.releasing {
		return
	}
	l.human, l.holder = false, ""
	l.takeCancel()
	m.restartChoosers(l.sess.Profile)
	if m.live[l.sess.Profile] == l {
		l.noteLocked(m.now(), KindControl, controlHandedBack)
		m.regateLocked(l)
	}
}

// ErrNotInControl: the person's control ended (they handed back, or it
// was ended for them: a handoff ended, the profile page's Hand back), and
// whoever drives now, if anyone, took it afresh.
var ErrNotInControl = errors.New("you don't have control of this browser")

// HumanInput records that holder, the person in control, used the
// browser: what holder alone may do, whichever of their views took it (a
// file chooser's answer, internal/picker). It fails with ErrNotInControl
// unless holder holds control and it isn't ending, and with ErrNotRunning
// while the browser isn't running or waiting for help (it is stopping,
// say). turn is their takeover's: send it to the browser with what the
// input does (TakeoverHeader), which refuses it once the takeover has
// ended. Call done once the input has reached the browser: a hand-back
// waits for input under way, so none lands (text put on the clipboard,
// say) after it has emptied the clipboard.
//
// Do all of it with ctx, from admission on: it ends the moment the
// takeover's hand-back begins (or the session ends), so whatever the
// operation hasn't sent by then never reaches the browser. The hand-back
// lets agents in, and the session lets go of its profile, only once done
// is called.
func (m *Manager) HumanInput(id, holder string) (ctx context.Context, turn uint64, done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if err := humanRefusal(l, holder, 0); err != nil {
		return nil, 0, nil, err
	}
	ctx, done = m.humanInputLocked(l)
	return ctx, l.humanGen, done, nil
}

// HumanInputTurn is HumanInput for the live view that took turn (from
// TakeControlTurn): the admin's views share one holder, so a view whose
// turn ended can't send input into the next one's, or read its clipboard,
// even when the same admin took control again from another view.
func (m *Manager) HumanInputTurn(id, holder string, turn uint64) (ctx context.Context, done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	if err := humanRefusal(l, holder, turn); err != nil {
		return nil, nil, err
	}
	ctx, done = m.humanInputLocked(l)
	return ctx, done, nil
}

func (m *Manager) humanInputLocked(l *live) (context.Context, func()) {
	l.lastActivity = m.now()
	l.humanInputs++
	return l.takeCtx, sync.OnceFunc(func() {
		m.mu.Lock()
		l.humanInputs--
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	})
}

// humanRefusal says why holder, in their turn (any of theirs if 0),
// can't use the browser now, if they can't. m.mu must be held.
func humanRefusal(l *live, holder string, turn uint64) error {
	switch {
	case l == nil:
		return ErrNotRunning
	case !l.human || l.releasing || holder == "" || l.holder != holder, turn != 0 && l.humanGen != turn:
		return ErrNotInControl
	case l.sess.State != Running && l.sess.State != NeedsHelp, l.opCtx.Err() != nil:
		return ErrNotRunning
	}
	return nil
}

// HoldsTurn reports whether turn (from TakeControlTurn) holds the
// browser now, as HumanInputTurn would admit its input: not while it is
// ending, nor while the browser is stopping or its session over.
func (m *Manager) HoldsTurn(id string, turn uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	return l != nil && humanRefusal(l, l.holder, turn) == nil
}

// ReleaseControl hands the browser back to agents, whatever ends the
// person's control: they hand back, a handoff ends, the live view goes.
// The browser fences the takeover first (fenceTakeover), refusing from
// then on whatever of it is still on its way, and lets go of any key or
// mouse button the person still holds (a drag when a handoff expires);
// then, a moment later (clipboardSettle), its clipboard and PRIMARY
// selection are emptied (PROJ-194), so no agent can paste what the
// person put there, copied or selected in the page; only then are agents
// let in. If the clipboard can't be emptied, that is logged and the
// browser handed back all the same. A browser that is stopping keeps its
// clipboard: the container takes it along, or, if the stop fails,
// runStop empties it before agents get the browser back. Recording
// (PROJ-23) stays off throughout, and the gate is asked again only as
// agents get the browser back. The log says so (PROJ-158).
//
// It returns once agents have the browser (or the session has ended: a
// stop ends a hand-back the browser never answers). Callers that must not
// wait longer than a request use HandBack.
//
// It ends whoever's control it is: it is for what ends a takeover from
// outside it (a handoff, the profile page's Hand back). The person's own
// live view hands back with ReleaseTurn.
func (m *Manager) ReleaseControl(id string) {
	_ = m.HandBack(context.Background(), id)
}

// HandBackWait bounds how long whoever explicitly ends a person's
// control waits for its hand-back (the live view's Hand back, a
// handoff's Done or withdrawal, the profile page's form, a reloaded
// handoff page waiting for the closed one's), so a browser that doesn't
// answer gets ErrHandingBack rather than a request that hangs. A
// hand-back that answers takes a few seconds at most: the fenced lift
// drains xdotool (2 s at most), then the settle (0.1 s) and emptying both
// selections (1.5 s each at most).
const HandBackWait = 30 * time.Second

// ErrHandingBack: the caller stopped waiting (its context ended) before
// the browser was handed back. The hand-back goes on to its end, agents
// kept out until then; ask again.
var ErrHandingBack = errors.New("the browser is still being handed back; ask again shortly")

// HandBack is ReleaseControl, waiting no longer than ctx: if ctx ends
// first it returns ErrHandingBack, and the hand-back goes on to its end
// without the caller (it runs tied to the session, so a caller giving up
// never leaves it half done).
func (m *Manager) HandBack(ctx context.Context, id string) error {
	m.mu.Lock()
	l := m.findLocked(id)
	if l == nil || !l.human {
		m.mu.Unlock()
		return nil
	}
	done := m.beginReleaseLocked(l, id)
	m.mu.Unlock()
	return waitHandedBack(ctx, done)
}

// waitHandedBack waits for a hand-back's done, or ctx: ErrHandingBack.
func waitHandedBack(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ErrHandingBack
	}
}

// ReleaseTurn is ReleaseControl for turn (from TakeControlTurn) alone:
// if that control has ended already, however, it does nothing and
// returns false, so a live view that missed the end of its control never
// ends, or lifts the keys or empties the clipboard of, the takeover of
// whoever took the browser since (PROJ-184, PROJ-196). The check and the
// start of the hand-back are one step under m.mu, and no one else can
// take control until it is over. It waits for it as HandBack does:
// ErrHandingBack if ctx ends first. ended is false if this turn's control
// had ended already, or was ending for it.
func (m *Manager) ReleaseTurn(ctx context.Context, id string, turn uint64) (ended bool, err error) {
	m.mu.Lock()
	l := m.findLocked(id)
	if l == nil || !l.human || l.humanGen != turn {
		m.mu.Unlock()
		return false, nil
	}
	ended = !l.releasing
	done := m.beginReleaseLocked(l, id)
	m.mu.Unlock()
	return ended, waitHandedBack(ctx, done)
}

// beginReleaseLocked starts ending l's person's control, unless that has
// started already, and returns a channel closed once it has ended. From
// here on human stays set, so no one else can take control, and agents
// stay out, until runRelease is done. m.mu must be held.
func (m *Manager) beginReleaseLocked(l *live, id string) <-chan struct{} {
	if l.releasing {
		return l.released
	}
	l.releasing = true
	l.releaseStarted = m.now()
	l.released = make(chan struct{})
	// What the person's live view still has under way breaks off now, on
	// this side: it returns at once, and can't reach the browser any
	// more.
	l.takeCancel()
	done, gen := l.released, l.humanGen
	if !m.spawn(func() { m.runRelease(l, id, gen) }) {
		// Closing: the next BrowserPier fences this takeover as it picks
		// the browser up (Reconcile).
		m.endReleaseLocked(l, id)
	}
	return done
}

// runRelease is the hand-back beginReleaseLocked started, for takeover
// gen.
func (m *Manager) runRelease(l *live, id string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	profile := l.sess.Profile
	// The browser fences the takeover, so whatever of it is still on its
	// way there, queued there (in xdotool) or held up in there is
	// refused or has run, and lets go of whatever key or button the
	// person still holds, so whoever drives next doesn't inherit a
	// pressed one. Tried until the browser answers or the session ends:
	// agents wait meanwhile, and a session whose browser doesn't answer
	// can still be stopped, idle or not (idleLocked), which ends this.
	// While the browser runs, waits for help or is stopping (a stop may
	// fail); not once the session is over.
	fenced := false
	if l.sess.State == Running || l.sess.State == NeedsHelp || l.sess.State == Stopping {
		m.unlockedOpLocked(l, func(ctx context.Context) {
			_, fenced = m.fenceTakeover(ctx, id, profile, gen)
		})
	}
	// The person's operations under way return, cancelled or refused.
	// One that doesn't (a bug) can do nothing to the browser once it has
	// fenced the takeover, so the hand-back goes on after inputWait; it
	// only waits for it if the browser didn't fence it, which happens
	// only as the session ends.
	busy := func() bool { return l.humanInputs > 0 && m.findLocked(id) == l && m.ctx.Err() == nil }
	if !m.waitBoundedLocked(inputWait, busy) {
		if fenced {
			m.log.Warn("a person's input was still under way "+inputWait.String()+" after their control began to end; the browser refuses it, so the hand-back goes on", "session", id)
		} else {
			for busy() {
				m.inputsDone.Wait()
			}
		}
	}
	switch l.sess.State {
	case Running, NeedsHelp:
		if fenced {
			m.unlockedOpLocked(l, func(ctx context.Context) {
				// The person's last keys have reached the browser, but
				// Chromium takes the selection a moment after a Ctrl+C:
				// let it land, so it is emptied too.
				if settle(ctx) {
					m.emptyClipboard(ctx, id, profile, gen)
				}
			})
		}
	case Stopping:
		// The container takes the clipboard along; if the stop fails
		// instead, runStop empties it before agents are let back in.
		l.clipboardLeft, l.clipboardLeftGen = true, gen
	}
	m.endReleaseLocked(l, id)
}

// endReleaseLocked lets agents back in as a hand-back ends.
func (m *Manager) endReleaseLocked(l *live, id string) {
	l.human, l.holder, l.releasing = false, "", false
	l.lastActivity = m.now()
	m.restartChoosers(l.sess.Profile)
	// Only now, the clipboard emptied and agents let back in, may the
	// gate record again (PROJ-23): never while the person's control is
	// still ending. A session stopped meanwhile has nothing to record.
	// The log says so (PROJ-158).
	if m.findLocked(id) == l {
		l.noteLocked(l.lastActivity, KindControl, controlHandedBack)
		m.regateLocked(l)
	}
	close(l.released)
	m.inputsDone.Broadcast()
}

// clipboardSettle is how long a hand-back waits, after the person's last
// input has reached the browser, before it empties the clipboard.
const clipboardSettle = 100 * time.Millisecond

// settle waits clipboardSettle, and reports false if ctx ended first.
func settle(ctx context.Context) bool {
	t := time.NewTimer(clipboardSettle)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// inputWait is how long a hand-back waits for the person's input under
// way, already cancelled and fenced: a cancelled call returns at once,
// so only one that never says done (a bug) makes it wait this long.
// Tests shorten it.
var inputWait = 30 * time.Second

// emptyClipboard empties a running browser's clipboard as takeover gen
// ends (PROJ-194): the browser does nothing if a later takeover has begun
// meanwhile, whose clipboard it would be (a call held up past its time,
// PROJ-196). A failure is logged without what the clipboard held, which
// BrowserPier never sees here; not when the session ended meanwhile
// (ctx), its container taking the clipboard along.
func (m *Manager) emptyClipboard(ctx context.Context, sessionID, profile string, gen uint64) {
	hdr := map[string]string{FenceHeader: strconv.FormatUint(gen, 10)}
	if err := m.controlWith(ctx, profile, http.MethodDelete, "/input/clipboard", hdr, nil, nil); err != nil && ctx.Err() == nil {
		m.log.Warn("couldn't empty the browser's clipboard as a person's control ended", "session", sessionID, "failure", controlFailure(err))
	}
}

// TakeoverHeader names, on each operation a person's live view sends the
// browser, the takeover it is for (its generation); FenceHeader, on the
// lift as a takeover ends, the takeover to fence: the browser refuses,
// from then on, anything from it or an earlier one, however long it was
// held up on its way, and has run what it queued of it. On the
// clipboard's emptying it names the takeover it is for, which the
// browser skips if a later one has begun (images/chromium/entrypoint/
// fence.go, PROJ-196).
const (
	TakeoverHeader = "X-BrowserPier-Takeover"
	FenceHeader    = "X-BrowserPier-Fence"
)

// fenceTakeover has the browser fence takeover want and let go of every
// key and button it, or an earlier one, still holds (a key left down
// would be the next driver's: a modifier, a drag). Only a 200 answer
// giving a fence at or above want counts: a browser that answers anything
// else (204, an empty or lower fence: an entrypoint from before the
// fence) hasn't fenced it. It tries until the browser does or ctx ends,
// waiting a little longer each time, and returns the fence answered. A
// failure is logged by its kind alone (controlFailure).
func (m *Manager) fenceTakeover(ctx context.Context, sessionID, profile string, want uint64) (fence uint64, ok bool) {
	wait := liftRetry
	hdr := map[string]string{FenceHeader: strconv.FormatUint(want, 10)}
	for try := 1; ; try++ {
		var out struct {
			Fence *uint64 `json:"fence"`
		}
		err := m.controlWith(ctx, profile, http.MethodPost, "/input/release", hdr, nil, &out)
		if err == nil {
			if out.Fence != nil && *out.Fence >= want {
				return *out.Fence, true
			}
			err = errNotFenced
		}
		if ctx.Err() != nil {
			return 0, false
		}
		if try == 1 || try%20 == 0 {
			m.log.Warn("the browser didn't fence and let go of a person's control as it ended; trying again, agents waiting", "session", sessionID, "tries", try, "failure", controlFailure(err))
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return 0, false
		}
		wait = min(2*wait, maxLiftRetry)
	}
}

// errNotFenced: the browser answered a fenced lift without the fence.
var errNotFenced = errors.New("the browser answered without fencing the takeover")

// liftRetry is how long fenceTakeover first waits before trying again;
// it doubles each time, up to maxLiftRetry.
const (
	liftRetry    = 100 * time.Millisecond
	maxLiftRetry = 5 * time.Second
)

// controlFailure describes a failed call on a browser's control socket
// for the log: the answer's status code, or what kind of failure it was,
// never the words of the answer. The container isn't trusted, and its
// words could carry what a clipboard held.
func controlFailure(err error) string {
	var ce *controlError
	switch {
	case errors.Is(err, errNotFenced):
		return "no fence in its answer"
	case errors.As(err, &ce):
		return "status " + strconv.Itoa(ce.status)
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer in time"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "the browser's control socket couldn't be reached"
	}
	return "the browser's answer couldn't be read"
}

// sockOpLocked admits one of BrowserPier's own calls on l's control
// socket that runs without m.mu (a hand-back's lift or clipboard
// emptying), for l's browser alone: it returns the context to make it
// with, and false once the session is ending. Call sockOpDoneLocked, with
// m.mu, once it is over: end waits for that before the profile, and its
// socket, can have a new browser.
func (m *Manager) sockOpLocked(l *live) (context.Context, bool) {
	if l.opCtx.Err() != nil {
		return nil, false
	}
	l.sockOps++
	return l.opCtx, true
}

func (m *Manager) sockOpDoneLocked(l *live) {
	l.sockOps--
	m.inputsDone.Broadcast()
}

// unlockedOpLocked runs f, a call on l's control socket, as sockOpLocked
// admits it, with m.mu let go meanwhile and held again after, panic or
// not. It reports false if the session is ending and f didn't run.
func (m *Manager) unlockedOpLocked(l *live, f func(ctx context.Context)) bool {
	ctx, ok := m.sockOpLocked(l)
	if !ok {
		return false
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.sockOpDoneLocked(l)
	}()
	f(ctx)
	return true
}

// waitBoundedLocked waits, with m.mu held (let go of meanwhile), while
// busy says so, for at most bound. It reports false if bound passed
// first.
func (m *Manager) waitBoundedLocked(bound time.Duration, busy func() bool) bool {
	if !busy() {
		return true
	}
	expired := false
	t := time.AfterFunc(bound, func() {
		m.mu.Lock()
		expired = true
		m.inputsDone.Broadcast()
		m.mu.Unlock()
	})
	defer t.Stop()
	for busy() && !expired {
		m.inputsDone.Wait()
	}
	return !expired
}

// sockOpWait bounds how long a session's end waits for its calls on the
// control socket. Each has a deadline well within it (controlWait, the
// control helper's 5 s), and the session's end cancels them at once: it
// is only a backstop against a caller that never says done.
const sockOpWait = 30 * time.Second

// waitSockOpsLocked waits, with m.mu held, until no call admitted for
// l's browser on the control socket is under way: the person's input
// (HumanInput) and a hand-back's or runStop's own (sockOpLocked). l's
// session has ended, so no more are admitted.
func (m *Manager) waitSockOpsLocked(l *live) {
	if !m.waitBoundedLocked(sockOpWait, func() bool { return l.sockOps > 0 || l.humanInputs > 0 }) {
		m.log.Error("a session ended with calls to its browser still under way; its profile is let go all the same",
			"session", l.sess.ID, "calls", l.sockOps+l.humanInputs)
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

// controlledByLocked: holder holds control, and it isn't ending.
func (l *live) controlledByLocked(holder string) bool {
	return l.human && !l.releasing && holder != "" && l.holder == holder && (l.sess.State == Running || l.sess.State == NeedsHelp)
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
