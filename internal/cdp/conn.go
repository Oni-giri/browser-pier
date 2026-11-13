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
//     (Page.createIsolatedWorld): Runtime.evaluate, Runtime.callFunctionOn
//     and DOM.resolveNode are refused anywhere else. The page's own world
//     never sees BrowserPier's scripts, and nothing is added to the page.
//
// Typical use:
//
//	c, err := cdp.Dial(ctx, sock, myAllowList, cdp.Options{})
//	defer c.Close()
//	t, err := c.Attach(ctx, targetID)        // a flat session on a page
//	world, err := t.IsolatedWorld(ctx, frameID)
//	var v struct{ Result cdp.RemoteObject }
//	err = t.Call(ctx, "Runtime.evaluate", map[string]any{"expression": "document.title", "contextId": world, "returnByValue": true}, &v)
//
// Events are never subscribed to (nothing is enabled), so the few the
// browser sends anyway are dropped.
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
	pending map[int64]chan reply
	err     error // why the connection ended; set once, before done closes
	// worlds are the isolated worlds this connection created, by session
	// and execution context; objects are the remote objects from them.
	worlds  map[worldKey]bool
	objects map[string]bool

	done chan struct{}
}

type worldKey struct {
	session string
	context int64
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
	c := &Conn{ws: ws, allow: allow, end: end, pending: map[int64]chan reply{},
		worlds: map[worldKey]bool{}, objects: map[string]bool{}, done: make(chan struct{})}
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
		ch := c.pending[msg.ID]
		delete(c.pending, msg.ID)
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
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method, "params": json.RawMessage(raw)}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	b, _ := json.Marshal(msg)
	if err := ctx.Err(); err != nil {
		c.forget(id)
		return err
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
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			r.err.Method = method
			return r.err
		}
		c.record(sessionID, method, r.result)
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
	delete(c.pending, id)
	c.mu.Unlock()
}

// check refuses what the allow-list doesn't name, and code outside the
// isolated worlds this connection made.
func (c *Conn) check(sessionID, method string, params []byte) error {
	if err := forbidden(method); err != nil {
		return fmt.Errorf("cdp: %w: %w", ErrNotAllowed, err)
	}
	if !c.allow.Allows(method) {
		return fmt.Errorf("cdp: %w: %s may not send %s", ErrNotAllowed, c.allow.name, method)
	}
	var p struct {
		ContextID          int64  `json:"contextId"`
		UniqueContextID    string `json:"uniqueContextId"`
		ExecutionContextID int64  `json:"executionContextId"`
		ObjectID           string `json:"objectId"`
		Flatten            *bool  `json:"flatten"`
	}
	_ = json.Unmarshal(params, &p)
	c.mu.Lock()
	defer c.mu.Unlock()
	inWorld := func(id int64) bool { return id != 0 && c.worlds[worldKey{sessionID, id}] }
	switch method {
	case "Runtime.evaluate":
		if p.UniqueContextID != "" || !inWorld(p.ContextID) {
			return fmt.Errorf("cdp: %w: Runtime.evaluate outside an isolated world", ErrNotAllowed)
		}
	case "Runtime.callFunctionOn":
		ownObject := p.ObjectID != "" && c.objects[p.ObjectID]
		inOwnWorld := p.ObjectID == "" && inWorld(p.ExecutionContextID)
		if !ownObject && !inOwnWorld {
			return fmt.Errorf("cdp: %w: Runtime.callFunctionOn outside an isolated world", ErrNotAllowed)
		}
	case "DOM.resolveNode":
		if !inWorld(p.ExecutionContextID) {
			return fmt.Errorf("cdp: %w: DOM.resolveNode outside an isolated world", ErrNotAllowed)
		}
	case "Runtime.releaseObject":
		if !c.objects[p.ObjectID] {
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

// record notes the isolated worlds and their objects a call returned.
func (c *Conn) record(sessionID, method string, result json.RawMessage) {
	var r struct {
		ExecutionContextID int64         `json:"executionContextId"`
		Result             *RemoteObject `json:"result"`
		Object             *RemoteObject `json:"object"`
	}
	switch method {
	case "Page.createIsolatedWorld", "Runtime.evaluate", "Runtime.callFunctionOn", "DOM.resolveNode":
	default:
		return
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if method == "Page.createIsolatedWorld" && r.ExecutionContextID != 0 {
		c.worlds[worldKey{sessionID, r.ExecutionContextID}] = true
	}
	for _, o := range []*RemoteObject{r.Result, r.Object} {
		if o != nil && o.ObjectID != "" {
			c.objects[o.ObjectID] = true
		}
	}
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
	return &Target{c: c, ID: targetID, SessionID: r.SessionID}, nil
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

// worldName names BrowserPier's isolated world. Chromium keeps one world
// per name and frame, so asking again on the same document returns the
// same context instead of making another.
const worldName = "browserpier"

// IsolatedWorld returns an isolated world in frameID's current document,
// creating it if needed. A world dies with its document: after a
// navigation, ask again.
func (t *Target) IsolatedWorld(ctx context.Context, frameID string) (int64, error) {
	var r struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := t.Call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": worldName}, &r); err != nil {
		return 0, err
	}
	return r.ExecutionContextID, nil
}

// Eval evaluates expression in an isolated world, returning its value as
// JSON (returnByValue). A thrown exception is an error.
func (t *Target) Eval(ctx context.Context, world int64, expression string, out any) error {
	var r struct {
		Result           RemoteObject      `json:"result"`
		ExceptionDetails *ExceptionDetails `json:"exceptionDetails"`
	}
	if err := t.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "contextId": world, "returnByValue": true, "awaitPromise": true, "silent": true}, &r); err != nil {
		return err
	}
	if r.ExceptionDetails != nil {
		return fmt.Errorf("script error: %s", r.ExceptionDetails.Text)
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

// Conn is the connection the target's session is on.
func (t *Target) Conn() *Conn { return t.c }
