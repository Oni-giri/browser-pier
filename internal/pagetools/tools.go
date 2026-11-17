// Package pagetools is BrowserPier's API-mode page tools (PROJ-21):
// snapshot, find, act, wait_for, navigate and tabs. They let an agent
// that speaks only MCP (or REST) read and act on a page in a few hundred
// tokens, without pulling raw HTML or a screenshot into its context.
//
// Each call takes a page lease on the session (sessions.Manager.PageLease),
// opens one short-lived CDP connection through the container's control
// socket (internal/cdp) that sends only the methods in toolsAllowList,
// and closes it before answering. Nothing is enabled, scripts run only in
// an isolated world, and nothing is written into the page: the tools are
// meant to be no easier to detect than a person using the browser (see
// images/chromium/ANTI-BOT.md). They work in API mode only: in Visual
// mode no CDP client may attach to the page.
//
// Everything they return that comes from the page goes through the
// untrusted-content fence (internal/fence).
package pagetools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/motion"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// toolsAllowList is every CDP method the page tools send, and no more
// (PROJ-21 §1). It is unexported: no other caller of internal/cdp, such
// as PROJ-57's checker, can borrow a list that allows input and DOM
// writes. Accessibility.getPartialAXTree is an addition to the design's
// list: it echoes what a step hit without reading the whole tree; so is
// DOM.setFileInputFiles, act's upload step (PROJ-117), which only ever
// gets /files/uploads/<name> paths. SPEC §8 lists the same methods.
var toolsAllowList = cdp.NewAllowList("page tools",
	"Target.getTargets", "Target.attachToTarget", "Target.detachFromTarget", "Target.activateTarget", "Target.createTarget", "Target.closeTarget",
	"Page.getFrameTree", "Page.createIsolatedWorld", "Page.navigate", "Page.navigateToHistoryEntry", "Page.getNavigationHistory", "Page.reload",
	"Runtime.evaluate", "Runtime.callFunctionOn", "Runtime.releaseObject",
	"DOM.resolveNode", "DOM.describeNode", "DOM.getContentQuads", "DOM.scrollIntoViewIfNeeded", "DOM.focus", "DOM.setFileInputFiles",
	"Accessibility.getFullAXTree", "Accessibility.getPartialAXTree",
	"DOMSnapshot.captureSnapshot",
	"Input.dispatchMouseEvent", "Input.dispatchKeyEvent", "Input.insertText",
)

// AllowedMethods lists the CDP methods the tools may send, for docs and tests.
func AllowedMethods() []string { return toolsAllowList.Methods() }

const (
	// maxMessage caps one answer from the browser: about a 30,000-element
	// page's AX tree (see the spike). What a page makes the orchestrator
	// hold is bounded by it, and by maxReads.
	maxMessage = 32 << 20
	// maxReads is how many whole-page reads (DOM snapshot and AX tree)
	// run at once, across all sessions, so a few large pages can't take a
	// small host's memory.
	maxReads = 2
	// quickTimeout is how long a renderer gets for a small call before
	// the page counts as not responding; slowTimeout is for reading a
	// whole page.
	quickTimeout = 5 * time.Second
	slowTimeout  = 20 * time.Second
)

// Error is a failure to tell the agent, with the HTTP status the API
// answers. Msg is safe to return as is: any part from the page is fenced.
type Error struct {
	Status int
	Msg    string
	// final: the answer already says why the lease ended (act broken
	// off by Take control), so fail keeps it.
	final bool
}

func (e *Error) Error() string { return e.Msg }

// errUnresponsive: a call into the page timed out while the browser
// itself still answers. Without Page.enable a dialog can't be seen or
// closed (see the spike), so a person has to.
var errUnresponsive = &Error{Status: http.StatusGatewayTimeout, Msg: "the page is busy or has a dialog open; if it stays that way, request a handoff."}

// Caller is the agent making a call.
type Caller struct {
	TokenID string
	Agent   string // the token's name, for the activity log
}

// Tools runs the page tools for every session.
type Tools struct {
	m   *sessions.Manager
	log *slog.Logger
	// files is the profiles' files folders, where act's upload step
	// finds what it sets on a file input; nil: no uploads.
	files *files.Store

	mu    sync.Mutex
	pages map[string]*state // by session ID
	// bases are the item lists diffs compare against (PROJ-114), by
	// session and tab, in memory only; baseBytes is about what they take
	// together (at most maxBaselines), baseSeq orders them by when they
	// were stored. Guarded by mu, since dropping the oldest reaches into
	// other sessions'.
	bases     map[baseKey]*baseline
	baseBytes int
	baseSeq   uint64
	// unkept marks the tabs whose last read was too large to keep
	// (maxBaseline), with the document it was of. Guarded by mu.
	unkept map[baseKey]baseDoc

	// quick and slow are quickTimeout and slowTimeout; tests shorten them.
	quick, slow time.Duration
	// settle is how long act waits after a step for the page to react.
	settle time.Duration
	// coverWait is how long a pointer step waits for what covers its
	// element to go, unless it says; coverPoll how often it looks.
	coverWait, coverPoll time.Duration
	// poll is how often wait_for and navigate look at the page.
	poll time.Duration
	// ambiguousFor is how long wait_for lets a target match several
	// elements before it fails (PROJ-123).
	ambiguousFor time.Duration
	// chainQuiet, chainPoll and chainCap are navigate's watch for script
	// redirects after a load (PROJ-121): how long a document must stay
	// put, how often it is looked at meanwhile, and the longest the chain
	// is followed. Tests shorten them.
	chainQuiet, chainPoll, chainCap time.Duration
	// navWait is navigateTimeout; tests shorten it.
	navWait time.Duration
	// reads holds a slot per whole-page read under way (maxReads);
	// readWait is how long a call waits for one before answering busy.
	reads    chan struct{}
	readWait time.Duration
	// newMotion makes a session's source of human pacing (PROJ-113), and
	// pause waits out one of its pauses; tests fix the one and skip the
	// other.
	newMotion func() *motion.Motion
	pause     func(ctx context.Context, d time.Duration) error
}

func New(m *sessions.Manager, log *slog.Logger) *Tools {
	t := &Tools{m: m, log: log, pages: map[string]*state{}, bases: map[baseKey]*baseline{}, unkept: map[baseKey]baseDoc{}, quick: quickTimeout, slow: slowTimeout,
		settle: 100 * time.Millisecond, coverWait: defaultCoverWait, coverPoll: 100 * time.Millisecond, poll: 250 * time.Millisecond, ambiguousFor: time.Second,
		chainQuiet: chainQuiet, chainPoll: chainPoll, chainCap: chainCap, navWait: navigateTimeout, reads: make(chan struct{}, maxReads), readWait: 10 * time.Second,
		newMotion: motion.NewRandom, pause: sleep}
	if m != nil {
		// A session's refs and kept lists go as it ends.
		m.OnEnd(t.forgetSession)
	}
	return t
}

// SetFiles gives act's upload step the profiles' files folders.
func (t *Tools) SetFiles(fs *files.Store) { t.files = fs }

// sleep waits for d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// state is what the tools remember about one session between calls.
type state struct {
	refs *refs
	gen  uint64 // the lease generation the refs belong to
	tabs map[string]*tabState
	// order lists tabs by first sight; seq counts activations.
	order []string
	next  int
	seq   int64
	// active is the target the last call found in front.
	active string
	listed bool // tabs have been listed once: new ones are news
	// mot paces act like a hand: one source per session, never shared.
	// pointer is where act's pointer last stopped, in the viewport's
	// CSS pixels; nil before its first move, or once a person has had
	// the browser.
	mot     *motion.Motion
	pointer *motion.Point
}

type tabState struct {
	id        string // t1, t2…
	target    string
	opener    string
	activated int64 // when the tools last saw or made it the front tab
}

func (t *Tools) state(sessionID string, gen uint64) *state {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.pages[sessionID]
	if !ok {
		// Sessions that have ended are forgotten as new ones come.
		if len(t.pages) > 0 {
			live := map[string]bool{}
			for _, s := range t.m.List() {
				live[s.ID] = true
			}
			for id := range t.pages {
				if !live[id] {
					delete(t.pages, id)
					t.dropBaselinesLocked(id)
				}
			}
		}
		st = &state{refs: newRefs(), gen: gen, tabs: map[string]*tabState{}, mot: t.newMotion()}
		t.pages[sessionID] = st
		// A new ref epoch: lists kept with the old refs' items go.
		t.dropBaselinesLocked(sessionID)
	}
	if st.gen != gen {
		// A person had the browser since: whatever the refs named may
		// have changed under them, and so may the page since the last
		// snapshot.
		st.refs.dropAll()
		st.gen = gen
		st.pointer = nil
		t.dropBaselinesLocked(sessionID)
	}
	return st
}

// call is one tool call: a lease, a connection and the active tab.
type call struct {
	t     *Tools
	st    *state
	sess  sessions.Session
	who   Caller
	ctx   context.Context // the caller's
	lease *sessions.Lease
	conn  *cdp.Conn
	// tgt is a session on the active tab, frame and loader its top
	// frame's current document, world its isolated world there.
	tgt    *cdp.Target
	tab    *tabState
	frame  string
	loader string
	url    string // the frame's URL, without its fragment
	// unreachable is the address the browser couldn't load, when the
	// document is its error page.
	unreachable string
	world       int64
	notices     []string
	// notes are BrowserPier's own lines for the result, which go after
	// the fence, not in it: the Cloudflare note, whose only part from the
	// page is a clamped point.
	notes []string
	// instant: the step under way is at instant pace, not a hand's.
	// keyAt is when act last pressed a key at human pace, and keyDue how
	// long after it the next key goes down.
	instant bool
	keyAt   time.Time
	keyDue  time.Duration
	// slotWait, if set and shorter than the tools' readWait, is how long
	// this call's whole-page reads wait for a read slot: a read the call
	// can do without (act's changes) or one bounded by a deadline (a
	// target wait's look) doesn't hold the session for the full wait.
	slotWait time.Duration
}

// begin starts a call on a session the caller may use, sending only
// what allow names.
func (t *Tools) begin(ctx context.Context, s sessions.Session, who Caller, allow *cdp.AllowList) (*call, error) {
	switch {
	case s.Mode != profiles.ModeAPI:
		return nil, &Error{Status: http.StatusConflict, Msg: "the page tools are for API-mode sessions; this one is in Visual mode, where nothing may attach to the page over CDP. Use screenshot, click and type_text, or restart the browser with mode api"}
	case s.State == sessions.NeedsHelp:
		return nil, &Error{Status: http.StatusConflict, Msg: sessions.ErrHandedOver.Error()}
	case s.State != sessions.Running:
		return nil, &Error{Status: http.StatusConflict, Msg: "the session is " + string(s.State) + ", not running"}
	}
	lease, err := t.m.PageLease(ctx, s.ID, who.TokenID)
	if err != nil {
		return nil, leaseError(err)
	}
	c := &call{t: t, sess: s, who: who, ctx: ctx, lease: lease}
	c.st = t.state(s.ID, lease.Generation)
	conn, err := cdp.Dial(lease.Ctx, t.m.Layout().ControlSocket(s.Profile), allow, cdp.Options{MaxMessage: maxMessage})
	if err != nil {
		err = c.fail(err)
		lease.Release()
		return nil, err
	}
	c.conn = conn
	if err := c.findActive(); err != nil {
		err = c.fail(err) // before end, which cancels the lease
		c.end()
		return nil, err
	}
	return c, nil
}

// end closes the connection and releases the lease.
func (c *call) end() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.lease.Release()
}

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

// fail turns an error into the agent's answer: why the lease ended if it
// did, the caller's own cancellation, or what went wrong.
func (c *call) fail(err error) error {
	var e *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &e) && e.final:
		return e
	case c.lease.Ctx.Err() != nil && c.ctx.Err() == nil:
		return leaseError(context.Cause(c.lease.Ctx))
	case c.ctx.Err() != nil:
		return c.ctx.Err()
	case errors.As(err, &e):
		return e
	case errors.Is(err, cdp.ErrTooLarge):
		return &Error{Status: http.StatusUnprocessableEntity, Msg: "the browser's answer was too large"}
	case errors.Is(err, cdp.ErrWorldGone):
		// An element or the isolated world died with its document under
		// the call: the page navigated, which is no refusal.
		return &Error{Status: http.StatusConflict, Msg: "the page changed while BrowserPier was working on it (it navigated); take a new snapshot and try again"}
	}
	var ce *cdp.Error
	if errors.As(err, &ce) {
		// Chromium's own words, which can quote the page (a URL, say).
		return &Error{Status: http.StatusBadGateway, Msg: "the browser refused: " + fence.Wrap("browser error", ce.Message)}
	}
	// The error's words can carry the page's (a script error's text).
	return &Error{Status: http.StatusBadGateway, Msg: "can't reach the browser: " + fence.Wrap("browser error", err.Error())}
}

// browser sends a browser-level method (Target.*), with the quick
// timeout: the browser process answers those even when a page is stuck.
func (c *call) browser(ctx context.Context, method string, params, out any) error {
	cctx, cancel := context.WithTimeout(ctx, c.t.quick)
	defer cancel()
	err := c.conn.Call(cctx, "", method, params, out)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return &Error{Status: http.StatusBadGateway, Msg: "the browser isn't answering"}
	}
	return err
}

// attach attaches to a tab, with the quick timeout.
func (c *call) attach(ctx context.Context, target string) (*cdp.Target, error) {
	cctx, cancel := context.WithTimeout(ctx, c.t.quick)
	defer cancel()
	t, err := c.conn.Attach(cctx, target)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, &Error{Status: http.StatusBadGateway, Msg: "the browser isn't answering"}
	}
	return t, err
}

// renderer sends a method to the active tab, giving the page timeout to
// answer. If it doesn't while the browser still answers, the page is
// stuck (a dialog, most likely): errUnresponsive.
func (c *call) renderer(ctx context.Context, timeout time.Duration, method string, params, out any) error {
	return c.rendererOn(ctx, c.tgt, timeout, method, params, out)
}

func (c *call) rendererOn(ctx context.Context, tgt *cdp.Target, timeout time.Duration, method string, params, out any) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := tgt.Call(cctx, method, params, out)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		// Browser-side: it answers even while the renderer is blocked.
		bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if tgt.Call(bctx, "Page.getNavigationHistory", nil, nil) == nil {
			return errUnresponsive
		}
		return &Error{Status: http.StatusBadGateway, Msg: "the browser isn't answering"}
	}
	return err
}

// frameTree reads a tab's top frame: its ID, its document and its URL.
func (c *call) frameTree(ctx context.Context, tgt *cdp.Target) (id, loader, url string, err error) {
	f, err := c.topFrame(ctx, tgt)
	return f.ID, f.LoaderID, f.URL, err
}

// topFrame is a target's top frame, as Page.getFrameTree describes it.
type topFrame struct {
	ID       string `json:"id"`
	LoaderID string `json:"loaderId"`
	URL      string `json:"url"`
	// UnreachableURL is set when the document is the browser's error
	// page: the address that couldn't be loaded.
	UnreachableURL string `json:"unreachableUrl"`
}

func (c *call) topFrame(ctx context.Context, tgt *cdp.Target) (topFrame, error) {
	var r struct {
		FrameTree struct {
			Frame topFrame `json:"frame"`
		} `json:"frameTree"`
	}
	if err := c.rendererOn(ctx, tgt, c.t.quick, "Page.getFrameTree", nil, &r); err != nil {
		return topFrame{}, err
	}
	return r.FrameTree.Frame, nil
}

// refresh reads the active tab's current document and isolated world.
//
// The world is made (and its globalThis read) only while the document
// stays the same; a navigation in between (ErrWorldGone) makes it read
// the frame again, a few times, before giving up.
func (c *call) refresh(ctx context.Context) error {
	for try := 0; ; try++ {
		f, err := c.topFrame(ctx, c.tgt)
		if err != nil {
			return err
		}
		frame, loader := f.ID, f.LoaderID
		c.url, c.unreachable = f.URL, f.UnreachableURL
		if loader == c.loader && frame == c.frame && c.world != 0 {
			return nil
		}
		c.frame, c.loader, c.world = frame, loader, 0
		cctx, cancel := context.WithTimeout(ctx, c.t.quick)
		w, err := c.tgt.IsolatedWorld(cctx, frame)
		cancel()
		if errors.Is(err, cdp.ErrWorldGone) && try < worldTries {
			continue
		}
		if err != nil {
			return c.timedOut(ctx, err)
		}
		c.world = w
		return nil
	}
}

// worldTries is how many times a call makes the isolated world again when
// the page navigates under it.
const worldTries = 3

func (c *call) timedOut(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return errUnresponsive
	}
	return err
}

// eval evaluates a script in the active tab's isolated world. The world
// dies with its document, so after a navigation it is made again.
func (c *call) eval(ctx context.Context, expr string, out any) error {
	for try := 0; ; try++ {
		if err := c.refresh(ctx); err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, c.t.quick)
		err := c.tgt.Eval(cctx, c.world, expr, out)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, cdp.ErrWorldGone) && try < worldTries {
			c.world = 0 // the document changed under us
			continue
		}
		return c.timedOut(ctx, err)
	}
}

// note adds to the session's activity log: the action and refs only,
// never page text or typed text.
func (c *call) note(what string) { c.t.m.Note(c.sess.ID, c.who.Agent, what) }

// result fences what a tool says from the page, with any new-tab notices,
// and adds trailer (BrowserPier's own words) after the fence.
func (c *call) result(source, body, trailer string) string {
	if len(c.notices) > 0 {
		if body != "" {
			body += "\n"
		}
		body += strings.Join(c.notices, "\n")
	}
	out := ""
	if body != "" {
		out = fence.Wrap(source, body)
	}
	return c.finish(out, trailer)
}

// pageResult fences page text in the page envelope (PROJ-167): the
// page's URL and title, then body in a content tag saying its format,
// with any new-tab notices after the page, all inside the fence; trailer
// goes after the fence, as with result. An empty body still gets the
// envelope when envelopeWhenEmpty (a snapshot or a text format: the URL
// and title are the answer); otherwise (find, a diff) it is answered as
// result would, with no envelope.
func (c *call) pageResult(source, url, title, format, body, trailer string, envelopeWhenEmpty bool) string {
	if body == "" && !envelopeWhenEmpty {
		return c.result(source, "", trailer)
	}
	out := fence.WrapPage(source, fence.Page{
		// Capped and on one line, as header() writes them.
		URL:     capText(url, maxHeader),
		Title:   capText(collapse(title), maxHeader),
		Format:  format,
		Content: body,
		After:   strings.Join(c.notices, "\n"),
	})
	return c.finish(out, trailer)
}

// finish adds trailer and the notes after a fenced answer.
func (c *call) finish(out, trailer string) string {
	switch {
	case out == "":
		out = trailer
	case trailer != "":
		out += "\n" + trailer
	}
	for _, n := range c.notes {
		if out != "" {
			out += "\n"
		}
		out += n
	}
	c.notes = nil // said once
	return out
}

// run runs one tool call from start to end.
func (t *Tools) run(ctx context.Context, s sessions.Session, who Caller, f func(c *call) (string, error)) (string, error) {
	return runWith(ctx, t, s, who, toolsAllowList, f)
}

// runWith runs one call from start to end with a caller's own allow-list
// (the content endpoints have theirs).
func runWith[T any](ctx context.Context, t *Tools, s sessions.Session, who Caller, allow *cdp.AllowList, f func(c *call) (T, error)) (T, error) {
	var zero T
	c, err := t.begin(ctx, s, who, allow)
	if err != nil {
		return zero, err
	}
	defer c.end()
	out, err := f(c)
	if err != nil {
		return zero, c.fail(err)
	}
	return out, nil
}

// thousands writes n with comma separators.
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
