package cdp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
)

var testList = cdp.NewAllowList("test", "Target.attachToTarget", "Target.getTargets", "Page.createIsolatedWorld", "Page.getFrameTree",
	"Runtime.evaluate", "Runtime.callFunctionOn", "Runtime.releaseObject", "DOM.resolveNode", "Accessibility.getFullAXTree")

func fake(t *testing.T, h cdptest.Handler) (*cdptest.Browser, string) {
	t.Helper()
	b := cdptest.New(h)
	return b, cdptest.Serve(t, b.Handler(nil))
}

// answers is a handler for the methods the tests below use.
func answers(m cdptest.Message) (any, error) {
	switch m.Method {
	case "Target.attachToTarget":
		return map[string]any{"sessionId": "S1"}, nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": 7}, nil
	case "Runtime.evaluate", "Runtime.callFunctionOn":
		return map[string]any{"result": map[string]any{"type": "object", "objectId": "obj-" + m.SessionID}}, nil
	case "DOM.resolveNode":
		return map[string]any{"object": map[string]any{"type": "object", "objectId": "node-1"}}, nil
	case "Accessibility.getFullAXTree":
		return map[string]any{"nodes": []any{map[string]any{"pad": strings.Repeat("x", 4096)}}}, nil
	}
	return map[string]any{}, nil
}

func dial(t *testing.T, sock string, opts cdp.Options) *cdp.Conn {
	t.Helper()
	c, err := cdp.Dial(context.Background(), sock, testList, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNoAllowListMayEnableOrGetTheDocument(t *testing.T) {
	for _, m := range []string{"Runtime.enable", "Page.enable", "DOM.enable", "Accessibility.enable", "Network.enable", "DOM.getDocument", "DOM.requestChildNodes", "Target.setAutoAttach", "nonsense"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("an allow-list took %s", m)
				}
			}()
			cdp.NewAllowList("bad", m)
		}()
	}
}

func TestOnlyAllowedMethodsAreSent(t *testing.T) {
	b, sock := fake(t, answers)
	c := dial(t, sock, cdp.Options{})
	ctx := context.Background()
	for _, m := range []string{"Input.dispatchMouseEvent", "Runtime.enable", "DOM.getDocument", "Page.navigate"} {
		if err := c.Call(ctx, "", m, nil, nil); !errors.Is(err, cdp.ErrNotAllowed) {
			t.Errorf("%s: %v, want ErrNotAllowed", m, err)
		}
	}
	if err := c.Call(ctx, "", "Target.getTargets", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.Methods(); len(got) != 1 || got[0] != "Target.getTargets" {
		t.Errorf("the browser was sent %v", got)
	}
}

// worldPage is a fake page for the isolated-world tests: its frame's
// document can change between calls, and the world's handle with it.
type worldPage struct {
	mu        sync.Mutex
	loaders   []string // what successive Page.getFrameTree calls say
	handleOK  bool     // the world's globalThis handle still resolves
	navigated bool     // calls fail as the target navigates away
	sameWorld bool     // a resolved node is in the world
}

func (w *worldPage) handle(m cdptest.Message) (any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var p struct {
		ObjectID            string `json:"objectId"`
		FunctionDeclaration string `json:"functionDeclaration"`
	}
	_ = json.Unmarshal(m.Params, &p)
	switch m.Method {
	case "Target.attachToTarget":
		return map[string]any{"sessionId": "S1"}, nil
	case "Page.getFrameTree":
		l := w.loaders[0]
		if len(w.loaders) > 1 {
			w.loaders = w.loaders[1:]
		}
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F", "loaderId": "L-top"},
			"childFrames": []any{map[string]any{"frame": map[string]any{"id": "F2", "loaderId": l}}}}}, nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": 7}, nil
	case "Runtime.evaluate":
		return map[string]any{"result": map[string]any{"type": "object", "objectId": "global-7"}}, nil
	case "DOM.resolveNode":
		return map[string]any{"object": map[string]any{"type": "object", "subtype": "node", "objectId": "node-1"}}, nil
	case "Runtime.callFunctionOn":
		switch {
		case w.navigated:
			return nil, cdptest.Error{Message: "Inspected target navigated or closed"}
		case p.ObjectID == "global-7" && !w.handleOK:
			return nil, cdptest.Error{Message: "Could not find object with given id"}
		case p.ObjectID == "node-1" && strings.Contains(p.FunctionDeclaration, "globalThis") && !w.sameWorld:
			return nil, cdptest.Error{Message: "Argument should belong to the same JavaScript world as target object"}
		case strings.Contains(p.FunctionDeclaration, "return g === globalThis"):
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		case strings.Contains(p.FunctionDeclaration, "document.title"):
			return map[string]any{"result": map[string]any{"type": "string", "value": "Title"}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "objectId": "made-by-" + p.ObjectID}}, nil
	}
	return map[string]any{}, nil
}

func TestCodeRunsOnlyInVerifiedIsolatedWorlds(t *testing.T) {
	w := &worldPage{loaders: []string{"L1"}, handleOK: true, sameWorld: true}
	b, sock := fake(t, w.handle)
	c := dial(t, sock, cdp.Options{})
	ctx := context.Background()
	tg, err := c.Attach(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": "T1"}, nil); !errors.Is(err, cdp.ErrNotAllowed) {
		t.Errorf("attach without flatten: %v", err)
	}
	refused := func(what, method string, params map[string]any) {
		t.Helper()
		if err := tg.Call(ctx, method, params, nil); !errors.Is(err, cdp.ErrNotAllowed) || !errors.Is(err, cdp.ErrNotSent) {
			t.Errorf("%s: %v, want ErrNotAllowed", what, err)
		}
	}
	// Nothing runs in the page's own world, or by a number this
	// connection didn't make.
	refused("evaluate in the main world", "Runtime.evaluate", map[string]any{"expression": "1"})
	refused("evaluate in another context", "Runtime.evaluate", map[string]any{"expression": "this", "contextId": 1})
	refused("evaluate by unique id", "Runtime.evaluate", map[string]any{"expression": "this", "uniqueContextId": "x"})
	refused("resolveNode into the main world", "DOM.resolveNode", map[string]any{"backendNodeId": 3})
	refused("callFunctionOn a foreign object", "Runtime.callFunctionOn", map[string]any{"objectId": "page-object", "functionDeclaration": "function(){}"})
	refused("releaseObject of a foreign object", "Runtime.releaseObject", map[string]any{"objectId": "page-object"})

	world, err := tg.IsolatedWorld(ctx, "F2")
	if err != nil || world != 7 {
		t.Fatalf("IsolatedWorld: %d %v", world, err)
	}
	// Even in our own world, code never runs by number: numbers can be
	// re-pointed by a cross-process navigation.
	refused("evaluate code by number", "Runtime.evaluate", map[string]any{"expression": "document.title", "contextId": world})
	refused("evaluate by value by number", "Runtime.evaluate", map[string]any{"expression": "this", "contextId": world, "returnByValue": true})
	// The name globalThis is the page's to redefine: not even it.
	refused("evaluate globalThis by number", "Runtime.evaluate", map[string]any{"expression": "globalThis", "contextId": world})
	refused("callFunctionOn by number", "Runtime.callFunctionOn", map[string]any{"executionContextId": world, "functionDeclaration": "function(){}"})
	var title string
	if err := tg.Eval(ctx, world, "document.title", &title); err != nil || title != "Title" {
		t.Errorf("Eval: %q %v", title, err)
	}
	// A resolved node takes only the check that it's in the world first.
	if err := tg.Call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": 3, "executionContextId": world}, nil); err != nil {
		t.Fatal(err)
	}
	refused("callFunctionOn an unverified node", "Runtime.callFunctionOn", map[string]any{"objectId": "node-1", "functionDeclaration": "function(){ return this.click(); }"})
	node, err := tg.ResolveNode(ctx, world, 3)
	if err != nil || node != "node-1" {
		t.Fatalf("ResolveNode: %q %v", node, err)
	}
	if err := tg.Call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": node, "functionDeclaration": "function(){ return this; }"}, nil); err != nil {
		t.Errorf("callFunctionOn a verified node: %v", err)
	}
	// What a function in the world returns is in the world.
	if err := tg.Call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": "made-by-node-1", "functionDeclaration": "function(){}"}, nil); err != nil {
		t.Errorf("callFunctionOn an object from the world: %v", err)
	}
	if err := tg.Call(ctx, "Runtime.releaseObject", map[string]any{"objectId": node}, nil); err != nil {
		t.Errorf("releaseObject: %v", err)
	}
	// Another session's world isn't this one's.
	if err := c.Call(ctx, "S2", "Runtime.callFunctionOn", map[string]any{"objectId": node, "functionDeclaration": "function(){}"}, nil); !errors.Is(err, cdp.ErrNotAllowed) {
		t.Errorf("an object in another session: %v", err)
	}
	for _, m := range b.Received() {
		if m.Method == "Runtime.evaluate" && !strings.Contains(string(m.Params), `"expression":"this"`) {
			t.Errorf("sent %s %s", m.Method, m.Params)
		}
	}
}

func TestAWorldWhoseDocumentChangesIsGone(t *testing.T) {
	ctx := context.Background()
	// The document changes while the world is made: not kept.
	w := &worldPage{loaders: []string{"L1", "L2"}, handleOK: true, sameWorld: true}
	_, sock := fake(t, w.handle)
	tg, err := dial(t, sock, cdp.Options{}).Attach(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.IsolatedWorld(ctx, "F2"); !errors.Is(err, cdp.ErrWorldGone) {
		t.Fatalf("a world made across a navigation: %v", err)
	}
	if err := tg.Eval(ctx, 7, "document.title", nil); !errors.Is(err, cdp.ErrWorldGone) {
		t.Errorf("Eval in a world that wasn't kept: %v", err)
	}
	// Later, the document goes (a cross-process navigation): its handle
	// no longer resolves, and nodes aren't in the world.
	w.mu.Lock()
	w.loaders = []string{"L2"}
	w.mu.Unlock()
	world, err := tg.IsolatedWorld(ctx, "F2")
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.handleOK, w.sameWorld = false, false
	w.mu.Unlock()
	if err := tg.Eval(ctx, world, "document.title", nil); !errors.Is(err, cdp.ErrWorldGone) {
		t.Errorf("Eval after the document went: %v", err)
	}
	if _, err := tg.ResolveNode(ctx, world, 3); !errors.Is(err, cdp.ErrWorldGone) {
		t.Errorf("ResolveNode after the document went: %v", err)
	}
	// Chromium's other word for it, mid-navigation.
	w.mu.Lock()
	w.navigated = true
	w.mu.Unlock()
	if err := tg.Eval(ctx, world, "document.title", nil); !errors.Is(err, cdp.ErrWorldGone) {
		t.Errorf("Eval as the target navigates: %v", err)
	}
	if err := tg.Call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": "node-1", "functionDeclaration": "function(){}"}, nil); !errors.Is(err, cdp.ErrNotAllowed) {
		t.Errorf("a node that failed its check took a call: %v", err)
	}
}

func TestTooLargeAnswer(t *testing.T) {
	_, sock := fake(t, answers)
	c := dial(t, sock, cdp.Options{MaxMessage: 1024})
	err := c.Call(context.Background(), "", "Accessibility.getFullAXTree", nil, nil)
	if !errors.Is(err, cdp.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open")
	}
}

func TestUnansweredCallsEndWithTheContextOrTheConnection(t *testing.T) {
	b, sock := fake(t, func(cdptest.Message) (any, error) { return nil, cdptest.NoReply })
	c := dial(t, sock, cdp.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "", "Target.getTargets", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "", "Target.getTargets", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, cdp.ErrClosed) {
			t.Errorf("after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call outlived Close")
	}
	select {
	case <-b.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the browser never saw the connection close")
	}
}

func TestBrowserErrors(t *testing.T) {
	_, sock := fake(t, func(cdptest.Message) (any, error) { return nil, cdptest.Error{Message: "No target with given id"} })
	c := dial(t, sock, cdp.Options{})
	err := c.Call(context.Background(), "", "Target.getTargets", nil, nil)
	var ce *cdp.Error
	if !errors.As(err, &ce) || ce.Method != "Target.getTargets" || ce.Message != "No target with given id" {
		t.Fatalf("got %#v", err)
	}
}

// A caller whose context ends can still finish what it started on the
// same connection (release a pressed button): neither the dial's context
// nor a call's cuts the connection; only Close does.
func TestOnlyCloseEndsAConnection(t *testing.T) {
	slow := make(chan struct{})
	b, sock := fake(t, func(m cdptest.Message) (any, error) {
		if m.Method == "Accessibility.getFullAXTree" {
			<-slow
		}
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	c, err := cdp.Dial(ctx, sock, testList, cdp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	callCtx, cancelCall := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancelCall() }()
	if err := c.Call(callCtx, "", "Accessibility.getFullAXTree", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call: %v", err)
	}
	close(slow)
	if err := c.Call(context.Background(), "", "Target.getTargets", nil, nil); err != nil {
		t.Fatalf("after the dial's and a call's contexts ended: %v", err)
	}
	if err := c.Call(callCtx, "", "Target.getTargets", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a call with an ended context was sent: %v", err)
	}
	_ = c.Close()
	select {
	case <-b.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't close the connection")
	}
	if _, err := cdp.Dial(ctx, sock, testList, cdp.Options{}); err == nil {
		t.Error("dialled with a cancelled context")
	}
}
