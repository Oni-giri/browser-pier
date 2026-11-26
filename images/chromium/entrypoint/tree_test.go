package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeObj is one accessible object on the fake bus.
type fakeObj struct {
	role     uint32
	states   uint64
	name     string
	ext      extents
	kids     []string
	parent   string
	text     string
	attrs    map[string]string
	docAttrs map[string]string
	value    *float64
	// noAttrs: GetAttributes fails.
	noAttrs bool
}

// fakeBus answers the AT-SPI read methods for a tree of fakeObjs, and
// fails the test on any other method: the reader must send nothing else.
type fakeBus struct {
	t     *testing.T
	app   string
	mu    sync.Mutex
	objs  map[string]*fakeObj
	delay time.Duration
	// at is what the hit test answers, by point.
	at map[[2]int32]string
	// textReads lists the objects whose text was read.
	textReads []string
	calls     int
	// onCall, if set, sees each call's context.
	onCall func(ctx context.Context)
	// hits counts hit tests; failHit, if set, fails the one with that
	// number (1 for the first).
	hits, failHit int
}

func show(bits ...int) uint64 {
	var s uint64 = 1<<stateVisible | 1<<stateShowing
	for _, b := range bits {
		s |= 1 << b
	}
	return s
}

func (f *fakeBus) add(path string, o *fakeObj) *fakeObj {
	if f.objs == nil {
		f.objs = map[string]*fakeObj{}
	}
	f.objs[path] = o
	if o.parent != "" {
		p := f.objs[o.parent]
		p.kids = append(p.kids, path)
	}
	return o
}

func (f *fakeBus) close() {}

func (f *fakeBus) call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	if f.onCall != nil {
		f.onCall(ctx)
	}
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if !readMethods[method] {
		f.t.Errorf("the reader sent %s, which isn't a read method", method)
		return nil, errors.New("refused")
	}
	if method == "org.freedesktop.DBus.ListNames" {
		return []any{[]string{"org.freedesktop.DBus", f.app, ":1.99"}}, nil
	}
	if dest != f.app {
		return nil, errors.New("no such name")
	}
	o, ok := f.objs[string(path)]
	if !ok {
		return nil, errors.New("unknown object")
	}
	switch method {
	case ifProperties + ".Get":
		iface, prop := args[0].(string), args[1].(string)
		switch {
		case iface == ifAccessible && prop == "Name":
			return []any{dbus.MakeVariant(o.name)}, nil
		case iface == ifAccessible && prop == "Parent":
			p := o.parent
			if p == "" {
				p = string(nullPath)
			}
			return []any{dbus.MakeVariant([]any{f.app, dbus.ObjectPath(p)})}, nil
		case iface == ifAccessible && prop == "ChildCount":
			return []any{dbus.MakeVariant(int32(len(o.kids)))}, nil
		case iface == ifValue && prop == "CurrentValue" && o.value != nil:
			return []any{dbus.MakeVariant(*o.value)}, nil
		case iface == ifText && prop == "CharacterCount":
			return []any{dbus.MakeVariant(int32(len([]rune(o.text))))}, nil
		}
		f.t.Errorf("the reader asked for %s.%s", iface, prop)
		return nil, errors.New("refused")
	case ifAccessible + ".GetChildAtIndex":
		i := int(args[0].(int32))
		if i < 0 || i >= len(o.kids) {
			return []any{[]any{f.app, nullPath}}, nil
		}
		return []any{[]any{f.app, dbus.ObjectPath(o.kids[i])}}, nil
	case ifAccessible + ".GetChildren":
		if len(o.kids) > fewKids {
			f.t.Errorf("GetChildren on %s, which has %d children: Chromium takes seconds to list that many", path, len(o.kids))
		}
		var kids [][]any
		for _, k := range o.kids {
			kids = append(kids, []any{f.app, dbus.ObjectPath(k)})
		}
		return []any{kids}, nil
	case ifAccessible + ".GetRole":
		return []any{o.role}, nil
	case ifAccessible + ".GetState":
		return []any{[]uint32{uint32(o.states), uint32(o.states >> 32)}}, nil
	case ifAccessible + ".GetAttributes":
		if o.noAttrs {
			return nil, errors.New("no attributes")
		}
		return []any{o.attrs}, nil
	case ifComponent + ".GetExtents":
		return []any{[]any{o.ext.X, o.ext.Y, o.ext.W, o.ext.H}}, nil
	case ifComponent + ".GetAccessibleAtPoint":
		f.hits++
		if f.hits == f.failHit {
			return nil, errors.New("org.freedesktop.DBus.Error.NoReply")
		}
		p := f.at[[2]int32{args[0].(int32), args[1].(int32)}]
		if p == "" {
			p = string(nullPath)
		}
		return []any{[]any{f.app, dbus.ObjectPath(p)}}, nil
	case ifText + ".GetText":
		if o.role == rolePasswordText {
			f.t.Errorf("the reader read a password field's text")
		}
		f.textReads = append(f.textReads, string(path))
		return []any{o.text}, nil
	case ifDocument + ".GetAttributes":
		return []any{o.docAttrs}, nil
	}
	return nil, fmt.Errorf("fake: %s not served", method)
}

const p = "/org/a11y/atspi/accessible/"

// browserBus is a browser window with the page's document, another tab's
// (hidden), and the omnibox's suggestions in a popup window of their own.
func browserBus(t *testing.T) *fakeBus {
	f := &fakeBus{t: t, app: ":1.0"}
	f.add(string(appRoot), &fakeObj{role: 75, name: "Chromium", states: show()})
	f.add(p+"1", &fakeObj{role: roleFrame, name: "Sign in - Chromium", states: show(), ext: extents{0, 0, 1440, 868}, parent: string(appRoot)})
	f.add(p+"2", &fakeObj{role: roleFrame, states: show(), ext: extents{99, 25, 1278, 88}, parent: string(appRoot)})
	f.add(p+"3", &fakeObj{role: roleFrame, ext: extents{99, 25, 1278, 178}, parent: string(appRoot)}) // hidden
	f.add(p+"10", &fakeObj{role: 39, states: show(), ext: extents{0, 0, 1440, 868}, parent: p + "1"})
	f.add(p+"11", &fakeObj{role: 63, states: show(), ext: extents{0, 40, 1440, 46}, parent: p + "10"})
	f.add(p+"12", &fakeObj{role: 79, name: "Address and search bar", states: show(), ext: extents{236, 51, 1075, 24}, parent: p + "11", text: "example.com/login"})
	f.add(p+"20", &fakeObj{role: 39, states: show(), ext: extents{0, 87, 1440, 781}, parent: p + "10"})
	f.add(p+"21", &fakeObj{role: 39, ext: extents{0, 87, 1440, 781}, parent: p + "20"}) // another tab
	f.add(p+"22", &fakeObj{role: roleDocumentWeb, name: "Old tab", states: 1 << stateVisible, ext: extents{0, 87, 1440, 781}, parent: p + "21"})
	f.add(p+"30", &fakeObj{role: roleDocumentWeb, name: "Sign in", states: show(12), ext: extents{0, 87, 1440, 781}, parent: p + "20",
		docAttrs: map[string]string{"URI": "https://example.com/login", "Title": "Sign in"}})
	f.add(p+"31", &fakeObj{role: roleLandmark, states: show(), ext: extents{8, 108, 1424, 81}, parent: p + "30", attrs: map[string]string{"xml-roles": "main", "tag": "main"}})
	f.add(p+"32", &fakeObj{role: 83, name: "Sign in", states: show(), ext: extents{8, 108, 1424, 38}, parent: p + "31"})
	f.add(p+"33", &fakeObj{role: roleEntry, name: "Email", states: show(7), ext: extents{50, 167, 186, 22}, parent: p + "31", text: "me@example.com",
		attrs: map[string]string{"html-input-name": "email", "id": "email", "text-input-type": "text", "tag": "input", "display": "inline-block"}})
	f.add(p+"34", &fakeObj{role: rolePasswordText, name: "Password", states: show(7), ext: extents{304, 167, 186, 22}, parent: p + "31", text: "hunter2",
		attrs: map[string]string{"html-input-name": "password", "text-input-type": "password"}})
	f.add(p+"35", &fakeObj{role: 43, name: "Sign in", states: show(), ext: extents{615, 167, 58, 22}, parent: p + "31"})
	f.add(p+"36", &fakeObj{role: 43, name: "Below the fold", states: 1 << stateVisible, ext: extents{8, 1200, 100, 22}, parent: p + "31"})
	// The omnibox's suggestions: a document in a popup window.
	f.add(p+"40", &fakeObj{role: roleDocumentWeb, name: "Omnibox Popup", states: show(), ext: extents{115, 41, 1246, 63}, parent: p + "2"})
	return f
}

func newTestReader(f *fakeBus) *treeReader {
	r := newTreeReader("unix:path=/nowhere", 1440, 900)
	r.dial = func(string) (busConn, error) { return f, nil }
	r.hitPause = time.Millisecond
	return r
}

func getTree(t *testing.T, r *treeReader, query string) (treeRead, int) {
	t.Helper()
	mux := http.NewServeMux()
	r.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tree"+query, nil))
	var out treeRead
	if rec.Code == http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q", ct)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
	}
	return out, rec.Code
}

func TestTreeReadsTheShowingDocumentOfTheBrowserWindow(t *testing.T) {
	f := browserBus(t)
	out, code := getTree(t, newTestReader(f), "")
	if code != http.StatusOK {
		t.Fatalf("GET /tree: %d", code)
	}
	if out.Doc.ID != p+"30" || out.Doc.URL != "https://example.com/login" || out.Doc.Title != "Sign in" || out.Doc.App != ":1.0" || out.Doc.Y != 87 {
		t.Fatalf("doc %+v: want the page's, not another tab's or the omnibox's", out.Doc)
	}
	if out.Cut != "" || out.ScreenWidth != 1440 || out.ScreenHeight != 900 {
		t.Fatalf("cut %q, screen %dx%d", out.Cut, out.ScreenWidth, out.ScreenHeight)
	}
	var ids []string
	for _, n := range out.Nodes {
		ids = append(ids, strings.TrimPrefix(n.ID, p))
	}
	// Document order, with each node's parent.
	if got := strings.Join(ids, " "); got != "31 32 33 34 35 36" {
		t.Fatalf("nodes %s", got)
	}
	if out.Nodes[0].Parent != -1 || out.Nodes[1].Parent != 0 || out.Nodes[0].Depth != 1 || out.Nodes[1].Depth != 2 {
		t.Fatalf("parents %+v", out.Nodes[:2])
	}
	email, pw := out.Nodes[2], out.Nodes[3]
	if email.Text != "me@example.com" || email.Attrs["html-input-name"] != "email" || email.Attrs["tag"] != "" || email.Attrs["display"] != "" {
		t.Fatalf("email %+v: want its text and only the attributes kept", email)
	}
	if pw.Text != "" || pw.Filled == nil || !*pw.Filled || pw.Attrs["text-input-type"] != "password" {
		t.Fatalf("password %+v: want filled, and no text", pw)
	}
	for _, id := range f.textReads {
		if id == p+"34" {
			t.Fatal("read the password field's text")
		}
	}
	if out.Nodes[0].Attrs["xml-roles"] != "main" || out.Nodes[4].X != 615 || out.Nodes[4].W != 58 {
		t.Fatalf("landmark %+v, button %+v", out.Nodes[0], out.Nodes[4])
	}
}

func TestTreeNeverSendsAMethodOutsideTheReadList(t *testing.T) {
	r := newTestReader(browserBus(t))
	r.conn = &fakeBus{t: t}
	for _, m := range []string{"org.a11y.atspi.Action.DoAction", "org.a11y.atspi.EditableText.SetTextContents",
		"org.a11y.atspi.Component.GrabFocus", "org.a11y.atspi.Component.ScrollTo", "org.a11y.atspi.Selection.SelectChild",
		"org.a11y.atspi.Value.SetCurrentValue", "org.freedesktop.DBus.Properties.Set"} {
		if err := r.call(context.Background(), ":1.0", p+"1", m, nil); !errors.Is(err, errNotRead) {
			t.Errorf("%s: %v, want it refused before the bus", m, err)
		}
	}
	if err := r.call(context.Background(), ":1.0", p+"1", ifProperties+".Get", []any{ifValue, "MinimumIncrement"}); !errors.Is(err, errNotRead) {
		t.Errorf("a property outside the list: %v", err)
	}
	for _, m := range ReadMethods() {
		for _, bad := range []string{"DoAction", "EditableText", "SetText", "GrabFocus", "ScrollTo", "SetCurrentValue", "Select"} {
			if strings.Contains(m, bad) {
				t.Errorf("the read list has %s", m)
			}
		}
	}
}

func TestTreeStopsAtItsLimits(t *testing.T) {
	big := func() *fakeBus {
		f := browserBus(t)
		// A list of 500 items below the main landmark, 3 deep each.
		f.add(p+"500", &fakeObj{role: 31, states: show(), ext: extents{8, 200, 100, 22}, parent: p + "31"})
		for i := range 500 {
			li := fmt.Sprintf("%s%d", p, 1000+i)
			f.add(li, &fakeObj{role: 32, states: show(), parent: p + "500"})
			f.add(li+"1", &fakeObj{role: 88, name: strings.Repeat("long name ", 300), states: show(), parent: li})
		}
		return f
	}
	for _, c := range []struct{ query, cut string }{{"?nodes=50", "nodes"}, {"?depth=2", "depth"}} {
		out, code := getTree(t, newTestReader(big()), c.query)
		if code != http.StatusOK || out.Cut != c.cut {
			t.Fatalf("%s: %d, cut %q, want %q", c.query, code, out.Cut, c.cut)
		}
		if c.cut == "nodes" && len(out.Nodes) > 50 {
			t.Fatalf("%d nodes past a limit of 50", len(out.Nodes))
		}
		for _, n := range out.Nodes {
			if c.cut == "depth" && n.Depth > 2 {
				t.Fatalf("a node at depth %d past a limit of 2", n.Depth)
			}
		}
	}
	out, _ := getTree(t, newTestReader(big()), "?string=40")
	for _, n := range out.Nodes {
		if len(n.Name) > 40 {
			t.Fatalf("a name of %d bytes past a limit of 40", len(n.Name))
		}
	}
	// Time: each call takes a while; the read answers what it has.
	f := big()
	f.delay = 2 * time.Millisecond
	start := time.Now()
	out, code := getTree(t, newTestReader(f), "?ms=300")
	if took := time.Since(start); code != http.StatusOK || out.Cut != "time" || took > 2*time.Second {
		t.Fatalf("%d, cut %q, %d nodes, took %s", code, out.Cut, len(out.Nodes), took)
	}
	if len(out.Nodes) == 0 || len(out.Nodes) > 1000 {
		t.Fatalf("%d nodes within 300 ms", len(out.Nodes))
	}
	// What is showing is read before what isn't: the button below the
	// fold, a sibling of the list, may be cut, the fields may not.
	ids := map[string]bool{}
	for _, n := range out.Nodes {
		ids[n.ID] = true
	}
	if !ids[p+"33"] || !ids[p+"35"] {
		t.Fatalf("the showing fields weren't read first: %v", ids)
	}
	// No limits at all: the whole list.
	out, _ = getTree(t, newTestReader(big()), "")
	if out.Cut != "" || len(out.Nodes) != 6+1+1000 {
		t.Fatalf("unlimited: cut %q, %d nodes", out.Cut, len(out.Nodes))
	}
}

func TestTreeTimesOutWithNothing(t *testing.T) {
	f := browserBus(t)
	f.delay = 50 * time.Millisecond
	if _, code := getTree(t, newTestReader(f), "?ms=20"); code != http.StatusGatewayTimeout {
		t.Fatalf("GET /tree: %d, want 504", code)
	}
}

func TestTreeRefusesBadLimits(t *testing.T) {
	for _, q := range []string{"?nodes=0", "?nodes=999999", "?ms=60000", "?depth=x"} {
		if _, code := getTree(t, newTestReader(browserBus(t)), q); code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, code)
		}
	}
}

func TestTreeWithNoPage(t *testing.T) {
	f := &fakeBus{t: t, app: ":1.0"}
	f.add(string(appRoot), &fakeObj{role: 75, name: "Chromium"})
	if _, code := getTree(t, newTestReader(f), ""); code != http.StatusNotFound {
		t.Fatalf("GET /tree: %d, want 404", code)
	}
}

func TestTreeBusErrorIsBadGateway(t *testing.T) {
	r := newTreeReader("unix:path=/nowhere", 1440, 900)
	r.dial = func(string) (busConn, error) { return nil, errors.New("connection refused") }
	if _, code := getTree(t, r, ""); code != http.StatusBadGateway {
		t.Fatalf("GET /tree: %d, want 502", code)
	}
}

type readKey struct{}

func TestTreeReadsOneAtATime(t *testing.T) {
	f := browserBus(t)
	f.delay = time.Millisecond
	var mu sync.Mutex
	active := map[any]bool{}
	var seen []any
	overlap := false
	f.onCall = func(ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		id := ctx.Value(readKey{})
		if len(seen) == 0 || seen[len(seen)-1] != id {
			seen = append(seen, id)
			if active[id] {
				overlap = true // came back to a read after another's calls
			}
		}
		active[id] = true
	}
	r := newTestReader(f)
	mux := http.NewServeMux()
	r.register(mux)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/tree", nil)
			req = req.WithContext(context.WithValue(req.Context(), readKey{}, i))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("read %d: %d", i, rec.Code)
			}
		}()
	}
	wg.Wait()
	if overlap || len(seen) != 2 {
		t.Fatalf("the reads interleaved: %v", seen)
	}
}

func getNode(t *testing.T, r *treeReader, doc, id string) nodeRead {
	t.Helper()
	mux := http.NewServeMux()
	r.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tree/node?app=:1.0&doc="+doc+"&id="+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tree/node: %d %s", rec.Code, rec.Body)
	}
	var out nodeRead
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTreeNodeReadsTheElementAgain(t *testing.T) {
	f := browserBus(t)
	r := newTestReader(f)
	f.at = map[[2]int32]string{{644, 178}: p + "35", {397, 178}: p + "31"}
	n := getNode(t, r, p+"30", p+"35")
	if n.Stale || n.Role != 43 || n.Name != "Sign in" || n.X != 615 || n.W != 58 || n.At != nil || n.Doc.Y != 87 {
		t.Fatalf("button: %+v", n)
	}
	// It moved since: the answer is where it is now.
	f.objs[p+"35"].ext = extents{700, 300, 58, 22}
	f.at[[2]int32{729, 311}] = p + "35"
	if n := getNode(t, r, p+"30", p+"35"); n.X != 700 || n.Y != 300 {
		t.Fatalf("moved button: %+v", n)
	}
	// Something else at its centre: the cookie banner.
	f.add(p+"37", &fakeObj{role: roleDialog, name: "Cookies", states: show(), parent: p + "30"})
	f.at[[2]int32{729, 311}] = p + "37"
	if n := getNode(t, r, p+"30", p+"35"); n.At == nil || n.At.ID != p+"37" || n.At.Name != "Cookies" || n.At.Role != roleDialog || n.At.Contains {
		t.Fatalf("covered: %+v", n.At)
	}
	// What is hit is its own ancestor: the test can't tell.
	if n := getNode(t, r, p+"30", p+"34"); n.At == nil || !n.At.Contains {
		t.Fatalf("password, hit its landmark: %+v", n.At)
	}
	// Gone, defunct, or from another document: stale.
	f.objs[p+"36"].states |= 1 << stateDefunct
	for _, c := range [][2]string{{p + "30", p + "99"}, {p + "30", p + "36"}, {p + "22", p + "35"}, {p + "30", p + "12"}} {
		if n := getNode(t, r, c[0], c[1]); !n.Stale {
			t.Errorf("%v: %+v, want stale", c, n)
		}
	}
}

// TestTreeReadsWhatIsOnScreenFirst: a long list scrolled to its top. In
// the time a read has, it reads the items on screen whole, links and
// all, before the thousands below the fold, which it asks for by index.
func TestTreeReadsWhatIsOnScreenFirst(t *testing.T) {
	f := browserBus(t)
	f.add(p+"500", &fakeObj{role: 31, states: show(), ext: extents{8, 200, 100, 22}, parent: p + "31"})
	for i := range 3000 {
		li := fmt.Sprintf("%s%d", p, 10000+i)
		st := uint64(1 << stateVisible)
		if i < 30 {
			st = show()
		}
		f.add(li, &fakeObj{role: 32, states: st, parent: p + "500"})
		f.add(li+"1", &fakeObj{role: 88, name: fmt.Sprintf("Item %d", i), states: st, parent: li})
		f.add(li+"11", &fakeObj{role: 116, name: fmt.Sprintf("Item %d", i), states: st, parent: li + "1"})
	}
	f.delay = time.Millisecond
	out, code := getTree(t, newTestReader(f), "?ms=500")
	if code != http.StatusOK || out.Cut != "time" {
		t.Fatalf("%d, cut %q, %d nodes", code, out.Cut, len(out.Nodes))
	}
	links := map[string]bool{}
	for _, n := range out.Nodes {
		if n.Role == 88 {
			links[n.Name] = true
		}
	}
	for i := range 30 {
		if !links[fmt.Sprintf("Item %d", i)] {
			t.Fatalf("item %d, on screen, wasn't read whole (%d nodes, %d links)", i, len(out.Nodes), len(links))
		}
	}
	t.Logf("%d nodes, %d links in 500 ms", len(out.Nodes), len(links))

	// A node limit makes the list's items as it reads them, not all at
	// once: the first items come whole.
	f.delay = 0
	out, _ = getTree(t, newTestReader(f), "?nodes=200")
	links = map[string]bool{}
	for _, n := range out.Nodes {
		if n.Role == 88 {
			links[n.Name] = true
		}
	}
	if out.Cut != "nodes" || !links["Item 0"] || !links["Item 29"] {
		t.Fatalf("200 nodes: cut %q, links %v", out.Cut, links)
	}
}

// TestTreeSendsWhatJudgesASliderSecret: a slider's value is printed, so
// the reader sends what the orchestrator's secret check needs, as for a
// text field: its attributes, and whether they could be read at all.
func TestTreeSendsWhatJudgesASliderSecret(t *testing.T) {
	f := browserBus(t)
	pin, vol := 4321.0, 3.0
	f.add(p+"38", &fakeObj{role: roleSlider, name: "Level", states: show(), ext: extents{8, 200, 100, 22}, parent: p + "31", value: &pin,
		attrs: map[string]string{"html-input-name": "pin", "id": "pin-dial", "tag": "input"}})
	f.add(p+"39", &fakeObj{role: roleSlider, name: "Volume", states: show(), ext: extents{8, 230, 100, 22}, parent: p + "31", value: &vol, noAttrs: true})
	r := newTestReader(f)
	mux := http.NewServeMux()
	r.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tree", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tree: %d %s", rec.Code, rec.Body)
	}
	var out treeRead
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var level, volume *treeNode
	for i := range out.Nodes {
		switch out.Nodes[i].ID {
		case p + "38":
			level = &out.Nodes[i]
		case p + "39":
			volume = &out.Nodes[i]
		}
	}
	if level == nil || level.Value == nil || *level.Value != pin || !level.AttrsRead || level.Attrs["html-input-name"] != "pin" || level.Attrs["id"] != "pin-dial" {
		t.Fatalf("slider %+v: want its value, its name and id attributes, and attrs_read", level)
	}
	if volume == nil || volume.AttrsRead || volume.Attrs != nil {
		t.Fatalf("slider whose attributes failed %+v: want attrs_read false", volume)
	}
	// The JSON the orchestrator reads (internal/visualtree reads the same
	// fields: TestReaderSliderJSON).
	if body := rec.Body.String(); !strings.Contains(body, `"attrs":{"html-input-name":"pin","id":"pin-dial"},"attrs_read":true`) {
		t.Fatalf("the slider's JSON: %s", body)
	}
}

// TestTreeNodeRefusesWhenTheHitTestFails: a hit test that fails is no
// confirmed hit: /tree/node answers an error, and nothing is clicked.
func TestTreeNodeRefusesWhenTheHitTestFails(t *testing.T) {
	for _, fail := range []int{1, 2} {
		f := browserBus(t)
		f.at = map[[2]int32]string{{644, 178}: p + "35"}
		f.failHit = fail
		r := newTestReader(f)
		mux := http.NewServeMux()
		r.register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tree/node?app=:1.0&doc="+p+"30&id="+p+"35", nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("hit test %d failed: %d %s, want an error", fail, rec.Code, rec.Body)
		}
	}
	// The hit test's pause cut short by the deadline: an error too.
	f := browserBus(t)
	f.at = map[[2]int32]string{{644, 178}: p + "35"}
	r := newTestReader(f)
	r.hitPause = time.Hour
	r.limits.Time = 200 * time.Millisecond
	mux := http.NewServeMux()
	r.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tree/node?app=:1.0&doc="+p+"30&id="+p+"35", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("the hit test timed out: %d %s, want an error", rec.Code, rec.Body)
	}
}
