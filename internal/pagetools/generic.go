package pagetools

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Generic controls (PROJ-124): many pages build a control from a div or a
// span with a click handler, which has no role, so the accessibility tree
// shows it as a plain container and a snapshot would print its text with
// no ref. What gives one away is what a person sees: the pointer cursor,
// or a tabindex that puts it in the tab order. Such an element is listed
// as a control, `generic "<its text>"`, with a ref, as Playwright does:
//
//   - it has no role of its own (AX role generic or none, AX-ignored as
//     uninteresting, or left out of the AX tree, as Chromium leaves out a
//     span whose click a listener on the document handles);
//   - the pointer cursor starts on it (its computed cursor is pointer and
//     its parent's isn't), or its tabindex is 0 or more;
//   - it is shown, has rendered text, holds no shown control (a pointer
//     div around a real button is the button's), and isn't inside a
//     control or another generic: what is inside it isn't listed again.
//
// Its name is its rendered text (the AX tree's text nodes, so what is
// hidden or aria-hidden isn't in it), and it is fenced and capped as
// every name is. Roles aren't guessed from class names.

// styleCursor is cursor's place in snapshotStyles.
const styleCursor = 4

// generics are a page's generic controls.
type generics struct {
	d *dom
	// names is each generic control's text, by DOM node index; order
	// lists them in document order.
	names map[int]string
	order []int
	// inAX marks the DOM nodes the AX tree has a node for (ignored ones
	// too); ctl the nodes a shown control is at or under.
	inAX, ctl []bool
	// owner memoises prunedOwner (0: not known yet, else the owner + 2);
	// up memoises laidOutParent the same way.
	owner, up []int32
}

// style is node i's computed style k (snapshotStyles' order), "" if it
// isn't laid out.
func (d *dom) style(i, k int) string {
	li, ok := d.layout[i]
	st := d.s.Documents[0].Layout.Styles
	if !ok || li >= len(st) || k >= len(st[li]) {
		return ""
	}
	return d.str(st[li][k])
}

// tabbable reports whether element i's tabindex puts it in the tab order
// (0 or more, read as HTML reads an integer: "0x" is 0).
func (d *dom) tabbable(i int) bool {
	v, ok := d.attr(i, "tabindex")
	if !ok {
		return false
	}
	v = strings.TrimLeft(cutBytes(v, 32), " \t\n\f\r")
	neg := strings.HasPrefix(v, "-")
	if neg || strings.HasPrefix(v, "+") {
		v = v[1:]
	}
	digits := 0
	for digits < len(v) && v[digits] >= '0' && v[digits] <= '9' {
		digits++
	}
	// -0 is 0.
	return digits > 0 && (!neg || strings.Trim(v[:digits], "0") == "")
}

// laidOutParent is the nearest ancestor element of node i that is laid
// out (whose computed style the DOM snapshot has), -1 if none: an
// element's parent may have no box (display: contents).
func (g *generics) laidOutParent(i int) int {
	doc := g.d.s.Documents[0].Nodes
	var path []int
	res := -1
	for p := g.d.parent(i); p >= 0; p = g.d.parent(p) {
		if v := g.up[p]; v != 0 {
			res = int(v) - 2
			break
		}
		if _, ok := g.d.layout[p]; ok && doc.NodeType[p] == 1 {
			res = p
			break
		}
		path = append(path, p)
	}
	for _, p := range path {
		g.up[p] = int32(res + 2)
	}
	return res
}

// pointerStarts reports whether the pointer cursor starts on element i:
// its computed cursor is pointer, and its parent's isn't (children
// inherit it, and aren't controls of their own for that).
func (g *generics) pointerStarts(i int) bool {
	if g.d.style(i, styleCursor) != "pointer" {
		return false
	}
	p := g.laidOutParent(i)
	return p < 0 || g.d.style(p, styleCursor) != "pointer"
}

// candidate reports whether element i, whose AX node is n (nil: it has
// none), may be a generic control: it has no role, is shown, holds no
// shown control, and the pointer starts on it or it is in the tab order.
func (g *generics) candidate(i int, n *axNode) bool {
	if g.d.s.Documents[0].Nodes.NodeType[i] != 1 || g.ctl[i] {
		return false
	}
	if n != nil && !n.Ignored {
		if r := n.Role.String(); r != "generic" && r != "none" {
			return false
		}
	}
	if _, ok := g.d.shown(i); !ok {
		return false
	}
	return g.d.tabbable(i) || g.pointerStarts(i)
}

// prunedOwner is the generic candidate that text node t is in among the
// elements the AX tree leaves out (between t and its nearest ancestor
// the AX tree has), the outermost if several; -1 if none. Memoised, so a
// deep tree costs one walk in all.
func (g *generics) prunedOwner(t int) int {
	doc := g.d.s.Documents[0].Nodes
	var path []int
	res := -1
	for p := g.d.parent(t); p >= 0; p = g.d.parent(p) {
		if v := g.owner[p]; v != 0 {
			res = int(v) - 2
			break
		}
		if g.inAX[p] || doc.NodeType[p] != 1 {
			break
		}
		path = append(path, p)
	}
	for k := len(path) - 1; k >= 0; k-- {
		p := path[k]
		if res < 0 && g.candidate(p, nil) {
			res = p
		}
		g.owner[p] = int32(res + 2)
	}
	return res
}

// nameText gathers a generic control's text, its whitespace collapsed, a
// space between blocks, at most maxName bytes.
type nameText struct {
	b     strings.Builder
	space bool // a space is due before the next text
	block int
}

// add adds text (collapsed already), which had space before it (lead)
// or after it (trail) in the page, from the given block.
func (nt *nameText) add(text string, lead, trail bool, block int) {
	if text == "" {
		nt.space = nt.space || lead || trail
		return
	}
	if nt.b.Len() > 0 && (nt.space || lead || block != nt.block) && nt.b.Len() < maxName {
		nt.b.WriteByte(' ')
	}
	nt.block = block
	if room := maxName - nt.b.Len(); room > 0 {
		nt.b.WriteString(cutBytes(text, room))
	}
	nt.space = trail
}

// addNode adds the text of StaticText node n, whose DOM node is ni.
func (nt *nameText) addNode(d *dom, n *axNode, ni int) {
	if nt.b.Len() >= maxName {
		return
	}
	doc := d.s.Documents[0].Nodes
	var raw, text string
	if doc.NodeType[ni] == 3 {
		// Collapsed once per distinct string, however many nodes share it.
		raw, text = d.str(doc.NodeValue[ni]), d.collapsed(doc.NodeValue[ni])
	} else {
		raw = n.Name.String()
		text = collapse(cutBytes(raw, 4*maxName))
	}
	first, _ := utf8.DecodeRuneInString(raw)
	last, _ := utf8.DecodeLastRuneInString(raw)
	nt.add(text, raw != "" && unicode.IsSpace(first), raw != "" && unicode.IsSpace(last), d.block(ni))
}

// findGenerics finds a page's generic controls, walking the AX tree as
// join does.
func findGenerics(ctx context.Context, nodes []axNode, d *dom) (*generics, error) {
	size := len(d.s.Documents[0].Nodes.ParentIndex)
	g := &generics{d: d, names: map[int]string{}, inAX: make([]bool, size), ctl: make([]bool, size),
		owner: make([]int32, size), up: make([]int32, size)}
	for i := range nodes {
		n := &nodes[i]
		ni, ok := d.byBack[n.Backend]
		if n.Backend == 0 || !ok {
			continue
		}
		g.inAX[ni] = true
		if n.Ignored || !controlRoles[n.Role.String()] {
			continue
		}
		if _, shown := d.shown(ni); shown {
			for p := ni; p >= 0 && !g.ctl[p]; p = d.parent(p) {
				g.ctl[p] = true
			}
		}
	}
	texts := map[int]*nameText{}
	var found []int
	open := -1 // the DOM node of the generic the walk is in
	text := func(ni int) *nameText {
		nt := texts[ni]
		if nt == nil {
			nt = &nameText{block: -1}
			texts[ni] = nt
			found = append(found, ni)
		}
		return nt
	}
	err := axWalk(ctx, nodes, func(n *axNode) bool {
		role := n.Role.String()
		ni, hasDOM := d.byBack[n.Backend]
		switch {
		case role == "StaticText":
			if n.Ignored || !hasDOM {
				return false
			}
			if _, shown := d.shown(ni); !shown {
				return false
			}
			if open >= 0 {
				text(open).addNode(d, n, ni)
			} else if o := g.prunedOwner(ni); o >= 0 {
				text(o).addNode(d, n, ni)
			}
			return false
		case role == "LineBreak":
			if open >= 0 {
				text(open).space = true
			}
			return false
		case !n.Ignored && controlRoles[role]:
			return false // nothing inside a control is a generic, nor its text
		case open < 0 && hasDOM && g.candidate(ni, n):
			open = ni
			text(ni)
		}
		return true
	}, func(n *axNode) {
		if ni, ok := d.byBack[n.Backend]; ok && ni == open {
			open = -1
		}
	})
	if err != nil {
		return nil, err
	}
	for _, ni := range found {
		if name := strings.TrimRight(texts[ni].b.String(), " "); name != "" {
			g.names[ni] = name
			g.order = append(g.order, ni)
		}
	}
	return g, nil
}

// fitsLine reports whether a control's name shows whole in its line.
func fitsLine(name string) bool { return utf8.RuneCountInString(name) <= maxField }
