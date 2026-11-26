package sessions

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"
)

// Call ids (PROJ-156). An agent may give an action that must not happen
// twice an id of its own; the session then keeps a receipt for it, so the
// same call sent again (a client's retry after a timeout) is answered with
// how the first one went instead of being run again.
//
// A receipt is taken before the call waits for anything, under the
// manager's lock, so two copies sent together make one call. It is
// admitted when the session grants the call its lease (AgentInput for a
// Visual-mode input, PageLease for a page tool): a call refused before
// that gives its receipt back, and the id can be sent again. Receipts hold
// the tool, a keyed fingerprint of the request, the token and the times,
// never the arguments or the answer; they are in memory only, per live
// session, and go when it ends.

const (
	// maxCallID is the longest call id.
	maxCallID = 64
	// maxReceipts is how many receipts a session keeps. When full, the
	// oldest ended one goes; one waiting or running never does.
	maxReceipts = 1000
)

var (
	// ErrOtherCall: the call id was taken by a request with another tool
	// or other arguments.
	ErrOtherCall = errors.New("this call id named another call; give this one its own")
	// ErrTooManyCalls: the session holds maxReceipts receipts, none of
	// them ended.
	ErrTooManyCalls = errors.New("this browser has 1000 calls with an id under way; wait for some to end")
)

// ValidCallID reports whether id is a call id: 1 to 64 ASCII letters,
// digits, '-' or '_'.
func ValidCallID(id string) bool {
	if id == "" || len(id) > maxCallID {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// CallState is how far a call with an id got.
type CallState string

const (
	// CallWaiting: taken, not yet admitted (waiting for its turn).
	CallWaiting CallState = "waiting"
	// CallRunning: admitted, under way.
	CallRunning CallState = "running"
	// CallOK: it ended and answered success.
	CallOK CallState = "ok"
	// CallError: it was admitted and answered with an error; some of it
	// may have happened.
	CallError CallState = "error"
	// CallInterrupted: it was cut short (a handoff, Take control, a stop,
	// a hold, the token's revocation, the caller going away or a panic);
	// how far it got isn't known.
	CallInterrupted CallState = "interrupted"
)

// Ended reports whether the state is final.
func (s CallState) Ended() bool { return s == CallOK || s == CallError || s == CallInterrupted }

// Fingerprint identifies a request without holding it: a keyed hash of
// its tool and body. The body can hold typed text, so a Fingerprint never
// prints or encodes as anything but a placeholder.
type Fingerprint struct{ sum [sha256.Size]byte }

func (Fingerprint) String() string               { return "[fingerprint]" }
func (Fingerprint) GoString() string             { return "[fingerprint]" }
func (Fingerprint) Format(s fmt.State, _ rune)   { _, _ = io.WriteString(s, "[fingerprint]") }
func (Fingerprint) LogValue() slog.Value         { return slog.StringValue("[fingerprint]") }
func (Fingerprint) MarshalText() ([]byte, error) { return []byte("[fingerprint]"), nil }

// equal compares in constant time.
func (f Fingerprint) equal(g Fingerprint) bool { return hmac.Equal(f.sum[:], g.sum[:]) }

// fingerprintKey is the fingerprints' HMAC key: random, per process, so a
// fingerprint can't be checked against a guessed body anywhere else.
var fingerprintKey = func() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k) // never fails (crypto/rand)
	return k
}()

// FingerprintOf is the fingerprint of a call to tool with body. A JSON
// body counts as canonical JSON (object keys sorted, whitespace dropped,
// numbers as written, so 1 and 1.0 differ); any other body as its bytes.
func FingerprintOf(tool string, body []byte) Fingerprint {
	mac := hmac.New(sha256.New, fingerprintKey)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tool)))
	mac.Write(n[:])
	mac.Write([]byte(tool))
	if c, ok := canonicalJSON(body); ok {
		mac.Write([]byte{'j'})
		mac.Write(c)
	} else {
		mac.Write([]byte{'b'})
		mac.Write(body)
	}
	var f Fingerprint
	mac.Sum(f.sum[:0])
	return f
}

// canonicalJSON re-encodes one JSON value: Go writes maps with their keys
// sorted, and json.Number as it was written. An empty body is "".
func canonicalJSON(body []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false // more after the value
	}
	out, err := json.Marshal(v)
	return out, err == nil
}

// callKey names a receipt: ids are per token, so two tokens using the
// same id never meet.
type callKey struct{ tokenID, id string }

// receipt is what a session keeps about a call with an id. Guarded by
// Manager.mu.
type receipt struct {
	tool    string
	fp      Fingerprint
	agent   string // the token's name
	state   CallState
	started time.Time
	ended   time.Time
	// endSeq orders ended receipts, for dropping the oldest.
	endSeq uint64
}

// Receipt is a call's receipt as callers see it.
type Receipt struct {
	CallID  string
	Tool    string
	Agent   string
	State   CallState
	Started time.Time
	Ended   time.Time // zero until it has ended
}

func (r *receipt) out(id string) Receipt {
	return Receipt{CallID: id, Tool: r.tool, Agent: r.agent, State: r.state, Started: r.started, Ended: r.ended}
}

// Call is one agent call that may take a lease on a session: who makes it,
// with what tool, and its receipt when it has an id. It travels to the
// lease in the request's context (WithCall), which is how a lease admits
// it, and the caller ends it with End once it has answered.
type Call struct {
	m       *Manager
	tokenID string
	agent   string
	tool    string
	id      string   // "" without a call id
	l       *live    // the session holding rec
	rec     *receipt // nil without a call id
	// Guarded by m.mu: a lease admitted it; a lease it held was cut
	// short; End was called.
	admitted, cut, ended bool
}

// TokenID, Agent, Tool and ID say who makes the call, with what, and its
// call id ("" for none).
func (c *Call) TokenID() string { return c.tokenID }
func (c *Call) Agent() string   { return c.agent }
func (c *Call) Tool() string    { return c.tool }
func (c *Call) ID() string      { return c.id }

// NewCall is a call without an id: it keeps no receipt, but its lease
// shows in RunningCalls and its activity entries name its token.
func (m *Manager) NewCall(tokenID, agent, tool string) *Call {
	return &Call{m: m, tokenID: tokenID, agent: agent, tool: tool}
}

// TakeCall takes the receipt for a call with an id on a live session, in
// one locked step. It returns:
//   - the new Call, when the id is new for this token;
//   - the earlier call's Receipt, when the same call (same tool and
//     fingerprint) was taken already: it must not run again;
//   - ErrOtherCall when the id was taken by another call, ErrTooManyCalls
//     when the session is full of calls under way, ErrNotRunning when the
//     session isn't live.
func (m *Manager) TakeCall(sessionID, tokenID, agent, tool, id string, fp Fingerprint) (*Call, *Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil || !l.sess.State.Live() {
		return nil, nil, ErrNotRunning
	}
	k := callKey{tokenID, id}
	if r, ok := l.calls[k]; ok {
		if r.tool != tool || !r.fp.equal(fp) {
			return nil, nil, ErrOtherCall
		}
		out := r.out(id)
		return nil, &out, nil
	}
	if len(l.calls) >= maxReceipts && !l.dropOldestEndedLocked() {
		return nil, nil, ErrTooManyCalls
	}
	if l.calls == nil {
		l.calls = map[callKey]*receipt{}
	}
	r := &receipt{tool: tool, fp: fp, agent: agent, state: CallWaiting, started: m.now()}
	l.calls[k] = r
	return &Call{m: m, tokenID: tokenID, agent: agent, tool: tool, id: id, l: l, rec: r}, nil, nil
}

// dropOldestEndedLocked drops the receipt that ended first, reporting
// whether there was one. m.mu must be held.
func (l *live) dropOldestEndedLocked() bool {
	var oldest callKey
	var seq uint64
	found := false
	for k, r := range l.calls {
		if r.state.Ended() && (!found || r.endSeq < seq) {
			oldest, seq, found = k, r.endSeq, true
		}
	}
	if found {
		delete(l.calls, oldest)
	}
	return found
}

// admitLocked marks the call granted its lease. m.mu must be held.
func (c *Call) admitLocked() {
	c.admitted = true
	if c.rec != nil && c.rec.state == CallWaiting {
		c.rec.state = CallRunning
	}
}

// End records how the call ended, once it has answered: ok if it answered
// success. A call never admitted gives its receipt back, so its id can be
// sent again. One admitted ends interrupted if a lease it held was cut
// short (its context ended before it let go), else ok or error. Calling it
// again does nothing.
func (c *Call) End(ok bool) {
	if c == nil || c.m == nil {
		return
	}
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	c.endLocked(ok)
}

// EndPanicked ends a call whose handler panicked: interrupted if it was
// admitted.
func (c *Call) EndPanicked() {
	if c == nil || c.m == nil {
		return
	}
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	c.cut = true
	c.endLocked(false)
}

func (c *Call) endLocked(ok bool) {
	if c.ended {
		return
	}
	c.ended = true
	if c.rec == nil {
		return
	}
	k := callKey{c.tokenID, c.id}
	if !c.admitted {
		if c.l.calls[k] == c.rec {
			delete(c.l.calls, k)
		}
		return
	}
	switch {
	case c.cut:
		c.rec.state = CallInterrupted
	case ok:
		c.rec.state = CallOK
	default:
		c.rec.state = CallError
	}
	c.rec.ended = c.m.now()
	c.l.endSeq++
	c.rec.endSeq = c.l.endSeq
}

type callCtxKey struct{}

// WithCall makes c the call a lease taken with ctx admits.
func WithCall(ctx context.Context, c *Call) context.Context {
	return context.WithValue(ctx, callCtxKey{}, c)
}

// CallFrom is the call ctx carries, or nil.
func CallFrom(ctx context.Context) *Call {
	c, _ := ctx.Value(callCtxKey{}).(*Call)
	return c
}

// CallReceipt is a token's own receipt for a call id on a live session.
func (m *Manager) CallReceipt(sessionID, tokenID, id string) (Receipt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return Receipt{}, false
	}
	r, ok := l.calls[callKey{tokenID, id}]
	if !ok {
		return Receipt{}, false
	}
	return r.out(id), true
}

// RunningCall is one of a token's calls under way on a session: holding
// its lease (running), or with an id and waiting for its turn.
type RunningCall struct {
	Tool    string
	CallID  string // "" without one
	State   CallState
	Started time.Time
}

// RunningCalls lists a token's calls under way on a live session, oldest
// first: those holding a lease, and those with an id that are waiting for
// their turn or still running after their lease (a Visual click waiting
// for its download, or taking its screenshot).
func (m *Manager) RunningCalls(sessionID, tokenID string) []RunningCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return nil
	}
	var out []RunningCall
	leased := map[*receipt]bool{}
	for _, set := range []map[*lease]struct{}{l.inputs, l.leases} {
		for ls := range set {
			if c := ls.call; c != nil && c.tokenID == tokenID {
				out = append(out, RunningCall{Tool: c.tool, CallID: c.id, State: CallRunning, Started: ls.started})
				if c.rec != nil {
					leased[c.rec] = true
				}
			}
		}
	}
	for k, r := range l.calls {
		if k.tokenID == tokenID && !r.state.Ended() && !leased[r] {
			out = append(out, RunningCall{Tool: r.tool, CallID: k.id, State: r.state, Started: r.started})
		}
	}
	slices.SortStableFunc(out, func(a, b RunningCall) int { return a.Started.Compare(b.Started) })
	return out
}

// RecentCall is an activity entry one of a token's calls wrote.
type RecentCall struct {
	At   time.Time
	What string
	// FromPage: What holds words a page chose (a downloaded file's name).
	FromPage bool
	Tool     string
	CallID   string    // "" without one
	State    CallState // the call's receipt's state now; "" without one
}

// RecentCalls is a live session's last n activity entries made by a
// token's calls, newest first, with each call's receipt state. The state
// is looked up by call id when asked: should the id have been taken again
// since (its receipt given back, or dropped 1,000 receipts later), an old
// entry shows the new call's.
func (m *Manager) RecentCalls(sessionID, tokenID string, n int) []RecentCall {
	// Scrubbed with the session's secrets as they are now, then cut, as
	// the web UI shows an entry (shown).
	sc := m.lockScrubber(sessionID)
	defer m.mu.Unlock()
	l := m.findLocked(sessionID)
	if l == nil {
		return nil
	}
	var out []RecentCall
	for i := len(l.activity) - 1; i >= 0 && len(out) < n; i-- {
		a := l.activity[i]
		// Only what the token's own calls did: never BrowserPier's
		// entries, a title among them (PROJ-158).
		if a.Kind != KindAction || a.TokenID != tokenID || tokenID == "" {
			continue
		}
		rc := RecentCall{At: a.At, What: shown(sc, a).What, FromPage: a.FromPage, Tool: a.Tool, CallID: a.CallID}
		if a.CallID != "" {
			if r, ok := l.calls[callKey{tokenID, a.CallID}]; ok {
				rc.State = r.state
			}
		}
		out = append(out, rc)
	}
	return out
}
