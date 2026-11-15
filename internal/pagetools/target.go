package pagetools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/fence"
)

// A target names an element by what an agent already knows about it,
// instead of a ref (PROJ-123), so it can act without a snapshot first and
// after a navigation has made every ref stale:
//
//	role=button[name="Save"]   a role, and a case-insensitive part of the
//	                           accessible name (name*= too; single quotes;
//	                           a trailing "…" copied from a cut name is
//	                           ignored)
//	text=Save                  the innermost element whose rendered text
//	text="Save"                holds it (ignoring case), or is exactly it
//	… >> nth=0                 one of several matches, from 0; -1 the last
//
// A target is resolved against the accessibility tree a snapshot reads,
// with the DOM snapshot for what is shown, never with a selector run in
// the page: CSS would reach elements the snapshot hides by design. Without
// nth it must match exactly one visible element; otherwise the error lists
// candidates with refs, as a snapshot would hand them out.
type target struct {
	raw  string // as the agent gave it, for answers
	base string // raw without its nth clause
	// role is a role target's role (lowercase); "" for a text target.
	role string
	// name is a role target's name, collapsed and folded; any name when
	// hasName is false.
	name    string
	hasName bool
	// text is a text target's text, collapsed, and folded unless exact.
	text   string
	exact  bool
	nth    int
	hasNth bool
}

const (
	// maxTarget caps a target, in characters.
	maxTarget = 500
	// maxCandidates is how many matches an error lists.
	maxCandidates = 5
	// maxNth caps nth either way.
	maxNth = 10000
)

// targetForms is how the answers say what a target looks like.
const targetForms = `role=button[name="Save"], text=Save or text="Save" (exact), optionally followed by >> nth=0`

// knownRoles are the roles a role target may name: WAI-ARIA's, and
// Chromium's own names for some native elements, as the snapshot shows
// them (lowercase).
var knownRoles = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range strings.Fields(`alert alertdialog application article banner blockquote button caption cell checkbox
		code columnheader combobox comment complementary contentinfo definition deletion dialog directory document
		emphasis feed figure form generic grid gridcell group heading img image insertion link list listbox listitem
		log main mark marquee math menu menubar menuitem menuitemcheckbox menuitemradio meter navigation none note
		option paragraph presentation progressbar radio radiogroup region row rowgroup rowheader scrollbar search
		searchbox sectionfooter sectionheader separator slider spinbutton status strong subscript suggestion
		superscript switch tab table tablist tabpanel term textbox time timer toolbar tooltip tree treegrid treeitem
		disclosuretriangle popupbutton togglebutton labeltext descriptionlist listmarker`) {
		m[r] = true
	}
	return m
}()

// roleAliases are the AX roles a role target's role also matches:
// Chromium names an image "image" or "img", and a toggle or pop-up
// button by a role of its own.
var roleAliases = map[string][]string{
	"img":    {"img", "image"},
	"image":  {"img", "image"},
	"button": {"button", "togglebutton", "popupbutton"},
}

// roleMatches reports whether an AX role is the target's role.
func (t *target) roleMatches(axRole string) bool {
	r := strings.ToLower(axRole)
	if as, ok := roleAliases[t.role]; ok {
		return slices.Contains(as, r)
	}
	return r == t.role
}

func targetError(format string, a ...any) error {
	return &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, a...)}
}

// parseTarget reads a target, or says what's wrong with it.
func parseTarget(s string) (target, error) {
	if utf8.RuneCountInString(s) > maxTarget {
		return target{}, targetError("target: at most %d characters", maxTarget)
	}
	body := strings.TrimSpace(s)
	t := target{raw: body}
	var rest string
	var err error
	switch {
	case strings.HasPrefix(body, "role="):
		rest, err = t.parseRole(body[len("role="):])
	case strings.HasPrefix(body, "text="):
		rest, err = t.parseText(body[len("text="):])
	case refPattern.MatchString(body):
		return target{}, targetError("target %s is a ref: give it as ref", body)
	default:
		return target{}, targetError("target %s: want %s. CSS selectors aren't accepted", strconv.Quote(capText(body, maxField)), targetForms)
	}
	if err != nil {
		return target{}, err
	}
	t.base = strings.TrimSpace(strings.TrimSuffix(body, rest))
	if err := t.parseNth(rest); err != nil {
		return target{}, err
	}
	return t, nil
}

// parseRole reads what follows role=, returning what is left after it.
func (t *target) parseRole(s string) (string, error) {
	i := 0
	for i < len(s) && roleChar(s[i]) {
		i++
	}
	if i == 0 {
		return "", targetError(`role=: name a role, like role=button or role=button[name="Save"]`)
	}
	t.role = strings.ToLower(s[:i])
	if !knownRoles[t.role] {
		return "", targetError("role %s isn't a role BrowserPier knows: use an ARIA role, such as button, link, textbox, checkbox, radio, combobox, option, tab, menuitem or heading", strconv.Quote(s[:i]))
	}
	rest := s[i:]
	if !strings.HasPrefix(rest, "[") {
		return rest, nil
	}
	rest = strings.TrimLeft(rest[1:], " ")
	if !strings.HasPrefix(rest, "name") {
		return "", targetError(`role=%s[…]: only name is accepted, as in role=%s[name="Save"]`, t.role, t.role)
	}
	rest = strings.TrimPrefix(strings.TrimLeft(rest[len("name"):], " "), "*")
	if !strings.HasPrefix(rest, "=") {
		return "", targetError(`role=%s[name…: want [name="Save"]`, t.role)
	}
	rest = strings.TrimLeft(rest[1:], " ")
	var name string
	if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
		var err error
		if name, rest, err = unquote(rest); err != nil {
			return "", err
		}
		rest = strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(rest, "]") {
			return "", targetError(`role=%s[name=…: missing ] after the name`, t.role)
		}
	} else {
		// A bare name runs to the first ].
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return "", targetError(`role=%s[name=…: missing ]; quote a name that holds one, as in [name="a]b"]`, t.role)
		}
		name, rest = rest[:end], rest[end:]
	}
	t.name = cleanName(name)
	t.hasName = true
	return rest[1:], nil
}

func roleChar(b byte) bool {
	return b == '-' || b == '_' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

// cleanName is a wanted name as it is compared: collapsed, folded, and
// without a trailing "…" (a snapshot's mark of a cut name).
func cleanName(s string) string {
	s = collapse(s)
	s = strings.TrimSpace(strings.TrimSuffix(s, "…"))
	return fold(s)
}

// parseText reads what follows text=, returning what is left after it.
func (t *target) parseText(s string) (string, error) {
	s = strings.TrimLeft(s, " ")
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		v, rest, err := unquote(s)
		if err != nil {
			return "", err
		}
		t.text, t.exact = collapse(v), true
		if t.text == "" {
			return "", targetError(`text="": say what text to look for`)
		}
		return rest, nil
	}
	// Bare text runs to a last ">> nth=", if there is one.
	text, rest := s, ""
	if j := strings.LastIndex(s, ">>"); j >= 0 && strings.HasPrefix(strings.TrimSpace(s[j+2:]), "nth=") {
		text, rest = s[:j], s[j:]
	}
	t.text = fold(collapse(text))
	if t.text == "" {
		return "", targetError("text=: say what text to look for")
	}
	return rest, nil
}

// parseNth reads what follows a target's base: nothing, or >> nth=N.
func (t *target) parseNth(rest string) error {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	clause, ok := strings.CutPrefix(rest, ">>")
	clause = strings.TrimSpace(clause)
	if !ok || !strings.HasPrefix(clause, "nth=") {
		return targetError("target %s: %s is left over after the target; only >> nth=N may follow it", t.base, strconv.Quote(capText(rest, maxField)))
	}
	v := strings.TrimSpace(clause[len("nth="):])
	n, err := strconv.Atoi(v)
	if err != nil || n > maxNth || n < -maxNth {
		return targetError("%s: want a number, like nth=0 (the first match) or nth=-1 (the last)", capText(clause, maxField))
	}
	t.nth, t.hasNth = n, true
	return nil
}

// unquote reads a quoted string at the start of s (its first byte is the
// quote, " or '), where a backslash escapes the next character, as a
// snapshot writes \" in a name; it returns the rest after the closing
// quote.
func unquote(s string) (v, rest string, err error) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == q:
			return b.String(), s[i+1:], nil
		default:
			b.WriteByte(c)
		}
	}
	return "", "", targetError("target: missing the closing %c", q)
}

// candidate is an element a target matches.
type candidate struct {
	backend int64
	role    string
	name    string
	// text is the element's rendered text, for naming one that has no name.
	text string
}

// targetRead is what one read of the active tab found for a target: its
// visible matches, in document order, and, for a role target with none,
// the visible elements of that role.
type targetRead struct {
	frame, loader string
	matches       []candidate
	near          []candidate
}

// editableRoles are the fields whose text is their value, which a text
// target never reads (a password is never matched nor printed).
var editableRoles = map[string]bool{"textbox": true, "searchbox": true, "combobox": true, "spinbutton": true, "slider": true}

// errTargetTooLarge is the answer for a target on a page past the size cap.
var errTargetTooLarge = &Error{Status: http.StatusUnprocessableEntity, Msg: "page too large to resolve role= or text= targets (over 60,000 elements); use find, then act on a ref it gives"}

// errTargetMoved: the document changed while a target was looked for.
var errTargetMoved = &Error{Status: http.StatusConflict, Msg: "the page navigated while BrowserPier looked for the target; try again"}

// readTarget reads the active tab's top document, like a snapshot (it
// takes a whole-page read slot and refuses a page past the size cap), and
// finds the target's visible matches.
func (c *call) readTarget(ctx context.Context, t target) (*targetRead, error) {
	release, err := c.readSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	r := &targetRead{frame: c.frame, loader: c.loader}
	d, err := c.readDOM(ctx)
	if err != nil {
		return nil, err
	}
	if len(d.s.Documents[0].Nodes.BackendNodeID) > maxDOMNodes {
		return nil, errTargetTooLarge
	}
	nodes, err := c.readAX(ctx)
	if errors.Is(err, errTooLarge) {
		return nil, errTargetTooLarge
	}
	if err != nil {
		return nil, err
	}
	if t.role != "" {
		err = matchRole(ctx, t, nodes, d, r)
	} else {
		err = matchText(ctx, t, nodes, d, r)
	}
	if err != nil {
		return nil, err
	}
	// The read must be of one document: refs are made for it.
	f, err := c.topFrame(ctx, c.tgt)
	if err != nil {
		return nil, err
	}
	if f.ID != r.frame || f.LoaderID != r.loader {
		return nil, errTargetMoved
	}
	return r, nil
}

// axWalk calls visit on each node of the AX tree in document order, from
// its roots, not going into iframes (their documents aren't read) nor
// under a node visit says to skip. It stops when ctx ends.
func axWalk(ctx context.Context, nodes []axNode, visit func(n *axNode) (descend bool), after func(n *axNode)) error {
	byID := make(map[string]*axNode, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	visited := 0
	var stop error
	var walk func(n *axNode)
	walk = func(n *axNode) {
		if visited++; visited%checkEvery == 0 && stop == nil {
			stop = ctx.Err()
		}
		if stop != nil {
			return
		}
		if r := n.Role.String(); r == "Iframe" || r == "iframe" {
			return
		}
		if !visit(n) {
			return
		}
		for _, id := range n.ChildIDs {
			if k := byID[id]; k != nil {
				walk(k)
			}
		}
		if after != nil {
			after(n)
		}
	}
	for i := range nodes {
		if nodes[i].ParentID == "" {
			walk(&nodes[i])
		}
	}
	return stop
}

// visibleElement is node n's DOM element index, if it is an element the
// snapshot would show: not AX-ignored, laid out, not display:none or
// visibility:hidden, with a non-empty box.
func visibleElement(n *axNode, d *dom) (int, bool) {
	if n.Ignored {
		return 0, false
	}
	ni, ok := d.byBack[n.Backend]
	if !ok || d.s.Documents[0].Nodes.NodeType[ni] != 1 {
		return 0, false
	}
	if _, shown := d.shown(ni); !shown {
		return 0, false
	}
	return ni, true
}

// matchRole finds a role target's visible matches, and the role's
// visible elements.
func matchRole(ctx context.Context, t target, nodes []axNode, d *dom, r *targetRead) error {
	return axWalk(ctx, nodes, func(n *axNode) bool {
		if !t.roleMatches(n.Role.String()) {
			return true
		}
		if _, ok := visibleElement(n, d); !ok {
			return true
		}
		cd := candidate{backend: n.Backend, role: n.Role.String(), name: cutBytes(n.Name.String(), maxName)}
		r.near = append(r.near, cd)
		if !t.hasName || strings.Contains(fold(collapse(cd.name)), t.name) {
			r.matches = append(r.matches, cd)
		}
		return true
	}, nil)
}

// docText gathers a page's rendered text twice over (as it is, and
// folded), with its whitespace collapsed and a space between blocks, so
// that each element's text is a range of it.
type docText struct {
	raw, folded strings.Builder
	space       bool // the last thing written is a space, or nothing is
	block       int
	left        int
}

func (p *docText) addSpace() {
	if !p.space {
		p.raw.WriteByte(' ')
		p.folded.WriteByte(' ')
		p.space = true
	}
}

func (p *docText) add(s string, block int) {
	if block != p.block {
		p.addSpace()
		p.block = block
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			p.addSpace()
			continue
		}
		if p.left <= 0 {
			return
		}
		n, _ := p.raw.WriteRune(r)
		p.folded.WriteRune(unicode.ToLower(r))
		p.left -= n
		p.space = false
	}
}

// span is an element's text, as a range of a docText: start and end in
// raw (r) and in folded (f). parent is the nearest enclosing span's index
// (-1: none).
type span struct {
	cand           candidate
	r0, r1, f0, f1 int
	parent         int
	matched, below bool
}

// matchText finds a text target's visible matches: each visible element
// whose rendered text holds the text (or, exact, is it) while no visible
// element inside it does. Text is a node's own rendered text: what the
// AX tree's text nodes show (the DOM's own characters for their spaces),
// not fields' values, nor what is hidden.
func matchText(ctx context.Context, t target, nodes []axNode, d *dom, r *targetRead) error {
	pt := &docText{space: true, block: -1, left: maxPageText}
	var spans []span
	var stack []int  // open spans: index in spans, -1 for a node that isn't one
	var closed []int // spans as they close: one inside another first
	err := axWalk(ctx, nodes, func(n *axNode) bool {
		role := n.Role.String()
		switch {
		case role == "StaticText":
			if n.Ignored {
				return false
			}
			ni, ok := d.byBack[n.Backend]
			if !ok {
				return false
			}
			if _, shown := d.shown(ni); !shown {
				return false
			}
			s := n.Name.String()
			if doc := d.s.Documents[0].Nodes; doc.NodeType[ni] == 3 {
				s = d.str(doc.NodeValue[ni])
			}
			pt.add(cutBytes(s, maxNodeText), d.block(ni))
			return false
		case role == "LineBreak":
			pt.addSpace()
			return false
		case !n.Ignored && editableRoles[role]:
			return false // a field's text is its value
		}
		parent := -1
		for k := len(stack) - 1; k >= 0; k-- {
			if stack[k] >= 0 {
				parent = stack[k]
				break
			}
		}
		if _, ok := visibleElement(n, d); !ok {
			stack = append(stack, -1)
			return true
		}
		spans = append(spans, span{cand: candidate{backend: n.Backend, role: role, name: cutBytes(n.Name.String(), maxName)},
			r0: pt.raw.Len(), f0: pt.folded.Len(), parent: parent})
		stack = append(stack, len(spans)-1)
		return true
	}, func(n *axNode) {
		if len(stack) == 0 {
			return
		}
		if i := stack[len(stack)-1]; i >= 0 {
			spans[i].r1, spans[i].f1 = pt.raw.Len(), pt.folded.Len()
			closed = append(closed, i)
		}
		stack = stack[:len(stack)-1]
	})
	if err != nil {
		return err
	}
	raw, folded := pt.raw.String(), pt.folded.String()
	// Where the text is, in folded: every start, overlapping ones too.
	var at []int
	if !t.exact {
		for i := 0; ; {
			j := strings.Index(folded[i:], t.text)
			if j < 0 {
				break
			}
			at = append(at, i+j)
			_, size := utf8.DecodeRuneInString(folded[i+j:])
			i += j + size
		}
	}
	holds := func(s *span) bool {
		if t.exact {
			return s.r1-s.r0 <= len(t.text)+2 && strings.TrimSpace(raw[s.r0:s.r1]) == t.text
		}
		k := sort.SearchInts(at, s.f0)
		return k < len(at) && at[k]+len(t.text) <= s.f1
	}
	// Spans close inner first, so an inner one is judged before its
	// parent; their order of opening is document order.
	for _, i := range closed {
		s := &spans[i]
		if !s.below && holds(s) {
			s.matched = true
		}
		if (s.matched || s.below) && s.parent >= 0 {
			spans[s.parent].below = true
		}
	}
	for i := range spans {
		if spans[i].matched {
			cd := spans[i].cand
			cd.text = capText(collapse(raw[spans[i].r0:spans[i].r1]), 40)
			r.matches = append(r.matches, cd)
		}
	}
	return nil
}

// pick is the one match a target names, or an error listing candidates
// with refs (made in the same map as a snapshot's) when it names none or
// several.
func (c *call) pick(t target, r *targetRead) (int64, error) {
	ms := r.matches
	if t.hasNth {
		i := t.nth
		if i < 0 {
			i += len(ms)
		}
		if i >= 0 && i < len(ms) {
			return ms[i].backend, nil
		}
		msg := fmt.Sprintf("no visible element matches %s: %s matches %s", capText(t.raw, 2*maxField), capText(t.base, 2*maxField), plural(len(ms), "visible element"))
		if len(ms) > 0 {
			msg += ":" + c.candidates(r, ms)
		}
		return 0, &Error{Status: http.StatusConflict, Msg: msg}
	}
	switch len(ms) {
	case 1:
		return ms[0].backend, nil
	case 0:
		msg := "no visible element matches " + capText(t.raw, 2*maxField)
		switch {
		case t.role != "" && len(r.near) > 0:
			msg += fmt.Sprintf("; the page's visible %s elements:", t.role) + c.candidates(r, r.near)
		case t.role != "":
			msg += fmt.Sprintf("; the page has no visible %s element. Take a snapshot to see what it has.", t.role)
		default:
			msg += ". Try a shorter part of the text, or find."
		}
		return 0, &Error{Status: http.StatusConflict, Msg: msg}
	}
	return 0, c.ambiguous(t, r)
}

// ambiguous is the error for a target that matches several elements.
func (c *call) ambiguous(t target, r *targetRead) error {
	return &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("%s matches %d visible elements:%s\nBe more specific, use one of their refs, or add >> nth=0 (the first; nth=-1 is the last).",
		capText(t.raw, 2*maxField), len(r.matches), c.candidates(r, r.matches))}
}

// candidates lists the first few of cs, each with a ref, fenced (their
// names and text are the page's), then says how many more there are.
// A field's value is never shown: only its role and name.
func (c *call) candidates(r *targetRead, cs []candidate) string {
	c.st.refs.dropStale(c.tgt.ID, r.loader)
	lines := make([]string, 0, min(len(cs), maxCandidates))
	for _, cd := range cs[:min(len(cs), maxCandidates)] {
		role := cd.role
		if role == "" || role == "generic" || role == "none" {
			role = "element"
		}
		name := collapse(cd.name)
		if name == "" {
			name = cd.text
		}
		ref := c.st.refs.ref(refKey{target: c.tgt.ID, frame: r.frame, loader: r.loader, backend: cd.backend})
		lines = append(lines, role+" "+quote(capText(name, maxField))+" [ref="+ref+"]")
	}
	out := "\n" + fence.Wrap("page elements", strings.Join(lines, "\n"))
	if more := len(cs) - len(lines); more > 0 {
		out += fmt.Sprintf("\nand %d more", more)
	}
	return out
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// resolveTarget finds the one element a target names now, and returns
// its ref (made as a snapshot would make it), for resolve to take on.
func (c *call) resolveTarget(ctx context.Context, raw string) (string, error) {
	t, err := parseTarget(raw)
	if err != nil {
		return "", err
	}
	r, err := c.readTarget(ctx, t)
	if err != nil {
		return "", err
	}
	backend, err := c.pick(t, r)
	if err != nil {
		return "", err
	}
	c.st.refs.dropStale(c.tgt.ID, r.loader)
	return c.st.refs.ref(refKey{target: c.tgt.ID, frame: r.frame, loader: r.loader, backend: backend}), nil
}
