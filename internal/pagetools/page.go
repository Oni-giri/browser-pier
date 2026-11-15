package pagetools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/fence"
)

// maxDOMNodes is the size cap set by the spike (images/chromium/ANTI-BOT.md):
// a page whose DOM snapshot holds more nodes is too large for the
// accessibility tree, which grows about four times as big and much
// slower. snapshot refuses it; find reads the DOM snapshot's text alone.
const maxDOMNodes = 60000

// Tiers, in the order the snapshot's budget is spent.
const (
	tierModal = iota // inside an open modal
	tierViewport
	tierMain
	tierRest
	nTiers
)

var tierNames = [nTiers]string{"in the open dialog", "in view", "in the main content", "elsewhere"}

// item is one line of a page read: a control, an iframe marker or a run
// of text.
type item struct {
	control bool
	iframe  bool
	backend int64 // the node a ref names: the control, or the text's block
	role    string
	name    string
	value   string
	states  []string
	text    string // for text
	tier    int
	// group is the navigation or menu the control is in (0: none);
	// snapshot sums up a big one in a line (find still sees each).
	group int
	// label marks the text of a <label>; its ref is the label itself.
	label bool
}

// group is a navigation landmark or a menu.
type group struct {
	role, name string
	controls   int
}

// navLimit is how many controls a navigation or menu may have before
// snapshot sums it up in one line.
const navLimit = 20

// page is what one read of the active tab found.
type page struct {
	url, title string
	frame      string
	loader     string
	items      []item
	groups     map[int]*group
	// textCut is set when the page had more text than maxPageText.
	textCut bool
	// axRead is false when the page was too large for the AX tree: items
	// are text only, from the DOM snapshot.
	axRead bool
}

// controlRoles are the AX roles shown as controls, with a ref.
var controlRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "checkbox": true, "radio": true,
	"combobox": true, "listbox": true, "menuitem": true, "menuitemcheckbox": true, "menuitemradio": true,
	"option": true, "slider": true, "spinbutton": true, "switch": true, "tab": true, "treeitem": true,
	// Chromium's own names for some native controls.
	"DisclosureTriangle": true, "PopUpButton": true, "ToggleButton": true,
}

// snapshotStyles are the computed styles asked of DOMSnapshot.captureSnapshot, in order:
// display and visibility first, as dom.shown expects; pointer-events and
// z-index tell an iframe that clicks would land on (see markCovered).
var snapshotStyles = []string{"display", "visibility", "pointer-events", "z-index"}

type rareStrings struct {
	Index []int `json:"index"`
	Value []int `json:"value"`
}

type rareBools struct {
	Index []int `json:"index"`
}

type rareInts struct {
	Index []int   `json:"index"`
	Value []int64 `json:"value"`
}

type domSnapshot struct {
	Documents []struct {
		DocumentURL int `json:"documentURL"`
		BaseURL     int `json:"baseURL"`
		Title       int `json:"title"`
		FrameID     int `json:"frameId"`
		Nodes       struct {
			ParentIndex          []int       `json:"parentIndex"`
			NodeType             []int       `json:"nodeType"`
			NodeName             []int       `json:"nodeName"`
			NodeValue            []int       `json:"nodeValue"`
			BackendNodeID        []int64     `json:"backendNodeId"`
			Attributes           [][]int     `json:"attributes"`
			InputValue           rareStrings `json:"inputValue"`
			TextValue            rareStrings `json:"textValue"`
			InputChecked         rareBools   `json:"inputChecked"`
			OptionSelected       rareBools   `json:"optionSelected"`
			ContentDocumentIndex rareInts    `json:"contentDocumentIndex"`
		} `json:"nodes"`
		Layout struct {
			NodeIndex []int        `json:"nodeIndex"`
			Styles    [][]int      `json:"styles"`
			Bounds    [][4]float64 `json:"bounds"`
			// PaintOrders is each layout node's paint layer, in the order
			// layers are painted (includePaintOrder): nodes of one layer
			// share it.
			PaintOrders []int `json:"paintOrders"`
		} `json:"layout"`
		ScrollOffsetX float64 `json:"scrollOffsetX"`
		ScrollOffsetY float64 `json:"scrollOffsetY"`
	} `json:"documents"`
	Strings []string `json:"strings"`
}

// dom is the top document of a DOM snapshot, indexed.
type dom struct {
	s      *domSnapshot
	byBack map[int64]int // backend node ID → node index
	layout map[int]int   // node index → layout index
	values map[int]string
	texts  map[int]string // textarea values
	check  map[int]bool
	sel    map[int]bool
	sub    map[int]int // node index → content document index
	// valueIdx is a field's value as a string index (values holds it as
	// a string); text caches collapse by string index; blocks memoises
	// blockOf (0: not known yet, else the block's index + 1).
	valueIdx map[int]int
	text     map[int]string
	blocks   []int32
	// secrets caches fence.Secret's verdict by the string indices of the
	// attributes it reads: fields that share them are judged once.
	secrets map[[6]int]bool
	// secretPart caches fence.Secret's verdict on one attribute string,
	// by its kind (0 type, 1 autocomplete, 2 a name) and string index.
	secretPart map[[2]int]bool
	// cut is set once some text was cut short (collapsed).
	cut bool
}

// The DOM snapshot keeps one copy of each string, so a page can give
// thousands of nodes the same long string (a megabyte of spaces as text,
// a megabyte name attribute) in a small answer. So no per-node work may
// be proportional to a string's length: strings are cut to what the
// output can use first, text is collapsed once per distinct string, and
// the text gathered has a byte budget.
const (
	// maxAttr is how much of an attribute fence.Secret reads: enough
	// for any real label or name (its verdict is cached per set of
	// attribute strings, so this is paid once per distinct set).
	maxAttr = 8 << 10
	// maxURL is how much of an iframe's src is parsed.
	maxURL = 2048
	// maxName is how much of an AX name a control or group keeps.
	maxName = 1024
	// maxPageText caps all the text a page read keeps. One text node,
	// and one block's text, may take all of it: find searches whole
	// blocks; snapshot's budget shows less. Text is collapsed once per
	// distinct string, and the distinct strings are bounded by the
	// answer's size, so the work stays linear.
	maxPageText = 4 << 20
	maxNodeText = maxPageText
	maxRunText  = maxPageText
	// checkEvery is how many nodes a loop handles between looks at its
	// context, so a handoff or stop isn't kept waiting.
	checkEvery = 1024
)

// cutBytes keeps at most n bytes of s, at a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// collapse joins s's words with single spaces.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// collapsed is string i collapsed and cut to maxNodeText, worked out once
// per distinct string however many nodes share it. A cut sets d.cut.
func (d *dom) collapsed(i int) string {
	if t, ok := d.text[i]; ok {
		return t
	}
	c := collapse(d.str(i))
	t := cutBytes(c, maxNodeText)
	if len(t) < len(c) {
		d.cut = true
	}
	d.text[i] = t
	return t
}

func (d *dom) str(i int) string {
	if i < 0 || i >= len(d.s.Strings) {
		return ""
	}
	return d.s.Strings[i]
}

func indexDOM(s *domSnapshot) *dom {
	doc := &s.Documents[0]
	d := &dom{s: s, byBack: map[int64]int{}, layout: map[int]int{}, values: map[int]string{},
		texts: map[int]string{}, check: map[int]bool{}, sel: map[int]bool{}, sub: map[int]int{},
		valueIdx: map[int]int{}, text: map[int]string{}, blocks: make([]int32, len(doc.Nodes.ParentIndex)),
		secrets: map[[6]int]bool{}, secretPart: map[[2]int]bool{}}
	for i, b := range doc.Nodes.BackendNodeID {
		d.byBack[b] = i
	}
	for li, ni := range doc.Layout.NodeIndex {
		d.layout[ni] = li
	}
	for k, ni := range doc.Nodes.InputValue.Index {
		d.values[ni] = d.str(doc.Nodes.InputValue.Value[k])
		d.valueIdx[ni] = doc.Nodes.InputValue.Value[k]
	}
	for k, ni := range doc.Nodes.TextValue.Index {
		d.texts[ni] = d.str(doc.Nodes.TextValue.Value[k])
		if _, ok := d.valueIdx[ni]; !ok {
			d.valueIdx[ni] = doc.Nodes.TextValue.Value[k]
		}
	}
	for _, ni := range doc.Nodes.InputChecked.Index {
		d.check[ni] = true
	}
	for _, ni := range doc.Nodes.OptionSelected.Index {
		d.sel[ni] = true
	}
	for k, ni := range doc.Nodes.ContentDocumentIndex.Index {
		d.sub[ni] = int(doc.Nodes.ContentDocumentIndex.Value[k])
	}
	return d
}

func (d *dom) name(i int) string { return d.str(d.s.Documents[0].Nodes.NodeName[i]) }

func (d *dom) attr(i int, key string) (string, bool) {
	idx := d.attrIdx(i, key)
	return d.str(idx), idx >= 0
}

// attrIdx is the string index of node i's attribute key, -1 if it has none.
func (d *dom) attrIdx(i int, key string) int {
	attrs := d.s.Documents[0].Nodes.Attributes
	if i >= len(attrs) {
		return -1
	}
	a := attrs[i]
	for k := 0; k+1 < len(a); k += 2 {
		if strings.EqualFold(d.str(a[k]), key) {
			return a[k+1]
		}
	}
	return -1
}

func (d *dom) parent(i int) int { return d.s.Documents[0].Nodes.ParentIndex[i] }

// shown reports whether node i is laid out, not display:none or
// visibility:hidden, with a non-zero box; and its bounds if so.
func (d *dom) shown(i int) (box [4]float64, ok bool) {
	li, ok := d.layout[i]
	if !ok {
		return box, false // display:none, or under it
	}
	l := d.s.Documents[0].Layout
	if li < len(l.Styles) {
		st := l.Styles[li]
		if len(st) > 0 && d.str(st[0]) == "none" {
			return box, false
		}
		if len(st) > 1 {
			if v := d.str(st[1]); v == "hidden" || v == "collapse" {
				return box, false
			}
		}
	}
	if li >= len(l.Bounds) {
		return box, false
	}
	box = l.Bounds[li]
	if box[2] <= 0 || box[3] <= 0 {
		return box, false
	}
	return box, true
}

// display is node i's computed display, "" if it isn't laid out.
func (d *dom) display(i int) string {
	li, ok := d.layout[i]
	if !ok {
		return ""
	}
	if st := d.s.Documents[0].Layout.Styles; li < len(st) && len(st[li]) > 0 {
		return d.str(st[li][0])
	}
	return ""
}

// block is the nearest ancestor element of node i that isn't inline,
// which text is grouped by.
func (d *dom) block(i int) int {
	if p := d.parent(i); p >= 0 {
		return d.blockOf(p)
	}
	return 0
}

// blockOf is node i if it is a block element, else its nearest ancestor
// that is; 0 if none. Memoised, so a deep tree costs one walk in all.
func (d *dom) blockOf(i int) int {
	var path []int
	found := 0
	for p := i; p >= 0; p = d.parent(p) {
		if p < len(d.blocks) && d.blocks[p] != 0 {
			found = int(d.blocks[p]) - 1
			break
		}
		if d.s.Documents[0].Nodes.NodeType[p] == 1 {
			if disp := d.display(p); disp != "" && disp != "inline" && disp != "contents" {
				found = p
				break
			}
		}
		path = append(path, p)
	}
	for _, p := range path {
		if p < len(d.blocks) {
			d.blocks[p] = int32(found) + 1
		}
	}
	if found < len(d.blocks) && found > 0 && d.blocks[found] == 0 {
		d.blocks[found] = int32(found) + 1
	}
	return found
}

// secret reports whether field i is a password or code, as fence.Secret
// judges it from the DOM snapshot's attributes.
func (d *dom) secret(i int) bool {
	keys := [6]string{"type", "autocomplete", "name", "id", "aria-label", "placeholder"}
	var idx [6]int
	for k, key := range keys {
		idx[k] = d.attrIdx(i, key)
	}
	if v, ok := d.secrets[idx]; ok {
		return v
	}
	v := d.secretParts(idx)
	d.secrets[idx] = v
	return v
}

// junction is how much of each side of two name parts' meeting point is
// checked for a keyword split across them ("one" + "time code"); the
// longest keyword is about 20 characters.
const junction = 64

// secretParts is fence.Secret over the attributes, worked out part by
// part: each attribute string is judged once however many fields share it
// (fields often share long attributes but each has its own id), and only
// the short junctions between adjacent name parts are judged per field.
// That is fence.Secret(type, autocomplete, the names joined by spaces),
// since a name keyword either lies within one part or spans a junction.
func (d *dom) secretParts(idx [6]int) bool {
	part := func(kind, k int) bool {
		key := [2]int{kind, idx[k]}
		if v, ok := d.secretPart[key]; ok {
			return v
		}
		str := cutBytes(d.str(idx[k]), maxAttr)
		var v bool
		switch kind {
		case 0:
			v = fence.Secret(str, "", "")
		case 1:
			v = fence.Secret("", str, "")
		default:
			v = fence.Secret("", "", str)
		}
		d.secretPart[key] = v
		return v
	}
	if part(0, 0) || part(1, 1) {
		return true
	}
	var names []string
	for k := 2; k < 6; k++ {
		if idx[k] < 0 || d.str(idx[k]) == "" {
			continue
		}
		if part(2, k) {
			return true
		}
		names = append(names, cutBytes(d.str(idx[k]), maxAttr))
	}
	for k := 0; k+1 < len(names); k++ {
		a, b := names[k], names[k+1]
		tail := a
		if len(a) > junction {
			// The last junction bytes, from a character boundary.
			start := len(a) - junction
			for start < len(a) && !utf8.RuneStart(a[start]) {
				start++
			}
			tail = a[start:]
		}
		if fence.Secret("", "", tail+" "+cutBytes(b, junction)) {
			return true
		}
	}
	return false
}

// value is a field's current value from the DOM snapshot.
func (d *dom) value(i int) (string, bool) {
	if v, ok := d.values[i]; ok {
		return v, true
	}
	if v, ok := d.texts[i]; ok {
		return v, true
	}
	return "", false
}

type axValue struct {
	Value json.RawMessage `json:"value"`
}

func (v *axValue) String() string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.Value, &s) == nil {
		return s
	}
	return strings.Trim(string(v.Value), `"`)
}

type axNode struct {
	NodeID     string   `json:"nodeId"`
	Ignored    bool     `json:"ignored"`
	Role       *axValue `json:"role"`
	Name       *axValue `json:"name"`
	Value      *axValue `json:"value"`
	Properties []axProp `json:"properties"`
	ParentID   string   `json:"parentId"`
	ChildIDs   []string `json:"childIds"`
	Backend    int64    `json:"backendDOMNodeId"`
	FrameID    string   `json:"frameId"`
}

type axProp struct {
	Name  string  `json:"name"`
	Value axValue `json:"value"`
}

func (n *axNode) prop(name string) string {
	for _, p := range n.Properties {
		if p.Name == name {
			return p.Value.String()
		}
	}
	return ""
}

// errBusy: every whole-page read slot (maxReads) stayed taken for
// readWait.
var errBusy = &Error{Status: http.StatusServiceUnavailable, Msg: "BrowserPier is busy reading other large pages; try again in a few seconds"}

// readSlot takes one of the whole-page read slots (maxReads), shared by
// every session's snapshots, finds and content calls, waiting at most
// readWait; release gives it back. Hold it for as long as the page's
// answer is held in memory, rendering included.
func (c *call) readSlot(ctx context.Context) (release func(), err error) {
	wait := time.NewTimer(c.t.readWait)
	defer wait.Stop()
	select {
	case c.t.reads <- struct{}{}:
		return func() { <-c.t.reads }, nil
	case <-wait.C:
		// Other sessions' large pages have the slots: don't hold this
		// session's lease (and keep a person from taking control) for long.
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// errTooLarge is snapshot's answer on a page past the size cap.
var errTooLarge = &Error{Status: 422, Msg: "page too large to snapshot; use find"}

// readDOM takes the DOM snapshot of the target's top document.
func (c *call) readDOM(ctx context.Context) (*dom, error) {
	var s domSnapshot
	if err := c.renderer(ctx, c.t.slow, "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": snapshotStyles, "includeDOMRects": true, "includePaintOrder": true}, &s); err != nil {
		if errors.Is(err, cdp.ErrTooLarge) {
			return nil, &Error{Status: 422, Msg: "page too large to read"}
		}
		return nil, err
	}
	if len(s.Documents) == 0 || len(s.Documents[0].Nodes.BackendNodeID) == 0 {
		return nil, &Error{Status: 502, Msg: "the page has no document yet; try again"}
	}
	return indexDOM(&s), nil
}

// readAX takes the accessibility tree of the target's top frame.
func (c *call) readAX(ctx context.Context) ([]axNode, error) {
	var r struct {
		Nodes []axNode `json:"nodes"`
	}
	if err := c.renderer(ctx, c.t.slow, "Accessibility.getFullAXTree", nil, &r); err != nil {
		if errors.Is(err, cdp.ErrTooLarge) {
			return nil, errTooLarge
		}
		return nil, err
	}
	return r.Nodes, nil
}

// pageInfo is what the isolated world says about the page.
type pageInfo struct {
	URL        string  `json:"url"`
	Title      string  `json:"title"`
	Ready      string  `json:"ready"`
	Visibility string  `json:"visibility"`
	W          float64 `json:"w"`
	H          float64 `json:"h"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
}

const jsPageInfo = `/*bp:info*/ ({url: location.href, title: document.title, ready: document.readyState, visibility: document.visibilityState, w: innerWidth, h: innerHeight, x: scrollX, y: scrollY})`

func (c *call) info(ctx context.Context) (pageInfo, error) {
	var pi pageInfo
	err := c.eval(ctx, jsPageInfo, &pi)
	return pi, err
}

// read reads the active tab: the DOM snapshot first, then, unless the page
// is past the size cap, the AX tree, joined on backend node IDs.
func (c *call) read(ctx context.Context, needAX bool) (*page, error) {
	release, err := c.readSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	pi, err := c.info(ctx)
	if err != nil {
		return nil, err
	}
	d, err := c.readDOM(ctx)
	if err != nil {
		return nil, err
	}
	p := &page{url: pi.URL, title: pi.Title, frame: c.frame, loader: c.loader}
	view := [4]float64{pi.X, pi.Y, pi.W, pi.H}
	if len(d.s.Documents[0].Nodes.BackendNodeID) > maxDOMNodes {
		if needAX {
			return nil, errTooLarge
		}
		p.items, p.textCut, err = textOnly(ctx, d, view)
		return p, err
	}
	nodes, err := c.readAX(ctx)
	if errors.Is(err, errTooLarge) && !needAX {
		p.items, p.textCut, err = textOnly(ctx, d, view)
		return p, err
	}
	if err != nil {
		return nil, err
	}
	p.axRead = true
	p.items, p.groups, p.textCut, err = join(ctx, nodes, d, view)
	return p, err
}

func intersects(box, view [4]float64) bool {
	return box[0] < view[0]+view[2] && box[0]+box[2] > view[0] && box[1] < view[1]+view[3] && box[1]+box[3] > view[1]
}

// join walks the AX tree in document order and builds the page's items.
//
// Excluded: AX-ignored nodes, nodes that are display:none or
// visibility:hidden (not laid out, or so by their computed style), and
// nodes with a zero-size box. Kept: everything else, including text a
// person can't see (opacity:0, off-screen, clipped, the page's colour)
// and names from aria-label, aria-labelledby (even to a hidden node), alt,
// title and placeholder. That kept text is contained, not filtered: it is
// fenced and capped like the rest.
func join(ctx context.Context, nodes []axNode, d *dom, view [4]float64) ([]item, map[int]*group, bool, error) {
	byID := make(map[string]*axNode, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	var out []item
	runs := map[int]*run{}
	var all []*run
	budget := textBudget{left: maxPageText}
	groups := map[int]*group{}
	visited := 0
	var stop error
	var walk func(n *axNode, tier int, inMain, inControl bool, grp int, label int64)
	walk = func(n *axNode, tier int, inMain, inControl bool, grp int, label int64) {
		if visited++; visited%checkEvery == 0 && stop == nil {
			stop = ctx.Err()
		}
		if stop != nil {
			return
		}
		role := n.Role.String()
		if !n.Ignored && (role == "navigation" || role == "menubar" || role == "menu") && grp == 0 {
			grp = len(groups) + 1
			groups[grp] = &group{role: role, name: cutBytes(n.Name.String(), maxName)}
		}
		ni, hasDOM := d.byBack[n.Backend]
		if !n.Ignored && role == "LabelText" && hasDOM {
			// Its text is gathered on the label itself, which a click can
			// hit (a hidden checkbox's label, say).
			label = n.Backend
		}
		if !n.Ignored && (role == "dialog" || role == "alertdialog") && hasDOM {
			_, hasOpen := d.attr(ni, "open")
			if n.prop("modal") == "true" || (strings.EqualFold(d.name(ni), "DIALOG") && hasOpen) {
				tier = tierModal
			}
		}
		if !n.Ignored && role == "main" {
			inMain = true
		}
		tierOf := func(box [4]float64) int {
			switch {
			case tier == tierModal:
				return tierModal
			case intersects(box, view):
				return tierViewport
			case inMain:
				return tierMain
			}
			return tierRest
		}
		switch {
		case n.Ignored || !hasDOM:
		case role == "Iframe" || role == "iframe":
			if box, ok := d.shown(ni); ok {
				out = append(out, item{iframe: true, backend: n.Backend, role: "iframe", name: frameOrigin(d, ni), tier: tierOf(box)})
			}
			return // its document isn't read
		case controlRoles[role] && !inControl:
			if box, ok := d.shown(ni); ok {
				it := control(n, role, d, ni, tierOf(box))
				if grp != 0 {
					it.group = grp
					groups[grp].controls++
				}
				out = append(out, it)
			}
			// A hidden or zero-size control's content is hidden with it.
			inControl = true
		case role == "StaticText" && !inControl:
			box, ok := d.shown(ni)
			if !ok || budget.left <= 0 {
				budget.cut = budget.cut || ok
				break
			}
			name := n.Name.String()
			text := cutBytes(collapse(cutBytes(name, 4*maxNodeText)), maxNodeText)
			if len(name) > 4*maxNodeText || len(text) == maxNodeText {
				budget.cut = true
			}
			if text == "" {
				break
			}
			key, ref := d.block(ni), int64(0)
			if label != 0 {
				key, ref = -int(label), label
			} else {
				ref = d.s.Documents[0].Nodes.BackendNodeID[key]
			}
			r := runs[key]
			if r == nil || out[r.at].tier != tierOf(box) {
				r = &run{at: len(out)}
				runs[key] = r
				all = append(all, r)
				out = append(out, item{backend: ref, tier: tierOf(box), label: label != 0})
			}
			budget.add(r, text)
		}
		for _, id := range n.ChildIDs {
			if c := byID[id]; c != nil {
				walk(c, tier, inMain, inControl, grp, label)
			}
		}
	}
	for i := range nodes {
		if nodes[i].ParentID == "" {
			walk(&nodes[i], tierRest, false, false, 0, 0)
		}
	}
	if stop != nil {
		return nil, nil, false, stop
	}
	for _, r := range all {
		out[r.at].text = r.String()
	}
	out = slices.DeleteFunc(out, func(it item) bool { return !it.control && !it.iframe && it.text == "" })
	markCovered(out, d)
	return dropLabelEchoes(out), groups, budget.cut || d.cut, nil
}

// underFrame is the state of a control an iframe covers.
const underFrame = "under an iframe"

// Bounds on markCovered's work, whatever the page: it tries the iframes
// painted last, and uses the first few that clicks would land on.
const (
	maxCoverTries  = 64
	maxCoverFrames = 16
)

// markCovered marks the controls whose centre an iframe covers (a consent
// banner from another site, most often), which act can't click, from what
// the DOM snapshot already holds: boxes and paint order. Nothing more is
// asked of the page.
//
// An iframe covers a control when its box holds the control's centre and
// its paint layer comes later in paint order. Chromium numbers layers
// parent first, so a layer with a negative z-index, painted under its
// parent's content, still comes after it: an iframe with a negative
// z-index on it or an ancestor (a video behind a hero banner) is left
// out, as is one with pointer-events: none, which clicks go through, as
// act's hit test does. What this can't see (a covering iframe clipped by
// its parent's overflow, a control out of view that a fixed banner will
// cover once scrolled to) act still finds when it hit-tests.
func markCovered(items []item, d *dom) {
	l := d.s.Documents[0].Layout
	if len(l.PaintOrders) == 0 {
		return
	}
	order := func(ni int) (int, bool) {
		li, ok := d.layout[ni]
		if !ok || li >= len(l.PaintOrders) {
			return 0, false
		}
		return l.PaintOrders[li], true
	}
	type cover struct {
		ni    int
		box   [4]float64
		order int
	}
	var frames []cover
	for _, it := range items {
		if !it.iframe {
			continue
		}
		ni := d.byBack[it.backend]
		box, ok := d.shown(ni)
		o, has := order(ni)
		if ok && has {
			frames = append(frames, cover{ni, box, o})
		}
	}
	if len(frames) == 0 {
		return
	}
	slices.SortStableFunc(frames, func(a, b cover) int { return b.order - a.order })
	var covers []cover
	for k, f := range frames {
		if k >= maxCoverTries || len(covers) >= maxCoverFrames {
			break
		}
		if d.takesClicks(f.ni) {
			covers = append(covers, f)
		}
	}
	for i := range items {
		it := &items[i]
		if !it.control {
			continue
		}
		ni := d.byBack[it.backend]
		box, ok := d.shown(ni)
		o, has := order(ni)
		if !ok || !has {
			continue
		}
		x, y := box[0]+box[2]/2, box[1]+box[3]/2
		for _, f := range covers {
			if f.order > o && x >= f.box[0] && x < f.box[0]+f.box[2] && y >= f.box[1] && y < f.box[1]+f.box[3] {
				it.states = append(it.states, underFrame)
				break
			}
		}
	}
}

// takesClicks reports whether clicks on node i's box land on it, as far as
// the DOM snapshot tells: its pointer-events aren't none, and neither it
// nor an ancestor has a negative z-index (see markCovered).
func (d *dom) takesClicks(i int) bool {
	st := d.s.Documents[0].Layout.Styles
	style := func(ni, k int) string {
		li, ok := d.layout[ni]
		if !ok || li >= len(st) || k >= len(st[li]) {
			return ""
		}
		return d.str(st[li][k])
	}
	if style(i, 2) == "none" {
		return false
	}
	for p := i; p >= 0; p = d.parent(p) {
		if strings.HasPrefix(style(p, 3), "-") {
			return false
		}
	}
	return true
}

// run gathers a block's text, joined once at the end (joining as it grew
// would cost the square of its length).
type run struct {
	at int // index in the items
	b  strings.Builder
}

func (r *run) String() string { return r.b.String() }

// textBudget is what text a page read may still gather (maxPageText),
// each run at most maxRunText.
type textBudget struct {
	left int
	cut  bool // some text was left out
}

func (tb *textBudget) add(r *run, text string) {
	if r.b.Len() > 0 {
		text = " " + text
	}
	room := min(tb.left, maxRunText-r.b.Len())
	if len(text) > room {
		text, tb.cut = cutBytes(text, max(room, 0)), true
	}
	r.b.WriteString(text)
	tb.left -= len(text)
}

// dropLabelEchoes drops label text that a shown control already has as
// its name ("Email" by textbox "Email"). A label whose control isn't
// shown (a display:none checkbox under a styled one) stays: the agent
// finds it and clicks it instead.
func dropLabelEchoes(items []item) []item {
	names := map[string]bool{}
	for _, it := range items {
		if it.control {
			names[strings.ToLower(strings.Join(strings.Fields(it.name), " "))] = true
		}
	}
	return slices.DeleteFunc(items, func(it item) bool {
		return it.label && names[strings.ToLower(it.text)]
	})
}

// control makes a control's line from its AX node and DOM node.
func control(n *axNode, role string, d *dom, ni int, tier int) item {
	it := item{control: true, backend: n.Backend, role: role, name: cutBytes(n.Name.String(), maxName), tier: tier}
	switch role {
	case "textbox", "searchbox", "combobox", "spinbutton", "slider":
		v, ok := d.value(ni)
		shown := ""
		if idx, has := d.valueIdx[ni]; has {
			shown = d.collapsed(idx) // once per distinct value
		} else if !ok {
			v = n.Value.String()
			shown = collapse(cutBytes(v, 4*maxNodeText))
		}
		if d.secret(ni) {
			if v == "" {
				it.value = "[empty]"
			} else {
				it.value = "[filled]"
			}
		} else if v != "" || ok {
			it.value = `"` + capText(shown, maxField) + `"`
		}
	}
	switch n.prop("checked") {
	case "true":
		it.states = append(it.states, "checked")
	case "mixed":
		it.states = append(it.states, "mixed")
	}
	if n.prop("pressed") == "true" {
		it.states = append(it.states, "pressed")
	}
	if n.prop("selected") == "true" && role != "option" {
		it.states = append(it.states, "selected")
	}
	switch n.prop("expanded") {
	case "true":
		it.states = append(it.states, "expanded")
	case "false":
		it.states = append(it.states, "collapsed")
	}
	if n.prop("disabled") == "true" {
		it.states = append(it.states, "disabled")
	}
	return it
}

// frameOrigin is an iframe's origin: its document's, else its src's.
func frameOrigin(d *dom, ni int) string {
	raw := ""
	if di, ok := d.sub[ni]; ok && di < len(d.s.Documents) {
		raw = d.str(d.s.Documents[di].DocumentURL)
	}
	if raw == "" {
		raw, _ = d.attr(ni, "src")
	}
	return origin(cutBytes(raw, maxURL))
}

// textOnly reads a page's text from the DOM snapshot alone, for pages too
// large for the AX tree: text nodes that are laid out, not hidden and not
// zero-size, grouped by block. Without the AX tree there are no controls
// or ARIA names, and aria-hidden text isn't known.
func textOnly(ctx context.Context, d *dom, view [4]float64) ([]item, bool, error) {
	doc := d.s.Documents[0]
	var out []item
	runs := map[int]*run{}
	var all []*run
	budget := textBudget{left: maxPageText}
	for i, typ := range doc.Nodes.NodeType {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if typ != 3 {
			continue
		}
		box, ok := d.shown(i)
		if !ok {
			continue
		}
		if budget.left <= 0 {
			budget.cut = true
			break
		}
		text := d.collapsed(doc.Nodes.NodeValue[i])
		if text == "" {
			continue
		}
		tier := tierRest
		if intersects(box, view) {
			tier = tierViewport
		}
		b := d.block(i)
		r := runs[b]
		if r == nil || out[r.at].tier != tier {
			r = &run{at: len(out)}
			runs[b] = r
			all = append(all, r)
			out = append(out, item{backend: doc.Nodes.BackendNodeID[b], tier: tier})
		}
		budget.add(r, text)
	}
	for _, r := range all {
		out[r.at].text = r.String()
	}
	out = slices.DeleteFunc(out, func(it item) bool { return it.text == "" })
	return out, budget.cut || d.cut, nil
}

// maxField caps names and values in a line.
const maxField = 80

// capText cuts s to n characters, marking the cut with "…". It looks at
// no more than n+1 of them, however long s is.
func capText(s string, n int) string {
	if len(s) <= n {
		return s // n bytes are at most n characters
	}
	count, cut := 0, 0
	for i := range s {
		if count == n-1 {
			cut = i
		}
		if count == n {
			return strings.TrimRightFunc(s[:cut], unicode.IsSpace) + "…"
		}
		count++
	}
	return s
}
