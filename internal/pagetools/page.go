package pagetools

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"unicode"

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

// snapshotStyles are the computed styles asked of DOMSnapshot.captureSnapshot, in order.
var snapshotStyles = []string{"display", "visibility"}

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
		texts: map[int]string{}, check: map[int]bool{}, sel: map[int]bool{}, sub: map[int]int{}}
	for i, b := range doc.Nodes.BackendNodeID {
		d.byBack[b] = i
	}
	for li, ni := range doc.Layout.NodeIndex {
		d.layout[ni] = li
	}
	for k, ni := range doc.Nodes.InputValue.Index {
		d.values[ni] = d.str(doc.Nodes.InputValue.Value[k])
	}
	for k, ni := range doc.Nodes.TextValue.Index {
		d.texts[ni] = d.str(doc.Nodes.TextValue.Value[k])
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
	attrs := d.s.Documents[0].Nodes.Attributes
	if i >= len(attrs) {
		return "", false
	}
	a := attrs[i]
	for k := 0; k+1 < len(a); k += 2 {
		if strings.EqualFold(d.str(a[k]), key) {
			return d.str(a[k+1]), true
		}
	}
	return "", false
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
	for p := d.parent(i); p >= 0; p = d.parent(p) {
		if d.s.Documents[0].Nodes.NodeType[p] != 1 {
			continue
		}
		if disp := d.display(p); disp != "" && disp != "inline" && disp != "contents" {
			return p
		}
	}
	return 0
}

// secret reports whether field i is a password or code, as fence.Secret
// judges it from the DOM snapshot's attributes.
func (d *dom) secret(i int) bool {
	get := func(k string) string { v, _ := d.attr(i, k); return v }
	names := strings.Join([]string{get("name"), get("id"), get("aria-label"), get("placeholder")}, " ")
	return fence.Secret(get("type"), get("autocomplete"), names)
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

// errTooLarge is snapshot's answer on a page past the size cap.
var errTooLarge = &Error{Status: 422, Msg: "page too large to snapshot; use find"}

// readDOM takes the DOM snapshot of the target's top document.
func (c *call) readDOM(ctx context.Context) (*dom, error) {
	var s domSnapshot
	if err := c.renderer(ctx, c.t.slow, "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": snapshotStyles, "includeDOMRects": true}, &s); err != nil {
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
		p.items = textOnly(d, view)
		return p, nil
	}
	nodes, err := c.readAX(ctx)
	if errors.Is(err, errTooLarge) && !needAX {
		p.items = textOnly(d, view)
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	p.axRead = true
	p.items, p.groups = join(nodes, d, view)
	return p, nil
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
func join(nodes []axNode, d *dom, view [4]float64) ([]item, map[int]*group) {
	byID := make(map[string]*axNode, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	var out []item
	// blocks gathers text by block element, in order of first sight.
	type run struct {
		at   int // index in out
		text []string
	}
	runs := map[int]*run{}
	groups := map[int]*group{}
	var walk func(n *axNode, tier int, inMain, inControl bool, grp int, label int64)
	walk = func(n *axNode, tier int, inMain, inControl bool, grp int, label int64) {
		role := n.Role.String()
		if !n.Ignored && (role == "navigation" || role == "menubar" || role == "menu") && grp == 0 {
			grp = len(groups) + 1
			groups[grp] = &group{role: role, name: n.Name.String()}
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
			text := strings.Join(strings.Fields(n.Name.String()), " ")
			if !ok || text == "" {
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
				out = append(out, item{backend: ref, tier: tierOf(box), label: label != 0})
			}
			r.text = append(r.text, text)
			out[r.at].text = strings.Join(r.text, " ")
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
	return dropLabelEchoes(out), groups
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
	it := item{control: true, backend: n.Backend, role: role, name: n.Name.String(), tier: tier}
	switch role {
	case "textbox", "searchbox", "combobox", "spinbutton", "slider":
		v, ok := d.value(ni)
		if !ok {
			v = n.Value.String()
		}
		if d.secret(ni) {
			if v == "" {
				it.value = "[empty]"
			} else {
				it.value = "[filled]"
			}
		} else if v != "" || ok {
			it.value = `"` + capText(strings.Join(strings.Fields(v), " "), maxField) + `"`
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
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return capText(raw, maxField)
	}
	return u.Scheme + "://" + u.Host
}

// textOnly reads a page's text from the DOM snapshot alone, for pages too
// large for the AX tree: text nodes that are laid out, not hidden and not
// zero-size, grouped by block. Without the AX tree there are no controls
// or ARIA names, and aria-hidden text isn't known.
func textOnly(d *dom, view [4]float64) []item {
	doc := d.s.Documents[0]
	var out []item
	runs := map[int]int{}
	for i, typ := range doc.Nodes.NodeType {
		if typ != 3 {
			continue
		}
		box, ok := d.shown(i)
		if !ok {
			continue
		}
		text := strings.Join(strings.Fields(d.str(doc.Nodes.NodeValue[i])), " ")
		if text == "" {
			continue
		}
		tier := tierRest
		if intersects(box, view) {
			tier = tierViewport
		}
		b := d.block(i)
		if at, ok := runs[b]; ok && out[at].tier == tier {
			out[at].text += " " + text
			continue
		}
		runs[b] = len(out)
		out = append(out, item{backend: doc.Nodes.BackendNodeID[b], text: text, tier: tier})
	}
	return out
}

// maxField caps names and values in a line.
const maxField = 80

// capText cuts s to n characters, marking the cut with "…".
func capText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRightFunc(string(r[:n-1]), unicode.IsSpace) + "…"
}
