package visualtree

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// Error is a refusal or failure to tell the agent, with the HTTP status
// the API answers. Msg is safe to return as is: any part from the page
// is fenced.
type Error = ReadError

// Caller is the agent making a call.
type Caller struct {
	TokenID string
	Agent   string // the token's name, for the activity log
}

// Tools serves Visual mode's element tree for every session.
type Tools struct {
	m *sessions.Manager

	mu     sync.Mutex
	states map[string]*state // by session ID
	// sock finds a profile's control socket; tests set it.
	sock func(profile string) string
}

// state is what the tools remember about one session between calls.
type state struct {
	refs *refs
	gen  uint64 // the lease generation the refs belong to
}

func New(m *sessions.Manager) *Tools {
	t := &Tools{m: m, states: map[string]*state{}}
	if m != nil {
		t.sock = func(profile string) string { return m.Layout().ControlSocket(profile) }
		m.OnEnd(t.forget) // a session's refs go as it ends
	}
	return t
}

func (t *Tools) forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, id)
}

// state is a session's state for a lease of generation gen: a person had
// the browser since the refs were made if gen moved on, and then they all
// go stale.
func (t *Tools) state(id string, gen uint64) *state {
	st, ok := t.states[id]
	if !ok {
		st = &state{refs: newRefs(), gen: gen}
		t.states[id] = st
	}
	if st.gen != gen {
		st.refs.dropAll()
		st.gen = gen
	}
	return st
}

// SnapshotIn is visual_snapshot's input.
type SnapshotIn struct {
	// MaxChars is the budget for the page's controls and text; 0 gives
	// only the URL and title. nil is DefaultMaxChars.
	MaxChars *int `json:"max_chars,omitempty"`
	// Query keeps only the lines that hold it (ignoring case), each under
	// the containers it is in: the Visual-mode counterpart of find.
	Query string `json:"query,omitempty"`
}

// check refuses a session the tree isn't for.
func check(s sessions.Session) error {
	switch {
	case s.Mode != profiles.ModeVisual:
		return &Error{Status: http.StatusConflict, Msg: "visual_snapshot and click by ref are for Visual-mode sessions; this one is in API mode, where snapshot reads the page and act clicks its refs"}
	case s.State == sessions.NeedsHelp:
		return &Error{Status: http.StatusConflict, Msg: sessions.ErrHandedOver.Error()}
	case s.State != sessions.Running:
		return &Error{Status: http.StatusConflict, Msg: "the session is " + string(s.State) + ", not running"}
	}
	return nil
}

// leaseError is why a read can't start or was cut short.
func leaseError(err error) error {
	switch {
	case errors.Is(err, sessions.ErrHumanDriving):
		return &Error{Status: http.StatusConflict, Msg: "a person is driving this browser from the live view; wait until they hand it back"}
	case errors.Is(err, sessions.ErrHandedOver), errors.Is(err, sessions.ErrSessionHeld), errors.Is(err, sessions.ErrTookControl):
		return &Error{Status: http.StatusConflict, Msg: err.Error()}
	case errors.Is(err, sessions.ErrNotRunning), errors.Is(err, sessions.ErrStopping):
		return &Error{Status: http.StatusConflict, Msg: "the session isn't running"}
	case errors.Is(err, sessions.ErrRevoked):
		return &Error{Status: http.StatusUnauthorized, Msg: err.Error()}
	}
	return err
}

// read runs f under a read lease, mapping a lease that ended under it to
// why.
func (t *Tools) read(ctx context.Context, s sessions.Session, who Caller, f func(lctx context.Context, st *state) error) error {
	if err := check(s); err != nil {
		return err
	}
	lease, err := t.m.ReadLease(ctx, s.ID, who.TokenID)
	if err != nil {
		return leaseError(err)
	}
	defer lease.Release()
	t.mu.Lock()
	st := t.state(s.ID, lease.Generation)
	t.mu.Unlock()
	err = f(lease.Ctx, st)
	switch {
	case err == nil:
		return nil
	case lease.Ctx.Err() != nil && ctx.Err() == nil:
		return leaseError(context.Cause(lease.Ctx))
	case ctx.Err() != nil:
		return ctx.Err()
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Status: http.StatusBadGateway, Msg: "can't reach the browser's element tree: " + fenceError(err.Error())}
}

// Snapshot reads the page of the active tab from the accessibility bus:
// its URL and title, then, within the budget, its controls with refs and
// its text, the open modal first, then what is in view, the main content
// and the rest. A marker says what was left out.
func (t *Tools) Snapshot(ctx context.Context, s sessions.Session, who Caller, in SnapshotIn) (string, error) {
	budget := DefaultMaxChars
	if in.MaxChars != nil {
		budget = *in.MaxChars
	}
	if budget < 0 || budget > MaxMaxChars {
		return "", &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf("max_chars: want 0 to %d", MaxMaxChars)}
	}
	query := strings.TrimSpace(in.Query)
	if utf8.RuneCountInString(query) > maxQuery {
		return "", &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf("query: at most %d characters", maxQuery)}
	}
	if query != "" && budget == 0 {
		return "", &Error{Status: http.StatusBadRequest, Msg: "query: want a max_chars above 0"}
	}
	var out string
	err := t.read(ctx, s, who, func(lctx context.Context, st *state) error {
		tree, err := reader{t.sock(s.Profile)}.read(lctx)
		if err != nil {
			return err
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		st.refs.keepDoc(tree.Doc.App, tree.Doc.ID)
		p := build(tree)
		body, marker := render(p, budget, query, func(node int) string {
			return st.refs.ref(refKey{app: tree.Doc.App, doc: tree.Doc.ID, id: tree.Nodes[node].ID})
		})
		out = wrap("page elements", body)
		if note := cutNote(tree, len(tree.Nodes)); note != "" {
			out += "\n" + note
		}
		if marker != "" {
			out += "\n" + marker
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	t.m.Note(s.ID, who.Agent, "Read the screen's elements")
	return out, nil
}

// cutNote says, in BrowserPier's words, that a read stopped short.
func cutNote(t *Tree, n int) string {
	switch t.Cut {
	case "nodes", "time":
		return fmt.Sprintf("(the page is larger than BrowserPier reads at once: it read %s elements, what is on screen first; scroll, or use query)", thousands(n))
	case "depth":
		return "(the page nests deeper than BrowserPier reads; its deepest elements are left out)"
	}
	return ""
}

// Target is where a click by ref lands: the element's centre now.
type Target struct {
	X, Y int
	// What names the element: its role and name, fenced.
	What string
}

// Locate reads a ref's element again and gives the point to click: its
// centre as it is now. It refuses an element that is gone (stale), off
// screen or under the taskbar (scroll it into view), or covered at its
// centre by something else, by Chromium's hit test. BrowserPier never
// scrolls for the agent.
func (t *Tools) Locate(ctx context.Context, s sessions.Session, who Caller, ref string) (Target, error) {
	var out Target
	err := t.read(ctx, s, who, func(lctx context.Context, st *state) error {
		t.mu.Lock()
		k, err := st.refs.lookup(ref)
		t.mu.Unlock()
		if err != nil {
			return err
		}
		n, err := reader{t.sock(s.Profile)}.node(lctx, k)
		if err != nil {
			return err
		}
		if n.Stale {
			t.mu.Lock()
			st.refs.forget(ref)
			t.mu.Unlock()
			return errStale(ref)
		}
		name := roleWord(n.Role) + " " + field(n.Name)
		x, y := n.X+n.W/2, n.Y+n.H/2
		vx1, vy1 := n.Doc.X+n.Doc.W, n.Doc.Y+n.Doc.H
		if sw, sh := n.ScreenWidth, n.ScreenHeight; sw > 0 && sh > 0 {
			vx1, vy1 = min(vx1, sw), min(vy1, sh-panelHeight)
		}
		if !has(n.States, stateShowing) || n.W <= 0 || n.H <= 0 || x < max(n.Doc.X, 0) || y < max(n.Doc.Y, 0) || x >= vx1 || y >= vy1 {
			return &Error{Status: http.StatusConflict, Msg: ref + " is off screen, or under the taskbar: scroll it into view, take a new visual_snapshot, then click it. It is " + wrap("page element", name)}
		}
		if n.At != nil && !n.At.Contains {
			return &Error{Status: http.StatusConflict, Msg: ref + " is covered at its centre; close or move what covers it first. It is " + wrap("page element", name) + "\nWhat covers it: " + wrap("page element", roleWord(n.At.Role)+" "+field(n.At.Name))}
		}
		out = Target{X: x, Y: y, What: wrap("page element", name)}
		return nil
	})
	return out, err
}
