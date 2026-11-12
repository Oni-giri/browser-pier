package cdp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
)

var testList = cdp.NewAllowList("test", "Target.attachToTarget", "Target.getTargets", "Page.createIsolatedWorld",
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

func TestCodeRunsOnlyInTheConnectionsIsolatedWorlds(t *testing.T) {
	b, sock := fake(t, answers)
	c := dial(t, sock, cdp.Options{})
	ctx := context.Background()
	if _, err := c.Attach(ctx, "T1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": "T1"}, nil); !errors.Is(err, cdp.ErrNotAllowed) {
		t.Errorf("attach without flatten: %v", err)
	}
	refused := func(what, method string, params map[string]any, session string) {
		t.Helper()
		if err := c.Call(ctx, session, method, params, nil); !errors.Is(err, cdp.ErrNotAllowed) {
			t.Errorf("%s: %v, want ErrNotAllowed", what, err)
		}
	}
	// The page's own world: no contextId, or one this connection didn't make.
	refused("evaluate in the main world", "Runtime.evaluate", map[string]any{"expression": "1"}, "S1")
	refused("evaluate in another context", "Runtime.evaluate", map[string]any{"expression": "1", "contextId": 1}, "S1")
	refused("evaluate by unique id", "Runtime.evaluate", map[string]any{"expression": "1", "uniqueContextId": "x"}, "S1")
	refused("resolveNode into the main world", "DOM.resolveNode", map[string]any{"backendNodeId": 3}, "S1")
	refused("callFunctionOn a foreign object", "Runtime.callFunctionOn", map[string]any{"objectId": "page-object", "functionDeclaration": "function(){}"}, "S1")
	refused("releaseObject of a foreign object", "Runtime.releaseObject", map[string]any{"objectId": "page-object"}, "S1")

	var w struct{ ExecutionContextID int64 }
	if err := c.Call(ctx, "S1", "Page.createIsolatedWorld", map[string]any{"frameId": "F"}, &w); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "S1", "Runtime.evaluate", map[string]any{"expression": "1", "contextId": w.ExecutionContextID}, nil); err != nil {
		t.Errorf("evaluate in the isolated world: %v", err)
	}
	// The same context ID in another session is another page's.
	refused("evaluate in another session", "Runtime.evaluate", map[string]any{"expression": "1", "contextId": w.ExecutionContextID}, "S2")
	if err := c.Call(ctx, "S1", "DOM.resolveNode", map[string]any{"backendNodeId": 3, "executionContextId": w.ExecutionContextID}, nil); err != nil {
		t.Errorf("resolveNode into the isolated world: %v", err)
	}
	for _, obj := range []string{"obj-S1", "node-1"} {
		if err := c.Call(ctx, "S1", "Runtime.callFunctionOn", map[string]any{"objectId": obj, "functionDeclaration": "function(){}"}, nil); err != nil {
			t.Errorf("callFunctionOn %s: %v", obj, err)
		}
		if err := c.Call(ctx, "S1", "Runtime.releaseObject", map[string]any{"objectId": obj}, nil); err != nil {
			t.Errorf("releaseObject %s: %v", obj, err)
		}
	}
	for _, m := range b.Received() {
		if m.Method == "Runtime.evaluate" && !strings.Contains(string(m.Params), `"contextId":7`) {
			t.Errorf("sent %s %s", m.Method, m.Params)
		}
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
