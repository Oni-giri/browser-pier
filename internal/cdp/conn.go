// Package cdp is BrowserPier's own Chrome DevTools Protocol client, for
// the orchestrator's calls into a browser: the page tools (PROJ-21), the
// content endpoints (PROJ-20) and checkers.
//
// It is built so that BrowserPier's calls stay as quiet as a browser can
// be driven over CDP:
//
//   - One short-lived connection per call. Dial opens a browser-level
//     connection through the container's control socket (never through
//     the agent proxy), and the caller closes it when its call ends, so
//     nothing stays attached between calls.
//   - Each caller sends only what its own AllowList names. No allow-list
//     may hold a domain's enable, or DOM.getDocument (see AllowList).
//   - Code runs only in isolated worlds the connection created itself
//     (Target.IsolatedWorld), and never by context number, which a
//     cross-process navigation can re-point at a page's own world: it runs
//     through Runtime.callFunctionOn on the world's globalThis handle (Eval,
//     EvalObject), on verified nodes (ResolveNode) and on objects those
//     return. Everything else is refused (see check). The page's own world
//     never sees BrowserPier's scripts, and nothing is added to the page.
//     A caller's allow-list needs Page.getFrameTree, Page.createIsolatedWorld,
//     Runtime.evaluate (for the bootstrap only), Runtime.callFunctionOn and,
//     for nodes, DOM.resolveNode.
//
// Typical use, for a caller with its own unexported allow-list:
//
//	var contentList = cdp.NewAllowList("content endpoints", "Target.getTargets", "Target.attachToTarget", …)
//
//	lease, err := m.PageLease(ctx, sessionID, tokenID) // sessions.Manager: one call at a time, cancelled by a handoff
//	defer lease.Release()
//	c, err := cdp.Dial(lease.Ctx, m.Layout().ControlSocket(profile), contentList, cdp.Options{})
//	defer c.Close()
//	t, err := c.Attach(lease.Ctx, targetID)  // a flat session on a page
//	world, err := t.IsolatedWorld(lease.Ctx, frameID)
//	var title string
//	err = t.Eval(lease.Ctx, world, "document.title", &title)
//
// Contexts: Dial's bounds only the dial, and a Call's only the wait for
// its answer. Neither ever cuts the connection, which lives until Close:
// a caller whose context has ended can still send what finishes its work
// (a button's release) with a fresh context. A renderer blocked by a
// JavaScript dialog answers no renderer-side method (see
// images/chromium/ANTI-BOT.md), so give each Call a timeout.
//
// Events are never subscribed to (nothing is enabled), so the few the
// browser sends anyway are dropped. internal/cdp/cdptest has a fake
// browser for tests.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// DefaultMaxMessage caps one message from the browser, unless Options
// says otherwise. A bigger answer (a huge page's accessibility tree, say)
// fails the call with ErrTooLarge and closes the connection.
const DefaultMaxMessage = 32 << 20

// writeTimeout bounds sending one message; a write that takes longer
// means the connection is gone.
const writeTimeout = 10 * time.Second

var (
	// ErrTooLarge: the browser's answer was bigger than the connection's cap.
	ErrTooLarge = errors.New("the browser's answer was too large")
	// ErrClosed: the connection closed before the answer came.
	ErrClosed = errors.New("the connection to the browser closed")
	// ErrNotAllowed: the caller's allow-list doesn't name the method, or
	// the code would run outside an isolated world.
	ErrNotAllowed = errors.New("not allowed")
	// ErrNotSent: the call never reached the browser (refused, or its
	// context had ended, or the connection was gone). A call that fails
	// any other way may have been carried out.
	ErrNotSent = errors.New("not sent")
	// ErrWorldGone: the isolated world's document changed or went (a
	// navigation); make the world again with IsolatedWorld.
	ErrWorldGone = errors.New("the isolated world's document is gone")
)

// Error is an error the browser answered with.
type Error struct {
	Method  string
	Code    int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Method, e.Message) }

// Options tune a connection.
type Options struct {
	// MaxMessage caps one message from the browser; 0 is DefaultMaxMessage.
	MaxMessage int64
}

// Conn is one browser-level CDP connection.
type Conn struct {
	ws    *websocket.Conn
	allow *AllowList
	end   context.CancelFunc // ends the connection's own context

	mu      sync.Mutex
	nextID  int64
	waiting map[int64]chan reply
	err     error // why the connection ended; set once, before done closes
	// worlds are the isolated worlds this connection created, by session
	// and execution context. globals are their verified globalThis objects;
	// trusted are objects known to live in such a world; pending are nodes
	// resolved into one and not yet verified (see check).
	worlds  map[worldKey]bool
	globals map[objKey]bool
	trusted map[objKey]bool
	pending map[objKey]bool

	done chan struct{}
}

type worldKey struct {
	session string
	context int64
}

type objKey struct {
	session string
	object  string
}

type reply struct {
	result json.RawMessage
	err    *Error
}

// Dial opens a browser-level connection through the control socket at
// sock, sending only what allow names. ctx bounds the dial only: the
// connection lives until Close, whatever happens to ctx after Dial
// returns. Each Call takes its own context.
func Dial(ctx context.Context, sock string, allow *AllowList, opts Options) (*Conn, error) {
	if allow == nil {
		return nil, errors.New("cdp: no allow-list")
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	path, err := browserPath(ctx, hc)
	if err != nil {
		return nil, err
	}
	// net/http ties an upgraded connection to its request's context, so
	// the WebSocket is dialled with one that ctx cancels only while the
	// handshake lasts: afterwards the connection lives until Close, and a
	// caller whose ctx ends can still finish what it started (release a
	// pressed button, say).
	life, end := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, end)
	//nolint:bodyclose // after an upgrade the connection owns the body
	ws, _, err := websocket.Dial(life, "ws://control/cdp"+path, &websocket.DialOptions{HTTPClient: hc})
	if !stop() {
		err = errors.Join(ctx.Err(), err)
		if ws != nil {
			_ = ws.CloseNow()
		}
	}
	if err != nil {
		end()
		return nil, fmt.Errorf("connecting to the browser: %w", err)
	}
	limit := opts.MaxMessage
	if limit <= 0 {
		limit = DefaultMaxMessage
	}
	ws.SetReadLimit(limit)
	c := &Conn{ws: ws, allow: allow, end: end, waiting: map[int64]chan reply{},
		worlds: map[worldKey]bool{}, globals: map[objKey]bool{}, trusted: map[objKey]bool{}, pending: map[objKey]bool{},
		done: make(chan struct{})}
	go c.read() //nolint:gosec // the reader lives as long as the connection, not the dial
	return c, nil
}

// browserPath asks the control socket for the browser's DevTools path,
// /devtools/browser/<id>.
func browserPath(ctx context.Context, hc *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/cdp/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching the browser: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return "", fmt.Errorf("reading the browser's version: %w", err)
	}
	u, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil || !strings.HasPrefix(u.Path, "/devtools/browser/") {
		return "", errors.New("unexpected browser endpoint")
	}
	return u.Path, nil
}

// Close closes the connection; calls still waiting fail with ErrClosed.
// Closing detaches every session the connection attached.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	err := c.ws.CloseNow()
	c.end()
	return err
}

// Done is closed once the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

func (c *Conn) read() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			if strings.Contains(err.Error(), "read limited at") {
				err = ErrTooLarge
			} else {
				err = fmt.Errorf("%w: %w", ErrClosed, err)
			}
			c.fail(err)
			_ = c.ws.CloseNow()
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
			continue // an event: nothing is enabled, so nothing to follow
		}
		c.mu.Lock()
		ch := c.waiting[msg.ID]
		delete(c.waiting, msg.ID)
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		r := reply{result: msg.Result}
		if msg.Error != nil {
			r.err = &Error{Code: msg.Error.Code, Message: msg.Error.Message}
		}
		ch <- r
	}
}

// Call sends a browser-level method (sessionID "") or one to an attached
// session, and decodes the result into out (if not nil).
func (c *Conn) Call(ctx context.Context, sessionID, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		raw = []byte("{}")
	}
	if err := c.check(sessionID, method, raw); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan reply, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method, "params": json.RawMessage(raw)}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	b, _ := json.Marshal(msg)
	if err := ctx.Err(); err != nil {
		c.forget(id)
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	// The WebSocket closes the whole connection if a write's context ends,
	// even just after the message went out; so ctx only bounds the wait
	// for the answer, and a cancelled call never cuts the connection
	// (another call on it may still need it, such as a button's release).
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	err = c.ws.Write(wctx, websocket.MessageText, b)
	cancel()
	if err != nil {
		c.forget(id)
		return fmt.Errorf("%w: %w: %w", ErrNotSent, ErrClosed, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			r.err.Method = method
			return r.err
		}
		c.record(sessionID, method, raw, r.result)
		if out != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, out)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		return err
	}
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.waiting, id)
	c.mu.Unlock()
}

// check refuses what the allow-list doesn't name, and code outside the
// isolated worlds this connection made.
//
// A world is named by a number (its execution context ID), but numbers
// are only unique within one renderer: after a cross-process navigation
// the same flat session talks to a new renderer, whose numbers start
// again, so a remembered number can name the page's own world. So code
// never runs by number. Runtime.evaluate by number is allowed only for
// the bootstrap that reads the world's global object, `this` (harmless anywhere),
// which Target.IsolatedWorld keeps only if the frame's document didn't
// change around it. Code then runs through Runtime.callFunctionOn on that
// object, or on objects it gave: remote object IDs carry their renderer's
// isolate, so they can't be re-pointed. A node DOM.resolveNode gives is
// verified first (Target.ResolveNode): one call, with the world's global
// as its argument, that the browser refuses unless both are in the same
// world.
func (c *Conn) check(sessionID, method string, params []byte) error {
	if err := forbidden(method); err != nil {
		return fmt.Errorf("cdp: %w: %w", ErrNotAllowed, err)
	}
	if !c.allow.Allows(method) {
		return fmt.Errorf("cdp: %w: %s may not send %s", ErrNotAllowed, c.allow.name, method)
	}
	var p struct {
		ContextID           int64  `json:"contextId"`
		UniqueContextID     string `json:"uniqueContextId"`
		ExecutionContextID  int64  `json:"executionContextId"`
		ObjectID            string `json:"objectId"`
		Flatten             *bool  `json:"flatten"`
		Expression          string `json:"expression"`
		ReturnByValue       bool   `json:"returnByValue"`
		FunctionDeclaration string `json:"functionDeclaration"`
		Arguments           []struct {
			ObjectID string `json:"objectId"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	c.mu.Lock()
	defer c.mu.Unlock()
	inWorld := func(id int64) bool { return id != 0 && c.worlds[worldKey{sessionID, id}] }
	obj := func(id string) objKey { return objKey{sessionID, id} }
	switch method {
	case "Runtime.evaluate":
		if p.UniqueContextID != "" || !inWorld(p.ContextID) || p.Expression != bootstrap || p.ReturnByValue {
			return fmt.Errorf("cdp: %w: Runtime.evaluate runs only the isolated-world bootstrap; use Target.Eval", ErrNotAllowed)
		}
	case "Runtime.callFunctionOn":
		trusted := p.ObjectID != "" && c.trusted[obj(p.ObjectID)]
		verifying := p.ObjectID != "" && c.pending[obj(p.ObjectID)] && p.FunctionDeclaration == verifyFn &&
			len(p.Arguments) == 1 && c.globals[obj(p.Arguments[0].ObjectID)]
		if !trusted && !verifying {
			return fmt.Errorf("cdp: %w: Runtime.callFunctionOn on an object not from a verified isolated world", ErrNotAllowed)
		}
	case "DOM.resolveNode":
		if !inWorld(p.ExecutionContextID) {
			return fmt.Errorf("cdp: %w: DOM.resolveNode outside an isolated world", ErrNotAllowed)
		}
	case "Runtime.releaseObject":
		if !c.trusted[obj(p.ObjectID)] && !c.pending[obj(p.ObjectID)] {
			return fmt.Errorf("cdp: %w: Runtime.releaseObject of an object from elsewhere", ErrNotAllowed)
		}
	case "Target.attachToTarget":
		// Only flat sessions: the other kind wraps messages, and this
		// check would no longer see them.
		if p.Flatten == nil || !*p.Flatten {
			return fmt.Errorf("cdp: %w: Target.attachToTarget without flatten", ErrNotAllowed)
		}
	}
	return nil
}

// bootstrap is the only expression Runtime.evaluate may send: the global
// object, as `this` at the top level gives it. Unlike the name globalThis,
// which a page can redefine with a getter, it looks nothing up, so even
// if it ever landed in a page's own world the page couldn't notice.
const bootstrap = "this"

// verifyFn is the only function run on a node DOM.resolveNode gave, with
// the world's global as its argument: the browser refuses the call unless
// both are in the same world, and the function itself touches nothing.
const verifyFn = "function(g) { return g === globalThis; }"

// record notes the isolated worlds a call made and the objects it gave.
func (c *Conn) record(sessionID, method string, params, result json.RawMessage) {
	switch method {
	case "Page.createIsolatedWorld", "Runtime.callFunctionOn", "DOM.resolveNode":
	default:
		return
	}
	var r struct {
		ExecutionContextID int64         `json:"executionContextId"`
		Result             *RemoteObject `json:"result"`
		Object             *RemoteObject `json:"object"`
	}
	var p struct {
		ObjectID string `json:"objectId"`
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	_ = json.Unmarshal(params, &p)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "Page.createIsolatedWorld":
		if r.ExecutionContextID != 0 {
			c.worlds[worldKey{sessionID, r.ExecutionContextID}] = true
		}
	case "DOM.resolveNode":
		if r.Object != nil && r.Object.ObjectID != "" {
			c.pending[objKey{sessionID, r.Object.ObjectID}] = true
		}
	case "Runtime.callFunctionOn":
		on := objKey{sessionID, p.ObjectID}
		if c.pending[on] {
			// The verification call: it ran, so both are in the world.
			if r.Result != nil && string(r.Result.Value) == "true" {
				delete(c.pending, on)
				c.trusted[on] = true
			}
			return
		}
		// What a function in the world returns lives in the world.
		if r.Result != nil && r.Result.ObjectID != "" {
			c.trusted[objKey{sessionID, r.Result.ObjectID}] = true
		}
	}
}

// trustGlobal records a world's globalThis, read by the bootstrap and
// checked by Target.IsolatedWorld.
func (c *Conn) trustGlobal(sessionID, objectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := objKey{sessionID, objectID}
	c.globals[k], c.trusted[k] = true, true
}

// RemoteObject is a value in the page, as CDP's Runtime domain gives it.
type RemoteObject struct {
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	ObjectID string          `json:"objectId,omitempty"`
}

// ExceptionDetails is a script's exception.
type ExceptionDetails struct {
	Text      string        `json:"text"`
	Exception *RemoteObject `json:"exception"`
}

// Target is a flat session attached to one target (a tab).
type Target struct {
	c         *Conn
	ID        string // the target's ID
	SessionID string

	mu      sync.Mutex
	globals map[int64]string // world (execution context) → its verified globalThis
}

// Attach attaches a flat session to a target. It detaches when the
// connection closes, or with Detach.
func (c *Conn) Attach(ctx context.Context, targetID string) (*Target, error) {
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &r); err != nil {
		return nil, err
	}
	return &Target{c: c, ID: targetID, SessionID: r.SessionID, globals: map[int64]string{}}, nil
}

// Detach ends the session; best effort, with a short timeout of its own
// so it runs even when ctx has ended.
func (t *Target) Detach() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = t.c.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": t.SessionID}, nil)
}

// Call sends a method to the target's session.
func (t *Target) Call(ctx context.Context, method string, params, out any) error {
	return t.c.Call(ctx, t.SessionID, method, params, out)
}

// Conn is the connection the target's session is on.
func (t *Target) Conn() *Conn { return t.c }

// worldName names BrowserPier's isolated world. Chromium keeps one world
// per name and frame, so asking again on the same document returns the
// same context instead of making another.
const worldName = "browserpier"

// frameLoader is the document (loader ID) frameID has now, "" if the
// frame isn't there.
func (t *Target) frameLoader(ctx context.Context, frameID string) (string, error) {
	type frameTree struct {
		Frame struct {
			ID       string `json:"id"`
			LoaderID string `json:"loaderId"`
		} `json:"frame"`
		ChildFrames []json.RawMessage `json:"childFrames"`
	}
	var r struct {
		FrameTree json.RawMessage `json:"frameTree"`
	}
	if err := t.Call(ctx, "Page.getFrameTree", nil, &r); err != nil {
		return "", err
	}
	queue := []json.RawMessage{r.FrameTree}
	for len(queue) > 0 {
		var f frameTree
		if json.Unmarshal(queue[0], &f) != nil {
			return "", errors.New("cdp: unexpected frame tree")
		}
		queue = queue[1:]
		if f.Frame.ID == frameID {
			return f.Frame.LoaderID, nil
		}
		queue = append(queue, f.ChildFrames...)
	}
	return "", nil
}

// IsolatedWorld returns an isolated world in frameID's current document,
// creating it if needed, and reads the world's globalThis, which Eval and
// ResolveNode go through. The read is kept only if the frame's document
// didn't change around it (Page.getFrameTree before and after, so the
// caller's allow-list needs it); otherwise ErrWorldGone, and the caller
// asks again. A world dies with its document: after a navigation, ask
// again.
func (t *Target) IsolatedWorld(ctx context.Context, frameID string) (int64, error) {
	before, err := t.frameLoader(ctx, frameID)
	if err != nil {
		return 0, err
	}
	var w struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := t.Call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": worldName}, &w); err != nil {
		return 0, err
	}
	var g struct {
		Result RemoteObject `json:"result"`
	}
	if err := t.Call(ctx, "Runtime.evaluate", map[string]any{"expression": bootstrap, "contextId": w.ExecutionContextID}, &g); err != nil {
		return 0, worldGone(err)
	}
	after, err := t.frameLoader(ctx, frameID)
	if err != nil {
		return 0, err
	}
	if before == "" || after != before || g.Result.ObjectID == "" {
		return 0, ErrWorldGone
	}
	t.c.trustGlobal(t.SessionID, g.Result.ObjectID)
	t.mu.Lock()
	t.globals[w.ExecutionContextID] = g.Result.ObjectID
	t.mu.Unlock()
	return w.ExecutionContextID, nil
}

func (t *Target) global(world int64) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	g, ok := t.globals[world]
	if !ok {
		return "", fmt.Errorf("%w: world %d wasn't made by IsolatedWorld on this target", ErrWorldGone, world)
	}
	return g, nil
}

// worldGone turns the browser's "that object or context is no more" into
// ErrWorldGone.
func worldGone(err error) error {
	var ce *Error
	if errors.As(err, &ce) && (strings.Contains(ce.Message, "find object") || strings.Contains(ce.Message, "find context") ||
		strings.Contains(ce.Message, "Inspected target navigated or closed") ||
		strings.Contains(ce.Message, "context was destroyed") || strings.Contains(ce.Message, "same JavaScript world")) {
		return fmt.Errorf("%w: %w", ErrWorldGone, err)
	}
	return err
}

// Eval evaluates an expression in an isolated world from IsolatedWorld,
// returning its value as JSON (returnByValue). It runs as a function
// called on the world's globalThis, never by context number. A thrown
// exception is an error; ErrWorldGone means the document went.
func (t *Target) Eval(ctx context.Context, world int64, expression string, out any) error {
	g, err := t.global(world)
	if err != nil {
		return err
	}
	var r struct {
		Result           RemoteObject      `json:"result"`
		ExceptionDetails *ExceptionDetails `json:"exceptionDetails"`
	}
	if err := t.Call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": g,
		"functionDeclaration": "function() { return (\n" + expression + "\n); }",
		"returnByValue":       true, "awaitPromise": true, "silent": true}, &r); err != nil {
		return worldGone(err)
	}
	if r.ExceptionDetails != nil {
		return fmt.Errorf("script error: %s", r.ExceptionDetails.Text)
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

// EvalObject is Eval returning the value as an object handle in the
// world (an element, say), which Runtime.callFunctionOn and
// DOM.describeNode then take.
func (t *Target) EvalObject(ctx context.Context, world int64, expression string) (RemoteObject, error) {
	g, err := t.global(world)
	if err != nil {
		return RemoteObject{}, err
	}
	var r struct {
		Result           RemoteObject      `json:"result"`
		ExceptionDetails *ExceptionDetails `json:"exceptionDetails"`
	}
	if err := t.Call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": g,
		"functionDeclaration": "function() { return (\n" + expression + "\n); }", "silent": true}, &r); err != nil {
		return RemoteObject{}, worldGone(err)
	}
	if r.ExceptionDetails != nil {
		return RemoteObject{}, fmt.Errorf("script error: %s", r.ExceptionDetails.Text)
	}
	return r.Result, nil
}

// ResolveNode gives a node (by backend node ID) as an object in an
// isolated world from IsolatedWorld, verified to be in that world before
// any code runs on it; Runtime.callFunctionOn then works on it. A node
// that's gone is the browser's *Error; ErrWorldGone means the document
// went.
func (t *Target) ResolveNode(ctx context.Context, world, backendNodeID int64) (string, error) {
	g, err := t.global(world)
	if err != nil {
		return "", err
	}
	var r struct {
		Object RemoteObject `json:"object"`
	}
	if err := t.Call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": backendNodeID, "executionContextId": world}, &r); err != nil {
		return "", err
	}
	var v struct {
		Result RemoteObject `json:"result"`
	}
	if err := t.Call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": r.Object.ObjectID, "functionDeclaration": verifyFn,
		"arguments": []map[string]any{{"objectId": g}}, "returnByValue": true, "silent": true}, &v); err != nil {
		return "", worldGone(err)
	}
	if string(v.Result.Value) != "true" {
		return "", ErrWorldGone
	}
	return r.Object.ObjectID, nil
}
