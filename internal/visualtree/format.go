package visualtree

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/fence"
)

const (
	// DefaultMaxChars is a snapshot's budget unless the agent asks for
	// another; MaxMaxChars caps what it may ask (as the page tools').
	DefaultMaxChars = 4000
	MaxMaxChars     = 20000
	// maxHeader caps the URL and the title.
	maxHeader = 256
	// maxField caps names and values in a line.
	maxField = 80
	// maxRun caps one run of text.
	maxRun = 1000
	// navLimit is how many controls a navigation or menu may have before
	// it is summed up in one line (as the page tools').
	navLimit = 20
	// panelHeight is the taskbar's height at the bottom of the screen
	// (images/chromium/jwmrc): nothing under it can be clicked.
	panelHeight = 32
	// maxQuery caps a query.
	maxQuery = 200
)

// Tiers, in the order the budget is spent (as the page tools').
const (
	tierModal = iota
	tierViewport
	tierMain
	tierRest
	nTiers
)

var tierNames = [nTiers]string{"in the open dialog", "in view", "in the main content", "elsewhere"}

// item is one line: a control, or a run of text.
type item struct {
	node    int // the control's node, or the run's first
	control bool
	role    string
	name    string
	value   string
	states  []string
	text    string
	tier    int
	label   bool // the text of a <label>
	heading bool // the text of a heading
	// raw is a text run's pieces as the tree has them, until build
	// cleans them into text; rawLen is their length.
	raw    []string
	rawLen int
	group  int // the navigation or menu it is in: its node + 1; 0 for none
	// at is the centre on screen; off is set when it isn't in view.
	atX, atY int
	off      bool
	// context is the containers it is in, outermost first, as lines.
	context []int
}

// group is a navigation landmark or a menu.
type group struct {
	label    string
	controls int
}

// page is a tree read, made into lines.
type page struct {
	url, title string
	items      []item
	groups     map[int]*group
	// containers are the context lines a query shows matches under, by
	// node.
	containers map[int]string
}

// listMarker matches the marker Chromium gives a list item as text of
// its own: a bullet, or a number or letter and a dot.
var listMarker = regexp.MustCompile(`^([•◦▪‣⁃·–-]|[0-9]+[.)]|[a-zA-Z][.)]|[ivxlcdmIVXLCDM]+[.)])$`)

// clean makes page text one line: no line breaks, runs of spaces
// collapsed, no object replacement characters (Chromium's stand-in for
// an embedded object in a parent's text).
func clean(s string) string {
	s = strings.ReplaceAll(s, "￼", " ")
	return strings.Join(strings.Fields(s), " ")
}

// capText cuts s to n characters, marking the cut with "…".
func capText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// quote quotes s, escaping backslashes and quotes, so a name can't seem
// to end early and add states or a point of its own.
func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// field is a name or value as a line shows it.
func field(s string) string { return quote(capText(clean(s), maxField)) }

// secretField reports whether a field holds a secret: a password field
// by its role, or one fence.Secret judges secret from what the tree has
// of it (its input type, name and id, accessible name and placeholder;
// AT-SPI has no autocomplete attribute, a gap SPEC §8 notes).
func secretField(n Node) bool {
	if n.Role == rolePasswordText {
		return true
	}
	a := n.Attrs
	return fence.Secret(a["text-input-type"], a["autocomplete"], a["html-input-name"]+" "+a["id"]+" "+n.Name+" "+a["placeholder"])
}

// build turns a tree read into lines.
func build(t *Tree) *page {
	p := &page{url: t.Doc.URL, title: t.Doc.Title, groups: map[int]*group{}, containers: map[int]string{}}
	nodes := t.Nodes
	n := len(nodes)
	hidden := make([]bool, n)
	inControl := make([]int, n) // the control a node is in, or -1
	inModal := make([]bool, n)
	inMain := make([]bool, n)
	groupOf := make([]int, n)
	ctx := make([][]int, n)
	parentOK := func(i int) bool { p := nodes[i].Parent; return p >= 0 && p < i }
	// The part of the screen a click can reach: the page's area above
	// the taskbar.
	vx0, vy0 := max(t.Doc.X, 0), max(t.Doc.Y, 0)
	vx1, vy1 := t.Doc.X+t.Doc.W, t.Doc.Y+t.Doc.H
	if t.ScreenWidth > 0 {
		vx1 = min(vx1, t.ScreenWidth)
	}
	if t.ScreenHeight > 0 {
		vy1 = min(vy1, t.ScreenHeight-panelHeight)
	}
	inView := func(nd Node) (int, int, bool) {
		x, y := nd.X+nd.W/2, nd.Y+nd.H/2
		return x, y, has(nd.States, stateShowing) && nd.W > 0 && nd.H > 0 && x >= vx0 && x < vx1 && y >= vy0 && y < vy1
	}
	var runs = map[int]int{} // parent node → the item of its open text run
	for i, nd := range nodes {
		inControl[i] = -1
		if parentOK(i) {
			pi := nd.Parent
			hidden[i] = hidden[pi]
			inControl[i] = inControl[pi]
			if _, ok := controlRoles[nodes[pi].Role]; ok {
				inControl[i] = pi
			}
			inModal[i] = inModal[pi] || has(nodes[pi].States, stateModal) && (nodes[pi].Role == roleDialog || nodes[pi].Role == roleAlert)
			inMain[i] = inMain[pi] || nodes[pi].Role == roleLandmark && nodes[pi].Attrs["xml-roles"] == "main"
			groupOf[i] = groupOf[pi]
			ctx[i] = ctx[pi]
			if c := containerLabel(nodes[pi]); c != "" {
				p.containers[pi] = c
				ctx[i] = append(append([]int(nil), ctx[pi]...), pi)
			}
			if isGroup(nodes[pi]) {
				groupOf[i] = pi + 1
				if p.groups[pi+1] == nil {
					p.groups[pi+1] = &group{label: containerLabel(nodes[pi])}
				}
			}
		}
		if !has(nd.States, stateVisible) || has(nd.States, stateDefunct) {
			hidden[i] = true
		}
		if hidden[i] {
			continue
		}
		tier := func(on bool) int {
			switch {
			case inModal[i]:
				return tierModal
			case on:
				return tierViewport
			case inMain[i]:
				return tierMain
			}
			return tierRest
		}
		if role, ok := controlRoles[nd.Role]; ok {
			if pc := inControl[i]; pc >= 0 && nodes[pc].Role == roleComboBox {
				continue // a <select>'s options: the combobox shows its value
			}
			x, y, on := inView(nd)
			it := item{node: i, control: true, role: role, name: nd.Name, value: value(nd, nodes, i), states: stateWords(nd.Role, nd.States),
				tier: tier(on), group: groupOf[i], atX: x, atY: y, off: !on, context: ctx[i]}
			if g := p.groups[groupOf[i]]; g != nil {
				g.controls++
			}
			p.items = append(p.items, it)
			continue
		}
		if inControl[i] >= 0 {
			continue // its text is the control's name
		}
		pi := nd.Parent
		if nd.Role == roleStatic {
			// Chromium splits a block's text where its style changes,
			// keeping the spaces in the pieces: a run joins them as they
			// are, and is cleaned once whole.
			if j, ok := runs[pi]; ok && j == len(p.items)-1 && p.items[j].rawLen < 4*maxRun {
				p.items[j].raw = append(p.items[j].raw, nd.Name)
				p.items[j].rawLen += len(nd.Name)
				continue
			}
		}
		if nd.Role == roleStatic && parentOK(i) && nodes[pi].Role == roleListItem && listMarker.MatchString(clean(nd.Name)) {
			continue // a list item's bullet or number
		}
		var raw string
		switch nd.Role {
		case roleStatic:
			raw = nd.Name
		case roleImage:
			if name := clean(nd.Name); name != "" {
				raw = "image " + quote(capText(name, maxField))
			}
		}
		if clean(raw) == "" && nd.Role != roleStatic {
			continue
		}
		_, _, on := inView(nd)
		it := item{node: i, raw: []string{raw}, rawLen: len(raw), tier: tier(on), context: ctx[i]}
		if parentOK(i) {
			switch nodes[pi].Role {
			case roleHeading:
				it.heading = true
			case roleLabel:
				it.label = true
			}
		}
		p.items = append(p.items, it)
		if nd.Role == roleStatic {
			runs[pi] = len(p.items) - 1
		}
	}
	// Runs become their text, cleaned once whole; empty ones go.
	kept := p.items[:0]
	for _, it := range p.items {
		if !it.control {
			it.text = capText(clean(strings.Join(it.raw, "")), maxRun)
			it.raw = nil
			if it.text == "" {
				continue
			}
			if it.heading {
				it.text = "# " + it.text
			} else {
				it.text = fence.TextLine(it.text) // never a control line's look-alike
			}
		}
		kept = append(kept, it)
	}
	p.items = kept
	p.items = dropLabelEchoes(p.items)
	return p
}

// value is a control's value as a line shows it.
func value(nd Node, nodes []Node, i int) string {
	switch nd.Role {
	case rolePasswordText, roleEntry, roleText, roleComboBox, roleSpinButton, roleSlider, roleProgressBar:
		// Any field that holds a value is judged first: a one-time code
		// or a card's code is often a type=number field, a spin button.
		// One whose attributes weren't read can't be judged by its id or
		// name, and counts as secret: it fails closed.
		if !nd.AttrsRead || secretField(nd) {
			filled := rawValue(nd, nodes, i) != ""
			if nd.Filled != nil {
				filled = filled || *nd.Filled
			}
			if filled {
				return "[filled]"
			}
			return "[empty]"
		}
	}
	switch nd.Role {
	case rolePasswordText, roleEntry, roleText:
		return field(nd.Text)
	case roleSlider, roleSpinButton, roleProgressBar:
		if nd.Value != nil {
			return rawValue(nd, nodes, i) // a number, unquoted
		}
	}
	if v := rawValue(nd, nodes, i); v != "" {
		return field(v)
	}
	return ""
}

// rawValue is a field's value as the tree has it, cleaned but neither
// capped nor quoted, or "" for none: a text field's text, a <select>'s
// selected option, a number's value.
func rawValue(nd Node, nodes []Node, i int) string {
	switch nd.Role {
	case rolePasswordText, roleEntry, roleText:
		return clean(nd.Text)
	case roleComboBox:
		// A <select>'s value is its selected option's name.
		for j := i + 1; j < len(nodes) && nodes[j].Depth > nd.Depth; j++ {
			if nodes[j].Role == roleMenuItem && has(nodes[j].States, stateSelected) {
				if v := clean(nodes[j].Name); v != "" {
					return v
				}
			}
		}
		return clean(nd.Text)
	case roleSlider, roleSpinButton, roleProgressBar:
		if nd.Value != nil {
			return strconv.FormatFloat(*nd.Value, 'g', 6, 64)
		}
		return clean(nd.Text)
	}
	return ""
}

// isGroup reports whether a node is a navigation or a menu, whose
// controls a long one is summed up by.
func isGroup(n Node) bool {
	switch n.Role {
	case roleMenu, roleMenuBar, rolePopupMenu, roleListBox, rolePageTabList, roleTree:
		return true
	case roleLandmark:
		return n.Attrs["xml-roles"] == "navigation"
	}
	return false
}

// landmarks are the ARIA landmark roles BrowserPier names, by the word it
// prints; any other is a "region".
var landmarks = map[string]string{"main": "main", "navigation": "navigation", "banner": "banner", "contentinfo": "contentinfo",
	"complementary": "complementary", "search": "search", "region": "region", "form": "form"}

// containerLabel is the context line of a container, or "" for a node
// that isn't one: a dialog, a landmark, a form, a list, a menu, a frame.
func containerLabel(n Node) string {
	var word string
	switch n.Role {
	case roleDialog, roleAlert:
		word = "dialog"
	case roleLandmark:
		word = landmarks[n.Attrs["xml-roles"]]
		if word == "" {
			word = "region"
		}
	case roleForm:
		word = "form"
	case roleList:
		word = "list"
	case roleMenu, roleMenuBar, rolePopupMenu:
		word = "menu"
	case roleListBox:
		word = "listbox"
	case rolePageTabList:
		word = "tablist"
	case roleTree:
		word = "tree"
	case roleDocumentWeb:
		word = "frame"
	default:
		return ""
	}
	if name := clean(n.Name); name != "" {
		return word + " " + field(name)
	}
	return word
}

// dropLabelEchoes drops a label's text that a control has as its name
// ("Email" by textbox "Email").
func dropLabelEchoes(items []item) []item {
	names := map[string]bool{}
	for _, it := range items {
		if it.control {
			names[strings.ToLower(clean(it.name))] = true
		}
	}
	out := items[:0]
	for _, it := range items {
		if it.label && names[strings.ToLower(it.text)] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// header is the URL and title lines.
func header(p *page) string {
	return "URL: " + capText(clean(p.url), maxHeader) + "\nTitle: " + capText(clean(p.title), maxHeader)
}

// line is how an item reads; ref makes a control's ref.
func line(it item, ref func(node int) string) string {
	if !it.control {
		return it.text
	}
	var b strings.Builder
	b.WriteString(ref(it.node) + " " + it.role + " " + field(it.name))
	if it.value != "" {
		b.WriteString(" = " + it.value)
	}
	for _, st := range it.states {
		b.WriteString(" [" + st + "]")
	}
	if it.off {
		b.WriteString(" [off screen]")
	} else {
		fmt.Fprintf(&b, " @%d,%d", it.atX, it.atY)
	}
	return b.String()
}

// estimate is a line's length before its ref is made: refs aren't spent
// on lines that won't be shown.
func estimate(it item) int {
	if !it.control {
		return utf8.RuneCountInString(it.text)
	}
	n := len(it.role) + utf8.RuneCountInString(capText(clean(it.name), maxField)) + utf8.RuneCountInString(it.value) + 32
	for _, st := range it.states {
		n += len(st) + 3
	}
	return n
}

// matches reports whether an item holds the query, ignoring case.
func matches(it item, q string) bool {
	if !it.control {
		return strings.Contains(strings.ToLower(it.text), q)
	}
	return strings.Contains(strings.ToLower(clean(it.name)), q) || strings.Contains(strings.ToLower(it.value), q) || strings.Contains(it.role, q)
}

// render spends the budget in tier order, controls before text in each,
// and says what didn't fit. With a query, only matching lines are kept,
// in document order, each under the containers it is in.
func render(p *page, budget int, query string, ref func(node int) string) (body, marker string) {
	var b strings.Builder
	b.WriteString(header(p))
	if budget == 0 {
		return b.String(), ""
	}
	left := budget
	omittedControls, omittedChars := 0, 0
	full := false
	count := func(it item) {
		if it.control {
			omittedControls++
		} else {
			omittedChars += utf8.RuneCountInString(it.text)
		}
	}
	add := func(lines *[]string, it item) {
		if full {
			count(it)
			return
		}
		if estimate(it)+1 > left {
			full = true
			if !it.control && left > 40 {
				// Text is cut to fit rather than dropped whole.
				l := capText(it.text, left-1)
				*lines = append(*lines, l)
				omittedChars += utf8.RuneCountInString(it.text) - utf8.RuneCountInString(l) + 1
				left = 0
				return
			}
			count(it)
			return
		}
		l := line(it, ref)
		*lines = append(*lines, l)
		left -= utf8.RuneCountInString(l) + 1
	}
	if query != "" {
		q := strings.ToLower(query)
		var lines []string
		shown := map[int]bool{}
		found := 0
		for _, it := range p.items {
			if !matches(it, q) {
				continue
			}
			found++
			for _, c := range it.context {
				if shown[c] || full {
					continue
				}
				l := "[" + p.containers[c] + "]"
				if len(l)+1 > left {
					full = true
					break
				}
				shown[c] = true
				lines = append(lines, l)
				left -= len(l) + 1
			}
			add(&lines, it)
		}
		if found == 0 {
			b.WriteString("\n(nothing on the page matches the query)")
		} else {
			b.WriteString("\n-- matching " + quote(capText(clean(query), maxField)) + " --\n")
			b.WriteString(strings.Join(lines, "\n"))
		}
	} else {
		summed := map[int]bool{}
		for tier := range nTiers {
			var lines []string
			for _, pass := range []bool{true, false} { // controls, then text
				for _, it := range p.items {
					if it.tier != tier || it.control != pass {
						continue
					}
					if g := p.groups[it.group]; g != nil && it.control && g.controls > navLimit {
						if summed[it.group] {
							continue
						}
						summed[it.group] = true
						l := summary(p, it.group, g)
						if !full && utf8.RuneCountInString(l)+1 <= left {
							lines = append(lines, l)
							left -= utf8.RuneCountInString(l) + 1
						} else {
							full = true
							omittedControls += g.controls
						}
						continue
					}
					add(&lines, it)
				}
			}
			if len(lines) > 0 {
				b.WriteString("\n-- " + tierNames[tier] + " --\n")
				b.WriteString(strings.Join(lines, "\n"))
			}
		}
	}
	if omittedControls > 0 || omittedChars > 0 {
		var parts []string
		if omittedControls > 0 {
			parts = append(parts, thousands(omittedControls)+" controls")
		}
		if omittedChars > 0 {
			parts = append(parts, thousands(omittedChars)+" characters")
		}
		marker = "…(" + strings.Join(parts, " and ") + " omitted; use query, or a larger max_chars)"
	}
	return b.String(), marker
}

// summary is the line that stands for a big navigation or menu: how many
// controls it has and the first few names.
func summary(p *page, id int, g *group) string {
	var names []string
	for _, it := range p.items {
		if it.group == id && it.control {
			names = append(names, quote(capText(clean(it.name), 30)))
			if len(names) == 5 {
				break
			}
		}
	}
	label := g.label
	if label == "" {
		label = "navigation"
	}
	return fmt.Sprintf("%s with %d controls: %s… (use query for them)", label, g.controls, strings.Join(names, ", "))
}

// thousands writes n with comma separators.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// wrap fences page text.
func wrap(source, text string) string { return fence.Wrap(source, text) }
