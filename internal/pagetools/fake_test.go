package pagetools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/sessions/sessionstest"
)

// fnode is a node of a fake page: an element, a text or the document.
type fnode struct {
	tag     string // "#text", "#document", or an element's tag
	text    string // a text node's text
	attrs   []string
	role    string // AX role; "" for none (transparent in the AX tree)
	name    string // AX name
	ignored bool
	props   map[string]string
	box     *[4]float64 // nil: not laid out
	display string
	visib   string
	value   *string // the DOM snapshot's inputValue
	kids    []*fnode
	backend int64
	parent  *fnode
}

func box(x, y, w, h float64) *[4]float64 { return &[4]float64{x, y, w, h} }

func el(tag, role, name string, b *[4]float64, kids ...*fnode) *fnode {
	return &fnode{tag: tag, role: role, name: name, box: b, display: "block", visib: "visible", kids: kids}
}

func txt(s string, b *[4]float64) *fnode {
	return &fnode{tag: "#text", text: s, role: "StaticText", name: s, box: b, display: "inline", visib: "visible"}
}

func (n *fnode) with(f func(*fnode)) *fnode { f(n); return n }

func (n *fnode) attr(k, v string) *fnode { n.attrs = append(n.attrs, k, v); return n }

// ftab is a tab of the fake browser.
type ftab struct {
	id, url, title, opener string
	loader                 string
	doc                    *fnode
	history                []string
	at                     int
	// stuck tabs answer nothing the renderer handles, as with an alert open.
	stuck bool
	// readyAfter is how many more readyState reads say "loading".
	readyAfter int
	scroll     [2]float64
}

// fakeBrowser is a browser with a few tabs whose pages are fnode trees.
type fakeBrowser struct {
	t *testing.T
	*cdptest.Browser

	mu      sync.Mutex
	tabs    []*ftab
	front   string
	nextID  int
	worlds  map[int64]string // execution context → tab
	nextCtx int64
	// pages makes the document a navigation lands on, by URL.
	pages func(url string) *fnode
	// eval answers isolated-world scripts the fake doesn't know.
	eval func(tab *ftab, expr string) (any, error)
	// call answers methods the fake doesn't know (phase 2's DOM and Input).
	call func(tab *ftab, m cdptest.Message) (res any, handled bool, err error)
	// maxNodes makes the DOM snapshot claim this many nodes, if set.
	bigDOM int
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	fb := &fakeBrowser{t: t, worlds: map[int64]string{}, nextCtx: 100}
	fb.Browser = cdptest.New(fb.handle)
	fb.pages = func(string) *fnode { return el("#document", "RootWebArea", "", box(0, 0, 1280, 800)) }
	return fb
}

// addTab opens a tab on doc; it is in front if front.
func (fb *fakeBrowser) addTab(url string, doc *fnode, front bool) *ftab {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.nextID++
	tb := &ftab{id: fmt.Sprintf("T%d", fb.nextID), url: url, title: "Page " + url, doc: doc, history: []string{url}}
	tb.loader = fmt.Sprintf("L%d-%d", fb.nextID, time.Now().UnixNano())
	number(doc)
	fb.tabs = append(fb.tabs, tb)
	if front || fb.front == "" {
		fb.front = tb.id
	}
	return tb
}

// number gives a tree's nodes backend IDs (once) and parents.
func number(doc *fnode) {
	next := int64(1)
	var walk func(n, p *fnode)
	walk = func(n, p *fnode) {
		n.parent = p
		if n.backend == 0 {
			n.backend = next
		}
		next = max(next, n.backend) + 1
		for _, k := range n.kids {
			walk(k, n)
		}
	}
	walk(doc, nil)
}

func (fb *fakeBrowser) tab(id string) *ftab {
	for _, tb := range fb.tabs {
		if tb.id == id {
			return tb
		}
	}
	return nil
}

func (fb *fakeBrowser) bySession(sid string) *ftab {
	return fb.tab(strings.TrimPrefix(sid, "S-"))
}

// rendererMethods are answered by the page's renderer: a stuck tab
// answers none of them.
func rendererMethod(m string) bool {
	switch m {
	case "Page.getNavigationHistory", "Target.getTargets", "Target.attachToTarget", "Target.detachFromTarget",
		"Target.activateTarget", "Target.createTarget", "Target.closeTarget":
		return false
	}
	return true
}

func (fb *fakeBrowser) handle(m cdptest.Message) (any, error) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	var p map[string]any
	_ = json.Unmarshal(m.Params, &p)
	tb := fb.bySession(m.SessionID)
	if tb != nil && tb.stuck && rendererMethod(m.Method) {
		return nil, cdptest.NoReply
	}
	switch m.Method {
	case "Target.getTargets":
		var infos []map[string]any
		for _, t := range fb.tabs {
			infos = append(infos, map[string]any{"targetId": t.id, "type": "page", "url": t.url, "title": t.title, "openerId": t.opener})
		}
		infos = append(infos, map[string]any{"targetId": "W1", "type": "service_worker", "url": "https://x/sw.js"})
		return map[string]any{"targetInfos": infos}, nil
	case "Target.attachToTarget":
		id, _ := p["targetId"].(string)
		if fb.tab(id) == nil {
			return nil, cdptest.Error{Message: "No target with given id found"}
		}
		return map[string]any{"sessionId": "S-" + id}, nil
	case "Target.detachFromTarget":
		return nil, nil
	case "Target.activateTarget":
		id, _ := p["targetId"].(string)
		if fb.tab(id) == nil {
			return nil, cdptest.Error{Message: "No target with given id found"}
		}
		fb.front = id
		return nil, nil
	case "Target.createTarget":
		url, _ := p["url"].(string)
		fb.mu.Unlock()
		t := fb.addTab(url, fb.pages(url), true)
		fb.mu.Lock()
		return map[string]any{"targetId": t.id}, nil
	case "Target.closeTarget":
		id, _ := p["targetId"].(string)
		for i, t := range fb.tabs {
			if t.id == id {
				fb.tabs = append(fb.tabs[:i], fb.tabs[i+1:]...)
				if fb.front == id {
					fb.front = ""
				}
				return map[string]any{"success": true}, nil
			}
		}
		return nil, cdptest.Error{Message: "No target with given id found"}
	}
	if tb == nil {
		return nil, cdptest.Error{Message: "no session " + m.SessionID}
	}
	switch m.Method {
	case "Page.getFrameTree":
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F-" + tb.id, "loaderId": tb.loader, "url": strings.SplitN(tb.url, "#", 2)[0]}}}, nil
	case "Page.createIsolatedWorld":
		fb.nextCtx++
		fb.worlds[fb.nextCtx] = tb.id + "/" + tb.loader
		return map[string]any{"executionContextId": fb.nextCtx}, nil
	case "Runtime.evaluate":
		ctx := int64(p["contextId"].(float64))
		if fb.worlds[ctx] != tb.id+"/"+tb.loader {
			return nil, cdptest.Error{Message: "Cannot find context with specified id"}
		}
		v, err := fb.evaluate(tb, p["expression"].(string))
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
	case "DOMSnapshot.captureSnapshot":
		return fb.snapshot(tb), nil
	case "Accessibility.getFullAXTree":
		return map[string]any{"nodes": axTree(tb.doc)}, nil
	case "Page.navigate":
		url, _ := p["url"].(string)
		fb.go_(tb, url)
		tb.history = append(tb.history[:tb.at+1], url)
		tb.at = len(tb.history) - 1
		return map[string]any{"frameId": "F-" + tb.id, "loaderId": tb.loader}, nil
	case "Page.reload":
		fb.go_(tb, tb.url)
		return nil, nil
	case "Page.getNavigationHistory":
		var entries []map[string]any
		for i, u := range tb.history {
			entries = append(entries, map[string]any{"id": i + 1, "url": u})
		}
		return map[string]any{"currentIndex": tb.at, "entries": entries}, nil
	case "Page.navigateToHistoryEntry":
		i := int(p["entryId"].(float64)) - 1
		tb.at = i
		fb.go_(tb, tb.history[i])
		return nil, nil
	}
	if fb.call != nil {
		if res, ok, err := fb.call(tb, m); ok {
			return res, err
		}
	}
	return nil, cdptest.Error{Message: "fake: no " + m.Method}
}

// go_ loads url in tb: a new document.
func (fb *fakeBrowser) go_(tb *ftab, url string) {
	tb.url = url
	tb.title = "Page " + url
	tb.loader = fmt.Sprintf("L-%d", time.Now().UnixNano())
	tb.doc = fb.pages(url)
	number(tb.doc)
}

func (fb *fakeBrowser) evaluate(tb *ftab, expr string) (any, error) {
	switch {
	case strings.Contains(expr, "/*bp:info*/"):
		ready := "complete"
		if tb.readyAfter > 0 {
			tb.readyAfter--
			ready = "loading"
		}
		vis := "hidden"
		if fb.front == tb.id {
			vis = "visible"
		}
		return map[string]any{"url": tb.url, "title": tb.title, "ready": ready, "visibility": vis, "w": 1280, "h": 800, "x": tb.scroll[0], "y": tb.scroll[1]}, nil
	case strings.Contains(expr, "/*bp:visibility*/"):
		if fb.front == tb.id {
			return "visible", nil
		}
		return "hidden", nil
	}
	if fb.eval != nil {
		return fb.eval(tb, expr)
	}
	return nil, cdptest.Error{Message: "fake: unknown script " + expr}
}

// snapshot is DOMSnapshot.captureSnapshot of tb's document.
func (fb *fakeBrowser) snapshot(tb *ftab) any {
	var strs []string
	idx := map[string]int{}
	s := func(v string) int {
		if i, ok := idx[v]; ok {
			return i
		}
		idx[v] = len(strs)
		strs = append(strs, v)
		return idx[v]
	}
	var parent, typ, name, value []int
	var back []int64
	var attrs [][]int
	inputIdx, inputVal := []int{}, []int{}
	var lnode []int
	var styles [][]int
	var bounds [][4]float64
	var walk func(n *fnode, p int)
	walk = func(n *fnode, p int) {
		i := len(parent)
		parent = append(parent, p)
		switch n.tag {
		case "#document":
			typ = append(typ, 9)
			name = append(name, s("#document"))
			value = append(value, -1)
		case "#text":
			typ = append(typ, 3)
			name = append(name, s("#text"))
			value = append(value, s(n.text))
		default:
			typ = append(typ, 1)
			name = append(name, s(strings.ToUpper(n.tag)))
			value = append(value, -1)
		}
		back = append(back, n.backend)
		var a []int
		for _, v := range n.attrs {
			a = append(a, s(v))
		}
		attrs = append(attrs, a)
		if n.value != nil {
			inputIdx = append(inputIdx, i)
			inputVal = append(inputVal, s(*n.value))
		}
		if n.box != nil {
			lnode = append(lnode, i)
			styles = append(styles, []int{s(n.display), s(n.visib)})
			bounds = append(bounds, *n.box)
		}
		for _, k := range n.kids {
			walk(k, i)
		}
	}
	walk(tb.doc, -1)
	if fb.bigDOM > 0 {
		for len(back) < fb.bigDOM {
			back = append(back, 0)
		}
	}
	return map[string]any{
		"documents": []any{map[string]any{
			"documentURL": s(tb.url), "title": s(tb.title), "frameId": s("F-" + tb.id),
			"nodes": map[string]any{"parentIndex": parent, "nodeType": typ, "nodeName": name, "nodeValue": value,
				"backendNodeId": back, "attributes": attrs, "inputValue": map[string]any{"index": inputIdx, "value": inputVal}},
			"layout":        map[string]any{"nodeIndex": lnode, "styles": styles, "bounds": bounds},
			"scrollOffsetX": tb.scroll[0], "scrollOffsetY": tb.scroll[1],
		}},
		"strings": strs,
	}
}

// axTree is Accessibility.getFullAXTree of a document: one AX node per
// fnode with a role, nodes without one being transparent.
func axTree(doc *fnode) []map[string]any {
	var out []map[string]any
	var walk func(n *fnode, parent string) []string
	walk = func(n *fnode, parent string) []string {
		if n.role == "" {
			var ids []string
			for _, k := range n.kids {
				ids = append(ids, walk(k, parent)...)
			}
			return ids
		}
		id := fmt.Sprint(n.backend)
		node := map[string]any{"nodeId": id, "ignored": n.ignored, "role": map[string]any{"type": "role", "value": n.role},
			"name": map[string]any{"type": "computedString", "value": n.name}, "backendDOMNodeId": n.backend}
		if parent != "" {
			node["parentId"] = parent
		}
		var props []map[string]any
		for k, v := range n.props {
			props = append(props, map[string]any{"name": k, "value": map[string]any{"type": "string", "value": v}})
		}
		node["properties"] = props
		out = append(out, node)
		var kids []string
		for _, k := range n.kids {
			kids = append(kids, walk(k, id)...)
		}
		node["childIds"] = kids
		return []string{id}
	}
	walk(doc, "")
	return out
}

// fixture is a running API-mode session whose browser is a fakeBrowser.
type fixture struct {
	t     *testing.T
	m     *sessions.Manager
	tools *Tools
	fb    *fakeBrowser
	sess  sessions.Session
	who   Caller
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := db.Open(context.Background(), filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	fb := newFakeBrowser(t)
	rt := sessionstest.New(dir)
	rt.Control = fb.Handler(nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	for _, name := range []string{"web", "shot"} {
		p := profiles.New(name)
		p.DefaultMode = profiles.ModeAPI
		if name == "shot" {
			p.DefaultMode = profiles.ModeVisual
		}
		if _, err := m.Profiles().Create(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	s, _, err := m.Start(context.Background(), "web", "")
	if err != nil {
		t.Fatal(err)
	}
	tools := New(m, log)
	tools.quick, tools.slow, tools.poll, tools.settle = 300*time.Millisecond, 300*time.Millisecond, 10*time.Millisecond, 0
	return &fixture{t: t, m: m, tools: tools, fb: fb, sess: s, who: Caller{TokenID: "tok", Agent: "claude"}}
}

// session is the current state of the fixture's session.
func (f *fixture) session() sessions.Session {
	s, err := f.m.Get(context.Background(), f.sess.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// closedAfter checks the tools left no connection open.
func (f *fixture) closedAfter() {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.fb.Open() > 0 {
		if time.Now().After(deadline) {
			f.t.Fatalf("%d CDP connections still open after the call", f.fb.Open())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
