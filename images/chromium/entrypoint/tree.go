package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

// The tree reader (PROJ-150) reads the page of a Visual-mode browser from
// the accessibility bus (a11y.go), with no CDP: GET /tree walks the
// showing document of the browser window, and GET /tree/node reads one
// element of it again, for a click by ref. It is read-only: it calls
// only the AT-SPI methods in readMethods, never one that acts on the page
// or moves its focus (Action, EditableText, Selection, GrabFocus,
// ScrollTo…), which a page could tell from a person.
//
// It sends structured JSON: roles and states as AT-SPI's numbers, never
// the bus's names for them, and the page's own strings (names, text, the
// URL and title), capped. The orchestrator maps the numbers to its own
// words, redacts and fences; nothing here logs what it read.

// The AT-SPI interfaces read.
const (
	ifAccessible = "org.a11y.atspi.Accessible"
	ifComponent  = "org.a11y.atspi.Component"
	ifText       = "org.a11y.atspi.Text"
	ifValue      = "org.a11y.atspi.Value"
	ifDocument   = "org.a11y.atspi.Document"
	ifProperties = "org.freedesktop.DBus.Properties"
	appRoot      = dbus.ObjectPath("/org/a11y/atspi/accessible/root")
	nullPath     = dbus.ObjectPath("/org/a11y/atspi/null")
)

// readMethods is every D-Bus method the reader sends, and no more. call
// refuses anything else before it reaches the bus.
var readMethods = map[string]bool{
	"org.freedesktop.DBus.ListNames":      true,
	ifProperties + ".Get":                 true, // only readProps
	ifAccessible + ".GetChildAtIndex":     true,
	ifAccessible + ".GetChildren":         true,
	ifAccessible + ".GetRole":             true,
	ifAccessible + ".GetState":            true,
	ifAccessible + ".GetAttributes":       true,
	ifComponent + ".GetExtents":           true,
	ifComponent + ".GetAccessibleAtPoint": true,
	ifText + ".GetText":                   true,
	ifDocument + ".GetAttributes":         true,
}

// readProps is the properties the reader gets.
var readProps = map[[2]string]bool{
	{ifAccessible, "Name"}:       true,
	{ifAccessible, "Parent"}:     true,
	{ifAccessible, "ChildCount"}: true,
	{ifText, "CharacterCount"}:   true,
	{ifValue, "CurrentValue"}:    true,
}

// ReadMethods lists the methods, for docs and tests.
func ReadMethods() []string {
	var out []string
	for m := range readMethods {
		out = append(out, m)
	}
	return out
}

// AT-SPI roles the reader looks at (AtspiRole).
const (
	roleCheckBox     = 7
	roleComboBox     = 11
	roleDialog       = 16
	roleFrame        = 23
	rolePasswordText = 40
	roleProgressBar  = 42
	roleSlider       = 51
	roleSpinButton   = 52
	roleText         = 61
	roleEntry        = 79
	roleForm         = 87
	roleDocumentWeb  = 95
	roleLandmark     = 110
)

// AT-SPI states (AtspiStateType), as bits of a node's States.
const (
	stateDefunct = 6
	stateModal   = 16
	stateShowing = 25
	stateVisible = 30
)

func has(states uint64, bit int) bool { return states&(1<<bit) != 0 }

// attrKeys are the object attributes passed on, for fields and landmarks:
// the input's type, name and id, which judge a secret field the way
// fence.Secret does, and the ARIA role, which names a landmark.
var attrKeys = []string{"xml-roles", "text-input-type", "html-input-name", "id", "autocomplete", "placeholder"}

// treeLimits bound one read.
type treeLimits struct {
	Nodes  int           // nodes read
	Depth  int           // below the document
	String int           // bytes of one string
	Time   time.Duration // the whole read
}

// defaultLimits: about 20,000 nodes is what 2 s reads (see ANTI-BOT.md).
var defaultLimits = treeLimits{Nodes: 20000, Depth: 128, String: 1000, Time: 2 * time.Second}

// inFlight is how many nodes the reader reads at once: Chromium answers
// in turn, but a call waiting for the last one's answer leaves it idle.
const inFlight = 32

// treeNode is one element.
type treeNode struct {
	// ID is the object's path on the bus: AT-SPI's identity for it.
	ID     string `json:"id"`
	Parent int    `json:"parent"` // index in Nodes; -1 for the document's children
	Depth  int    `json:"depth"`
	Role   uint32 `json:"role"`
	States uint64 `json:"states"`
	Name   string `json:"name,omitempty"`
	// Text is an editable field's text. A password field's never is:
	// Filled says whether it has any.
	Text   string            `json:"text,omitempty"`
	Filled *bool             `json:"filled,omitempty"`
	Value  *float64          `json:"value,omitempty"`
	X      int32             `json:"x"`
	Y      int32             `json:"y"`
	W      int32             `json:"w"`
	H      int32             `json:"h"`
	Attrs  map[string]string `json:"attrs,omitempty"`
	// AttrsRead: Attrs is what the element has, not a read that failed.
	// The orchestrator prints no value of a field whose attributes it
	// couldn't judge (a secret is named by its id or name attribute).
	AttrsRead bool `json:"attrs_read,omitempty"`
	// Kids is how many children it has, read or not.
	Kids int `json:"kids,omitempty"`
}

// treeDoc is the document read.
type treeDoc struct {
	App   string `json:"app"` // Chromium's name on the bus
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	X     int32  `json:"x"`
	Y     int32  `json:"y"`
	W     int32  `json:"w"`
	H     int32  `json:"h"`
}

// treeRead is GET /tree's answer.
type treeRead struct {
	Doc          treeDoc    `json:"doc"`
	ScreenWidth  int        `json:"screen_width"`
	ScreenHeight int        `json:"screen_height"`
	Nodes        []treeNode `json:"nodes"`
	// Cut says why the read stopped short: nodes, depth or time; "" if
	// it read the whole document.
	Cut string `json:"cut,omitempty"`
}

// busConn is a connection to the bus: godbus's, or a test's fake.
type busConn interface {
	call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error)
	close()
}

type godbusConn struct{ c *dbus.Conn }

func (g godbusConn) call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	var c *dbus.Call
	if method == "org.freedesktop.DBus.ListNames" {
		c = g.c.BusObject().CallWithContext(ctx, method, 0)
	} else {
		c = g.c.Object(dest, path).CallWithContext(ctx, method, 0, args...)
	}
	return c.Body, c.Err
}

func (g godbusConn) close() { _ = g.c.Close() }

// treeReader serves the tree endpoints. One read runs at a time.
type treeReader struct {
	addr          string
	width, height int
	limits        treeLimits
	dial          func(addr string) (busConn, error)
	// hitPause is how long the hit test waits for the renderer's answer.
	hitPause time.Duration

	mu   sync.Mutex
	conn busConn
}

func newTreeReader(addr string, width, height int) *treeReader {
	return &treeReader{addr: addr, width: width, height: height, limits: defaultLimits, dial: dialBus, hitPause: 100 * time.Millisecond}
}

// dialBus connects to the bus, giving up after connectTimeout: a bus that
// takes the connection but never answers mustn't hold the reader.
func dialBus(addr string) (busConn, error) {
	c, err := dbus.Dial(addr)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := c.Auth(nil)
		if err == nil {
			err = c.Hello()
		}
		done <- err
	}()
	select {
	case err = <-done:
	case <-time.After(connectTimeout):
		_ = c.Close()
		// The handshake ends with the close: its error says how far it got.
		err = fmt.Errorf("no answer within %s: %w", connectTimeout, <-done)
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return godbusConn{c}, nil
}

// connectTimeout bounds connecting to the bus.
const connectTimeout = 2 * time.Second

func (r *treeReader) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /tree", r.serveTree)
	mux.HandleFunc("GET /tree/node", r.serveNode)
}

// errNotRead is a call refused before it reached the bus.
var errNotRead = errors.New("not a read method")

// call sends one method, if readMethods has it. It is the one way the
// reader reaches the bus.
func (r *treeReader) call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args []any, out ...any) error {
	if !readMethods[method] {
		return fmt.Errorf("%w: %s", errNotRead, method)
	}
	if method == ifProperties+".Get" {
		if len(args) != 2 {
			return fmt.Errorf("%w: property %v", errNotRead, args)
		}
		iface, _ := args[0].(string)
		prop, _ := args[1].(string)
		if !readProps[[2]string{iface, prop}] {
			return fmt.Errorf("%w: property %v", errNotRead, args)
		}
	}
	body, err := r.conn.call(ctx, dest, path, method, args...)
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return nil
	}
	return dbus.Store(body, out...)
}

func (r *treeReader) prop(ctx context.Context, dest string, path dbus.ObjectPath, iface, name string) (any, error) {
	var v dbus.Variant
	if err := r.call(ctx, dest, path, ifProperties+".Get", []any{iface, name}, &v); err != nil {
		return nil, err
	}
	return v.Value(), nil
}

// connect opens this read's connection to the bus, with r.mu held. Each
// read has its own (about a millisecond to open): a read cut short leaves
// calls in flight, and the next read mustn't inherit them.
func (r *treeReader) connect() error {
	c, err := r.dial(r.addr)
	if err != nil {
		return fmt.Errorf("can't reach the accessibility bus: %w", err)
	}
	r.conn = c
	return nil
}

// drop closes this read's connection, with r.mu held.
func (r *treeReader) drop() {
	if r.conn != nil {
		r.conn.close()
		r.conn = nil
	}
}

type objRef struct {
	Name string
	Path dbus.ObjectPath
}

type extents struct{ X, Y, W, H int32 }

func (r *treeReader) states(ctx context.Context, dest string, path dbus.ObjectPath) (uint64, error) {
	var s []uint32
	if err := r.call(ctx, dest, path, ifAccessible+".GetState", nil, &s); err != nil {
		return 0, err
	}
	var bits uint64
	if len(s) > 0 {
		bits = uint64(s[0])
	}
	if len(s) > 1 {
		bits |= uint64(s[1]) << 32
	}
	return bits, nil
}

func (r *treeReader) role(ctx context.Context, dest string, path dbus.ObjectPath) (uint32, error) {
	var role uint32
	err := r.call(ctx, dest, path, ifAccessible+".GetRole", nil, &role)
	return role, err
}

func (r *treeReader) children(ctx context.Context, dest string, path dbus.ObjectPath) ([]objRef, error) {
	var kids []objRef
	err := r.call(ctx, dest, path, ifAccessible+".GetChildren", nil, &kids)
	return kids, err
}

func (r *treeReader) extents(ctx context.Context, dest string, path dbus.ObjectPath) (extents, error) {
	var e extents
	err := r.call(ctx, dest, path, ifComponent+".GetExtents", []any{uint32(0)}, &e) // 0: screen coordinates
	return e, err
}

func (r *treeReader) name(ctx context.Context, dest string, path dbus.ObjectPath) (string, error) {
	v, err := r.prop(ctx, dest, path, ifAccessible, "Name")
	s, _ := v.(string)
	return s, err
}

// errNoPage: Chromium shows no page the reader can find.
var errNoPage = errors.New("no page is showing in the browser window (none on the accessibility bus)")

// findDoc finds the document of the active tab: the biggest showing
// document in the biggest showing window of Chromium. Other windows are
// Chrome's own popups (the omnibox's suggestions are a document of their
// own), and other tabs' documents aren't showing. It looks afresh each
// time: after a navigation the old document's object stays on the bus,
// still marked showing, with the old page in it.
func (r *treeReader) findDoc(ctx context.Context) (app string, doc dbus.ObjectPath, ext extents, err error) {
	if app, err = r.findApp(ctx); err != nil {
		return "", "", extents{}, err
	}
	win, err := r.mainWindow(ctx, app)
	if err != nil {
		return "", "", extents{}, err
	}
	docs, firstErr := r.showingDocs(ctx, app, win)
	if ctx.Err() != nil {
		return "", "", extents{}, ctx.Err()
	}
	var best extents
	for _, d := range docs {
		e, err := r.extents(ctx, app, d)
		if err == nil && int64(e.W)*int64(e.H) > int64(best.W)*int64(best.H) {
			doc, best = d, e
		}
	}
	if doc == "" {
		if firstErr != nil {
			return "", "", extents{}, firstErr // not "no page" when the bus failed
		}
		return "", "", extents{}, errNoPage
	}
	return app, doc, best, nil
}

// findDocAgain is findDoc, looking once more a moment later if no page
// is showing: while Chromium rebuilds a big page's tree, its document can
// be briefly unmarked.
func (r *treeReader) findDocAgain(ctx context.Context) (string, dbus.ObjectPath, extents, error) {
	app, doc, ext, err := r.findDoc(ctx)
	if errors.Is(err, errNoPage) {
		select {
		case <-ctx.Done():
			return "", "", extents{}, err
		case <-time.After(250 * time.Millisecond):
		}
		app, doc, ext, err = r.findDoc(ctx)
	}
	return app, doc, ext, err
}

// findApp finds Chromium on the bus: the connection whose application
// object answers, named Chromium if several do.
func (r *treeReader) findApp(ctx context.Context) (string, error) {
	var names []string
	if err := r.call(ctx, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus.ListNames", nil, &names); err != nil {
		return "", err
	}
	var any string
	for _, n := range names {
		if !strings.HasPrefix(n, ":") {
			continue
		}
		name, err := r.name(ctx, n, appRoot)
		if err != nil {
			continue
		}
		if name == "Chromium" {
			return n, nil
		}
		if any == "" {
			any = n
		}
	}
	if any == "" {
		return "", errors.New("chromium isn't on the accessibility bus")
	}
	return any, nil
}

// mainWindow is the biggest showing frame of the application.
func (r *treeReader) mainWindow(ctx context.Context, app string) (dbus.ObjectPath, error) {
	kids, err := r.children(ctx, app, appRoot)
	if err != nil {
		return "", err
	}
	var win dbus.ObjectPath
	var best int64
	for _, k := range kids {
		if k.Path == nullPath || k.Path == "" {
			continue
		}
		st, err := r.states(ctx, app, k.Path)
		if err != nil || !has(st, stateShowing) {
			continue
		}
		e, err := r.extents(ctx, app, k.Path)
		if err != nil {
			continue
		}
		if a := int64(e.W) * int64(e.H); a > best {
			win, best = k.Path, a
		}
	}
	if win == "" {
		return "", errNoPage
	}
	return win, nil
}

// maxChromeNodes bounds the walk through the window's own interface
// (toolbars, tabs) looking for documents.
const maxChromeNodes = 2000

// showingDocs lists the showing documents in the window, without going
// into any: hidden views and documents (other tabs') aren't descended.
func (r *treeReader) showingDocs(ctx context.Context, app string, win dbus.ObjectPath) (docs []dbus.ObjectPath, firstErr error) {
	var (
		mu      sync.Mutex
		visited int
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	level := []dbus.ObjectPath{win}
	for len(level) > 0 && ctx.Err() == nil {
		var next []dbus.ObjectPath
		var wg sync.WaitGroup
		sem := make(chan struct{}, inFlight)
		for _, p := range level {
			mu.Lock()
			visited++
			stop := visited > maxChromeNodes
			mu.Unlock()
			if stop {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				st, err := r.states(ctx, app, p)
				if err != nil {
					fail(err)
					return
				}
				if !has(st, stateShowing) || has(st, stateDefunct) {
					return
				}
				role, err := r.role(ctx, app, p)
				if err != nil {
					fail(err)
					return
				}
				if role == roleDocumentWeb {
					mu.Lock()
					docs = append(docs, p)
					mu.Unlock()
					return
				}
				kids, err := r.children(ctx, app, p)
				if err != nil {
					fail(err)
					return
				}
				mu.Lock()
				for _, k := range kids {
					if k.Path != nullPath && k.Path != "" {
						next = append(next, k.Path)
					}
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		level = next
	}
	return docs, firstErr
}

// docInfo reads a document's URL and title.
func (r *treeReader) docInfo(ctx context.Context, app string, doc dbus.ObjectPath, e extents, limit int) treeDoc {
	d := treeDoc{App: app, ID: string(doc), X: e.X, Y: e.Y, W: e.W, H: e.H}
	var attrs map[string]string
	if r.call(ctx, app, doc, ifDocument+".GetAttributes", nil, &attrs) == nil {
		d.URL, d.Title = capBytes(attrs["URI"], 4*limit), capBytes(attrs["Title"], limit)
	}
	return d
}

// capBytes cuts s to at most n bytes, on a character boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// walk is one read's state.
type walk struct {
	r      *treeReader
	ctx    context.Context
	app    string
	limits treeLimits

	mu    sync.Mutex
	cond  *sync.Cond
	nodes []walkNode
	// inView and later are what is waiting to be read, as stacks: the
	// page is read depth first, in document order, a node's children
	// before its next sibling. Showing nodes' children go on inView, the
	// others' on later, read once inView is empty: a read cut short has
	// what is on screen.
	inView, later []job
	busy          int
	cut           string
	failed        error
}

// job is a node to read, or a cursor over a big parent's children,
// which makes them one at a time as it is taken: a node made is one of
// the read's limit, so 20,000 children mustn't be made at once.
type job struct {
	node   int
	cursor bool
	next   int // the cursor's next child
	count  int // the parent's children
}

type walkNode struct {
	treeNode
	// path is the object's; "" until a child of a big parent is found by
	// its index.
	path   dbus.ObjectPath
	parent int // index in nodes, -1 for the document
	index  int // its index in its parent, when path is ""
	read   bool
	kids   []int
	// kidShowing: a child read so far was showing; offAfter: one read
	// after it wasn't, so the rest of a big parent's children are likely
	// off screen too, and wait in later.
	kidShowing, offAfter bool
}

// fewKids is how many children a node may have for GetChildren to list
// them: Chromium takes seconds to list thousands (4 s for 20,000), where
// asking for each by its index takes a fraction of a millisecond, so a
// big parent's children are asked for one by one, as the limits allow.
const fewKids = 64

func (w *walk) setCut(why string) {
	if w.cut == "" {
		w.cut = why
	}
}

// read walks the document's subtree, depth first with the showing parts
// first, inFlight nodes at a time, within the limits.
func (w *walk) read(doc dbus.ObjectPath) {
	w.cond = sync.NewCond(&w.mu)
	w.nodes = []walkNode{{treeNode: treeNode{Depth: 0}, path: doc, parent: -2}}
	w.inView = []job{{node: 0}}
	var wg sync.WaitGroup
	for range inFlight {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.worker()
		}()
	}
	// Wake the workers when time is up.
	stop := context.AfterFunc(w.ctx, func() {
		w.mu.Lock()
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	wg.Wait()
	stop()
}

// newNode makes node i's child, if the limits allow, reporting whether
// they did.
func (w *walk) newNode(i int, path dbus.ObjectPath, index int) (int, bool) {
	if w.nodes[i].Depth+1 > w.limits.Depth {
		w.setCut("depth")
		return 0, false
	}
	if len(w.nodes) >= w.limits.Nodes {
		w.setCut("nodes")
		return 0, false
	}
	j := len(w.nodes)
	w.nodes = append(w.nodes, walkNode{treeNode: treeNode{Depth: w.nodes[i].Depth + 1}, path: path, parent: i, index: index})
	w.nodes[i].kids = append(w.nodes[i].kids, j)
	return j, true
}

// take takes the next node to read, with w.mu held; ok is false when the
// walk is over. A cursor makes its next child here.
func (w *walk) take() (i int, ok bool) {
	for {
		if w.ctx.Err() != nil || w.failed != nil {
			return 0, false
		}
		var jb job
		fromView := false
		switch {
		case len(w.inView) > 0:
			jb, w.inView = w.inView[len(w.inView)-1], w.inView[:len(w.inView)-1]
			fromView = true
		case len(w.later) > 0:
			jb, w.later = w.later[len(w.later)-1], w.later[:len(w.later)-1]
		case w.busy == 0:
			w.cond.Broadcast()
			return 0, false
		default:
			w.cond.Wait()
			continue
		}
		if !jb.cursor {
			return jb.node, true
		}
		if jb.next >= jb.count {
			continue
		}
		if fromView && w.nodes[jb.node].offAfter {
			// The rest of a big parent's children, past one off
			// screen, wait with what isn't showing.
			w.later = append(w.later, jb)
			continue
		}
		j, made := w.newNode(jb.node, "", jb.next)
		if !made {
			continue
		}
		jb.next++
		stack := &w.later
		if fromView {
			stack = &w.inView
		}
		*stack = append(*stack, jb, job{node: j})
	}
}

func (w *walk) worker() {
	for {
		w.mu.Lock()
		i, ok := w.take()
		if !ok {
			w.mu.Unlock()
			return
		}
		w.busy++
		path := w.nodes[i].path
		var parentPath dbus.ObjectPath
		if path == "" {
			parentPath = w.nodes[w.nodes[i].parent].path
		}
		index := w.nodes[i].index
		w.mu.Unlock()

		var err error
		if path == "" {
			// A child of a big parent, by its index.
			var k objRef
			if err = w.r.call(w.ctx, w.app, parentPath, ifAccessible+".GetChildAtIndex", []any{i32(index)}, &k); err == nil {
				path = k.Path
				if path == nullPath || path == "" {
					path, err = "", errors.New("no such child")
				}
			}
		}
		var n treeNode
		var kids []objRef
		count := 0
		if err == nil {
			n, kids, count, err = w.r.readNode(w.ctx, w.app, path, w.limits.String)
		}

		w.mu.Lock()
		w.busy--
		switch {
		case err != nil && i == 0 && w.ctx.Err() == nil:
			w.failed = err // the document itself
		case err == nil:
			n.ID = string(path)
			n.Depth = w.nodes[i].Depth
			n.Kids = count
			w.nodes[i].path = path
			w.nodes[i].treeNode = n
			w.nodes[i].read = true
			showing := has(n.States, stateShowing)
			if pa := w.nodes[i].parent; pa >= 0 {
				if showing {
					w.nodes[pa].kidShowing = true
				} else if w.nodes[pa].kidShowing {
					w.nodes[pa].offAfter = true
				}
			}
			stack := &w.later
			if showing {
				stack = &w.inView
			}
			if kids == nil && count > 0 {
				*stack = append(*stack, job{node: i, cursor: true, count: count})
			}
			var added []int
			for _, k := range kids {
				if k.Path == nullPath || k.Path == "" {
					continue
				}
				j, made := w.newNode(i, k.Path, 0)
				if !made {
					break
				}
				added = append(added, j)
			}
			for x := len(added) - 1; x >= 0; x-- { // the first child on top
				*stack = append(*stack, job{node: added[x]})
			}
		}
		w.cond.Broadcast()
		w.mu.Unlock()
	}
}

// readNode reads one element: role, states, name, extents and children,
// and for fields and landmarks what judges them.
//
// It lists the children when there are at most fewKids, and otherwise
// only counts them (kids is nil), for the walk to ask for each by index.
func (r *treeReader) readNode(ctx context.Context, app string, path dbus.ObjectPath, limit int) (n treeNode, kids []objRef, count int, err error) {
	role, err := r.role(ctx, app, path)
	if err != nil {
		return n, nil, 0, err
	}
	n.Role = role
	if n.States, err = r.states(ctx, app, path); err != nil {
		return n, nil, 0, err
	}
	name, err := r.name(ctx, app, path)
	if err != nil {
		return n, nil, 0, err
	}
	n.Name = capBytes(name, limit)
	e, err := r.extents(ctx, app, path)
	if err == nil {
		n.X, n.Y, n.W, n.H = e.X, e.Y, e.W, e.H
	}
	v, err := r.prop(ctx, app, path, ifAccessible, "ChildCount")
	if err != nil {
		return n, nil, 0, err
	}
	c, _ := v.(int32)
	count = int(max(c, 0))
	if count > 0 && count <= fewKids {
		if kids, err = r.children(ctx, app, path); err != nil {
			return n, nil, 0, err
		}
		count = len(kids)
	}
	switch role {
	case rolePasswordText:
		// Never its text, nor its length (Chromium gives one bullet a
		// character): only whether it has any.
		filled := false
		if v, err := r.prop(ctx, app, path, ifText, "CharacterCount"); err == nil {
			c, _ := v.(int32)
			filled = c > 0
		}
		n.Filled = &filled
		n.Attrs, n.AttrsRead = r.attrs(ctx, app, path, limit)
	case roleEntry, roleText, roleComboBox, roleSpinButton:
		n.Attrs, n.AttrsRead = r.attrs(ctx, app, path, limit)
		var text string
		if r.call(ctx, app, path, ifText+".GetText", []any{int32(0), i32(limit)}, &text) == nil {
			n.Text = capBytes(text, limit)
		}
	case roleSlider:
		// Like every role whose value the orchestrator prints, its secret
		// check needs the input's name and id as much as a text field's.
		n.Attrs, n.AttrsRead = r.attrs(ctx, app, path, limit)
	case roleLandmark, roleDialog, roleForm:
		n.Attrs, n.AttrsRead = r.attrs(ctx, app, path, limit)
	}
	switch role {
	case roleSlider, roleSpinButton, roleProgressBar:
		if v, err := r.prop(ctx, app, path, ifValue, "CurrentValue"); err == nil {
			if f, ok := v.(float64); ok {
				n.Value = &f
			}
		}
	}
	return n, kids, count, nil
}

// attrs reads the object attributes in attrKeys; ok is false when they
// couldn't be read.
func (r *treeReader) attrs(ctx context.Context, app string, path dbus.ObjectPath, limit int) (_ map[string]string, ok bool) {
	var all map[string]string
	if r.call(ctx, app, path, ifAccessible+".GetAttributes", nil, &all) != nil {
		return nil, false
	}
	var out map[string]string
	for _, k := range attrKeys {
		if v, ok := all[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = capBytes(v, min(limit, 200))
		}
	}
	return out, true
}

// output lists the nodes read in document order (depth first), each
// with its parent's index there.
func (w *walk) output() []treeNode {
	var out []treeNode
	var visit func(i, parent int)
	visit = func(i, parent int) {
		n := w.nodes[i]
		if !n.read {
			return
		}
		at := parent
		if i != 0 {
			n.Parent = parent
			out = append(out, n.treeNode)
			at = len(out) - 1
		}
		for _, k := range n.kids {
			visit(k, at)
		}
	}
	visit(0, -1)
	return out
}

// limitsFrom reads limits a request may lower (tests use them).
func (r *treeReader) limitsFrom(req *http.Request) (treeLimits, error) {
	l := r.limits
	for _, f := range []struct {
		key string
		set func(int)
		max int
	}{
		{"nodes", func(n int) { l.Nodes = n }, r.limits.Nodes},
		{"depth", func(n int) { l.Depth = n }, r.limits.Depth},
		{"string", func(n int) { l.String = n }, r.limits.String},
		{"ms", func(n int) { l.Time = time.Duration(n) * time.Millisecond }, int(r.limits.Time / time.Millisecond)},
	} {
		s := req.URL.Query().Get(f.key)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > f.max {
			return l, fmt.Errorf("%s: want 1 to %d", f.key, f.max)
		}
		f.set(n)
	}
	return l, nil
}

// serveTree is GET /tree: the active tab's page, within the limits. A
// read cut off answers what it has, with cut saying why.
func (r *treeReader) serveTree(w http.ResponseWriter, req *http.Request) {
	limits, err := r.limitsFrom(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.connect(); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer r.drop()
	ctx, cancel := context.WithTimeout(req.Context(), limits.Time)
	defer cancel()
	app, doc, ext, err := r.findDocAgain(ctx)
	if err != nil {
		r.fail(w, req, err)
		return
	}
	// The URL and title first: once the time is up, Chromium may still
	// be busy with the walk's last calls.
	info := r.docInfo(ctx, app, doc, ext, limits.String)
	wk := &walk{r: r, ctx: ctx, app: app, limits: limits}
	wk.read(doc)
	if wk.failed != nil {
		r.fail(w, req, wk.failed)
		return
	}
	out := treeRead{Doc: info, ScreenWidth: r.width, ScreenHeight: r.height, Nodes: wk.output(), Cut: wk.cut}
	if ctx.Err() != nil && req.Context().Err() == nil {
		out.Cut = "time"
	}
	if len(out.Nodes) == 0 && out.Cut == "time" {
		http.Error(w, "reading the page took longer than "+limits.Time.String(), http.StatusGatewayTimeout)
		return
	}
	writeTreeJSON(w, out)
}

// fail answers a read that failed: the bus's error, which can quote the
// page (a name), goes to the orchestrator, which fences it.
func (r *treeReader) fail(w http.ResponseWriter, req *http.Request, err error) {
	switch {
	case req.Context().Err() != nil:
		return
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "reading the page took too long", http.StatusGatewayTimeout)
		return
	case errors.Is(err, errNoPage):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, "reading the accessibility bus failed: "+err.Error(), http.StatusBadGateway)
}

func writeTreeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// nodeRead is GET /tree/node's answer: one element read again, as it is
// now, for a click by ref.
type nodeRead struct {
	// Stale: the element is gone, or the document showing isn't the one
	// it was read from (the page navigated, or another tab is in front).
	Stale  bool   `json:"stale"`
	Role   uint32 `json:"role"`
	States uint64 `json:"states"`
	Name   string `json:"name,omitempty"`
	X      int32  `json:"x"`
	Y      int32  `json:"y"`
	W      int32  `json:"w"`
	H      int32  `json:"h"`
	// Doc is the document's extents now: the part of the screen that
	// shows the page.
	Doc          treeDoc `json:"doc"`
	ScreenWidth  int     `json:"screen_width"`
	ScreenHeight int     `json:"screen_height"`
	// At is what is at the element's centre, by Chromium's hit test, when
	// it isn't the element or inside it; nil when it is, or the test
	// found nothing.
	At *atPoint `json:"at,omitempty"`
}

type atPoint struct {
	ID   string `json:"id"`
	Role uint32 `json:"role"`
	Name string `json:"name,omitempty"`
	// Contains: what was hit is an ancestor of the element (a container
	// whose box holds it), so the test can't tell what covers it.
	Contains bool `json:"contains,omitempty"`
}

// maxAncestry bounds the walk up from an element to its document.
const maxAncestry = 256

// ancestors lists path and its ancestors, up to stop if it meets it (the
// last then is stop), else up to the application.
func (r *treeReader) ancestors(ctx context.Context, app string, path, stop dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	var chain []dbus.ObjectPath
	for p := path; len(chain) < maxAncestry; {
		chain = append(chain, p)
		if p == stop {
			return chain, nil
		}
		v, err := r.prop(ctx, app, p, ifAccessible, "Parent")
		if err != nil {
			return nil, err
		}
		var parent objRef
		if err := dbus.Store([]any{v}, &parent); err != nil {
			return nil, err
		}
		if parent.Path == "" || parent.Path == nullPath || parent.Path == appRoot {
			return chain, nil
		}
		p = parent.Path
	}
	return chain, nil
}

// serveNode is GET /tree/node?app=…&doc=…&id=…: the element read again.
func (r *treeReader) serveNode(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	app, docID, id := q.Get("app"), q.Get("doc"), q.Get("id")
	if app == "" || !dbus.ObjectPath(docID).IsValid() || !dbus.ObjectPath(id).IsValid() {
		http.Error(w, "want app, doc and id", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.connect(); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer r.drop()
	ctx, cancel := context.WithTimeout(req.Context(), r.limits.Time)
	defer cancel()
	nowApp, doc, docExt, err := r.findDocAgain(ctx)
	if err != nil {
		r.fail(w, req, err)
		return
	}
	out := nodeRead{Doc: treeDoc{App: nowApp, ID: string(doc), X: docExt.X, Y: docExt.Y, W: docExt.W, H: docExt.H}, ScreenWidth: r.width, ScreenHeight: r.height}
	path := dbus.ObjectPath(id)
	if nowApp != app || string(doc) != docID {
		out.Stale = true
		writeTreeJSON(w, out)
		return
	}
	st, err := r.states(ctx, app, path)
	if err != nil || has(st, stateDefunct) {
		if ctx.Err() != nil {
			r.fail(w, req, ctx.Err())
			return
		}
		out.Stale = true // gone
		writeTreeJSON(w, out)
		return
	}
	chain, err := r.ancestors(ctx, app, path, doc)
	if err != nil || len(chain) == 0 || chain[len(chain)-1] != doc {
		if ctx.Err() != nil {
			r.fail(w, req, ctx.Err())
			return
		}
		out.Stale = true // no longer in the document
		writeTreeJSON(w, out)
		return
	}
	out.States = st
	if out.Role, err = r.role(ctx, app, path); err != nil {
		r.fail(w, req, err)
		return
	}
	name, _ := r.name(ctx, app, path)
	out.Name = capBytes(name, r.limits.String)
	e, err := r.extents(ctx, app, path)
	if err != nil {
		r.fail(w, req, err)
		return
	}
	out.X, out.Y, out.W, out.H = e.X, e.Y, e.W, e.H
	if e.W > 0 && e.H > 0 {
		// A hit test that failed is no confirmed hit: the click is
		// refused rather than sent blind.
		if out.At, err = r.hit(ctx, app, doc, path, chain, e.X+e.W/2, e.Y+e.H/2); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			r.fail(w, req, fmt.Errorf("the hit test at the element's centre: %w", err))
			return
		}
	}
	writeTreeJSON(w, out)
}

// hit asks Chromium what is at (x, y) in the document and says what it
// is if it isn't the element at path or inside it. chain is the
// element's ancestry, up to the document.
//
// Chromium answers at once from what it has, and asks the renderer, whose
// answer comes later: its first answer can be a guess from boxes alone
// (wrong where boxes overlap), or what was at the last point asked about.
// So it asks twice, hitPause apart, and takes the second.
//
// An error (a call that failed or timed out, an ancestry it couldn't
// walk) is returned as one, never as a hit on the element: the caller
// must not click on a test that didn't run.
func (r *treeReader) hit(ctx context.Context, app string, doc, path dbus.ObjectPath, chain []dbus.ObjectPath, x, y int32) (*atPoint, error) {
	var got objRef
	for i := range 2 {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(r.hitPause):
			}
		}
		if err := r.call(ctx, app, doc, ifComponent+".GetAccessibleAtPoint", []any{x, y, uint32(0)}, &got); err != nil {
			return nil, err
		}
	}
	if got.Path == "" || got.Path == nullPath {
		return nil, nil
	}
	if got.Path == path {
		return nil, nil
	}
	up, err := r.ancestors(ctx, app, got.Path, path)
	if err != nil {
		return nil, err
	}
	if up[len(up)-1] == path {
		return nil, nil // inside the element
	}
	at := &atPoint{ID: string(got.Path)}
	for _, a := range chain {
		if a == got.Path {
			at.Contains = true
		}
	}
	at.Role, _ = r.role(ctx, app, got.Path)
	name, _ := r.name(ctx, app, got.Path)
	at.Name = capBytes(name, r.limits.String)
	return at, nil
}

// i32 is n as D-Bus's int32, clamped.
func i32(n int) int32 {
	return int32(max(min(n, math.MaxInt32), 0)) //nolint:gosec // clamped above
}
