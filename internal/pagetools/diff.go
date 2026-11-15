package pagetools

import (
	"context"
	"hash/maphash"
	"slices"
	"strconv"
	"strings"
)

// Snapshot diffs (PROJ-114): after act, what changed on the page since the
// agent's last snapshot of that document, rather than a whole snapshot.
// They compare the page's items, not snapshot text: render orders lines by
// priority and sums up long navigations, so its text would report lines
// that only moved. Refs are stable within a document, so an item that
// didn't change is the same element with the same ref.

const (
	// maxDiffLines caps a diff's lines, context lines included.
	maxDiffLines = 20
	// maxDiffText caps a text line of a diff, in characters.
	maxDiffText = 200
	// maxIndent caps how deep a diff line is indented, and how far up its
	// enclosing items are looked for.
	maxIndent = 8
	// maxBaseline caps one stored item list, as baseSize counts it, and
	// maxBaselines all of them together, across sessions: past it, the
	// least recently stored go first.
	maxBaseline  = 4 << 20
	maxBaselines = 32 << 20
)

// Messages of a diff that aren't from the page.
const (
	diffNoBaseline = "no earlier snapshot of this page; take one"
	diffNoChanges  = "no changes since the last snapshot"
	diffNavigated  = "take a snapshot of the new page"
	// diffChangedPast ends an added line that reads as its removed one.
	diffChangedPast = " (changed past what this line shows)"
)

// keySeed seeds the items' keys; keys are compared only within this process.
var keySeed = maphash.MakeSeed()

// itemKey identifies an item's line for a diff: its node (so its ref) and
// everything its line shows, except focus, which every click moves. Its
// tier, group and place in the list aren't part of it: they change the
// snapshot's order, not the line.
func itemKey(it item) uint64 {
	if it.key != 0 {
		return it.key
	}
	var h maphash.Hash
	h.SetSeed(keySeed)
	kind := "t"
	switch {
	case it.control:
		kind = "c"
	case it.iframe:
		kind = "i"
	}
	_, _ = h.WriteString(kind + strconv.FormatInt(it.backend, 10))
	for _, s := range []string{it.role, it.name, it.value, it.text} {
		_ = h.WriteByte(0)
		_, _ = h.WriteString(s)
	}
	for _, st := range it.states {
		if st == "focused" {
			continue
		}
		_ = h.WriteByte(1)
		_, _ = h.WriteString(st)
	}
	k := h.Sum64()
	if k == 0 {
		k = 1 // 0 means not worked out
	}
	return k
}

// compact makes the copy of a page's items that is kept as a baseline:
// each with its key worked out from its whole text, then its strings cut
// to what a diff line shows and copied, so a kept list holds no part of
// the page's answer and stays small.
func compact(items []item) []item {
	out := make([]item, len(items))
	for i, it := range items {
		it.key = itemKey(it)
		it.role = strings.Clone(cutBytes(it.role, 64))
		it.name = strings.Clone(cutBytes(collapse(cutBytes(it.name, maxName)), 4*(maxField+1)))
		it.value = strings.Clone(it.value)
		it.text = strings.Clone(cutBytes(it.text, 4*(maxDiffText+1)))
		it.states = slices.Clone(it.states)
		out[i] = it
	}
	return out
}

// baseSize is about what a kept list takes in memory.
func baseSize(items []item) int {
	n := 0
	for _, it := range items {
		n += 160 + len(it.role) + len(it.name) + len(it.value) + len(it.text)
		for _, st := range it.states {
			n += 16 + len(st)
		}
	}
	return n
}

// diffLine is one line of a diff: '+' added, '-' removed, ' ' an
// unchanged item that encloses a changed one. old says which list it is
// from (removed lines and their context are from the old one).
type diffLine struct {
	sign  byte
	depth int
	it    item
	old   bool
	// pair, for an added line whose node had another line before (a
	// changed state), is the index of that removed line among the
	// diff's lines; -1 otherwise.
	pair int
}

// itemDiff is what diffItems found.
type itemDiff struct {
	noBaseline bool
	lines      []diffLine
}

// diffItems compares two reads of one document's items. A nil old list is
// no baseline at all (not "everything was added"). Items are matched by
// key, whatever their order, so an item that only moved isn't reported;
// what is left is removed (from old) or added (to new). Each comes after
// its nearest enclosing items that didn't change, as context, removed
// ones at their old place. A changed state ([checked], [expanded],
// [filled], [under an iframe]) is a changed key: one removed line and one
// added, with the same ref.
func diffItems(old, new []item) itemDiff {
	if old == nil {
		return itemDiff{noBaseline: true}
	}
	byKey := map[uint64][]int{}
	for j, it := range new {
		k := itemKey(it)
		byKey[k] = append(byKey[k], j)
	}
	matchOld := make([]int, len(old)) // the new index, or -1
	matchNew := make([]int, len(new)) // the old index, or -1
	for j := range matchNew {
		matchNew[j] = -1
	}
	for i, it := range old {
		k := itemKey(it)
		if js := byKey[k]; len(js) > 0 {
			matchOld[i], matchNew[js[0]] = js[0], i
			byKey[k] = js[1:]
		} else {
			matchOld[i] = -1
		}
	}
	// A removed item goes after the new place of the last unchanged item
	// before it in the old list.
	removedAfter := map[int][]int{}
	last := -1
	for i := range old {
		if matchOld[i] >= 0 {
			last = matchOld[i]
			continue
		}
		removedAfter[last] = append(removedAfter[last], i)
	}
	d := itemDiff{lines: []diffLine{}}
	shownOld, shownNew := map[int]bool{}, map[int]bool{}
	// enclose adds the unchanged items enclosing list[at], outermost first.
	enclose := func(list []item, at int, isOld bool) {
		var up []int
		for p, n := list[at].parent, 0; p > 0 && p <= len(list) && n < maxIndent; p, n = list[p-1].parent, n+1 {
			up = append(up, p-1)
		}
		for k := len(up) - 1; k >= 0; k-- {
			a := up[k]
			var o, n int
			if isOld {
				o, n = a, matchOld[a]
			} else {
				o, n = matchNew[a], a
			}
			if o < 0 || n < 0 || shownOld[o] || shownNew[n] {
				continue
			}
			shownOld[o], shownNew[n] = true, true
			d.lines = append(d.lines, diffLine{sign: ' ', depth: depthOf(list, a), it: list[a], old: isOld, pair: -1})
		}
	}
	// A removed item whose node is still there with another line (a
	// changed state) goes just before its new line instead.
	sameNode := map[int64][]int{}
	for i := range old {
		if matchOld[i] < 0 {
			sameNode[old[i].backend] = append(sameNode[old[i].backend], i)
		}
	}
	pairOf := make([]int, len(new)) // the old index, or -1
	paired := make([]bool, len(old))
	for j := range new {
		pairOf[j] = -1
		if matchNew[j] >= 0 {
			continue
		}
		if is := sameNode[new[j].backend]; len(is) > 0 {
			pairOf[j], paired[is[0]] = is[0], true
			sameNode[new[j].backend] = is[1:]
		}
	}
	// A removed item whose enclosing item was removed or changed too goes
	// right after that one's removed line, not before or outside it.
	under := make([]bool, len(old))
	kids := map[int][]int{}
	for i := range old {
		if p := old[i].parent - 1; matchOld[i] < 0 && p >= 0 && p < i && matchOld[p] < 0 {
			under[i] = true
			kids[p] = append(kids[p], i)
		}
	}
	var removed func(i int) int
	removed = func(i int) int {
		enclose(old, i, true)
		at := len(d.lines)
		d.lines = append(d.lines, diffLine{sign: '-', depth: depthOf(old, i), it: old[i], old: true, pair: -1})
		for _, k := range kids[i] {
			if !paired[k] {
				removed(k)
			}
		}
		return at
	}
	unpaired := func(is []int) {
		for _, i := range is {
			if !paired[i] && !under[i] {
				removed(i)
			}
		}
	}
	unpaired(removedAfter[-1])
	for j := range new {
		if matchNew[j] < 0 {
			pair := -1
			if i := pairOf[j]; i >= 0 {
				pair = removed(i)
			}
			enclose(new, j, false)
			d.lines = append(d.lines, diffLine{sign: '+', depth: depthOf(new, j), it: new[j], pair: pair})
		}
		unpaired(removedAfter[j])
	}
	return d
}

// depthOf is how many items enclose list[i], at most maxIndent.
func depthOf(list []item, i int) int {
	n := 0
	for p := list[i].parent; p > 0 && p <= len(list) && n < maxIndent; p = list[p-1].parent {
		n++
	}
	return n
}

// render writes the diff: body is the page's (for the fence), trailer
// BrowserPier's own words. line is how an item reads, from the old list
// or the new.
func (d itemDiff) render(line func(it item, old bool) string) (body, trailer string) {
	switch {
	case d.noBaseline:
		return "", diffNoBaseline
	case len(d.lines) == 0:
		return "", diffNoChanges
	}
	var b strings.Builder
	shown := make([]string, 0, min(len(d.lines), maxDiffLines))
	for k, l := range d.lines {
		if k == maxDiffLines {
			trailer = "(" + thousands(len(d.lines)-maxDiffLines) + " more lines; snapshot for the whole page)"
			break
		}
		if k > 0 {
			b.WriteByte('\n')
		}
		s := line(l.it, l.old)
		shown = append(shown, s)
		if l.pair >= 0 && l.pair < k && shown[l.pair] == s {
			// What changed is past what the line shows (a long text's
			// end, say): the two lines would read the same.
			s += diffChangedPast
		}
		b.WriteByte(l.sign)
		b.WriteByte(' ')
		b.WriteString(strings.Repeat("  ", l.depth))
		b.WriteString(s)
	}
	return b.String(), trailer
}

// linkParents sets each item's parent: the nearest item whose node is an
// ancestor of its own in the DOM (a control in a paragraph has the
// paragraph's text as its parent). Text items stand for their block, so
// their parents are looked for from the block up. Memoised per node, so
// a deep tree costs one walk in all.
func linkParents(items []item, d *dom) {
	owner := make(map[int]int, len(items)) // node index → first item on it
	for i, it := range items {
		if ni, ok := d.byBack[it.backend]; ok {
			if _, dup := owner[ni]; !dup {
				owner[ni] = i
			}
		}
	}
	// near[n] is the nearest item at or above node n, plus one; 0 not
	// known yet, -1 none.
	near := make([]int32, len(d.s.Documents[0].Nodes.ParentIndex))
	nearest := func(n int) int {
		var path []int
		res := -1
		for q := n; q >= 0 && q < len(near); q = d.parent(q) {
			if near[q] != 0 {
				res = int(near[q])
				break
			}
			if i, ok := owner[q]; ok {
				res = i + 1
				break
			}
			path = append(path, q)
		}
		for _, q := range path {
			near[q] = int32(res)
		}
		return res
	}
	for i := range items {
		ni, ok := d.byBack[items[i].backend]
		if !ok {
			continue
		}
		if p := nearest(d.parent(ni)); p > 0 && p-1 != i {
			items[i].parent = p
		}
	}
}

// baseKey names one kept list: a session's tab.
type baseKey struct{ session, target string }

// baseline is the item list of the agent's last snapshot of a tab's
// document, kept for diffs.
type baseline struct {
	frame, loader string
	items         []item
	size          int
	seq           uint64 // when it was stored, for dropping the oldest
}

// baseline is a tab's kept list, nil if none.
func (t *Tools) baseline(session, target string) *baseline {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bases[baseKey{session, target}]
}

// setBaseline keeps items (compacted) as a tab's list for its document,
// in place of any before. A list past maxBaseline isn't kept; past
// maxBaselines in all, the oldest lists go.
func (t *Tools) setBaseline(session, target, frame, loader string, items []item) {
	t.keepBaseline(session, target, frame, loader, compact(items))
}

// keepBaseline is setBaseline with items already compacted.
func (t *Tools) keepBaseline(session, target, frame, loader string, kept []item) {
	size := baseSize(kept)
	t.mu.Lock()
	defer t.mu.Unlock()
	k := baseKey{session, target}
	t.dropBaseLocked(k)
	if size > maxBaseline {
		return
	}
	t.baseSeq++
	t.bases[k] = &baseline{frame: frame, loader: loader, items: kept, size: size, seq: t.baseSeq}
	t.baseBytes += size
	for t.baseBytes > maxBaselines {
		var oldest baseKey
		var seq uint64
		for bk, b := range t.bases {
			if seq == 0 || b.seq < seq {
				oldest, seq = bk, b.seq
			}
		}
		t.dropBaseLocked(oldest)
	}
}

func (t *Tools) dropBaseLocked(k baseKey) {
	if b := t.bases[k]; b != nil {
		t.baseBytes -= b.size
		delete(t.bases, k)
	}
}

// dropBaseline forgets a tab's kept list.
func (t *Tools) dropBaseline(session, target string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropBaseLocked(baseKey{session, target})
}

// dropBaselinesLocked forgets a session's kept lists.
func (t *Tools) dropBaselinesLocked(session string) {
	for k := range t.bases {
		if k.session == session {
			t.dropBaseLocked(k)
		}
	}
}

// forgetSession drops what the tools keep about a session that ended.
func (t *Tools) forgetSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropBaselinesLocked(id)
	delete(t.pages, id)
}

// diffKind is what a diff could compare.
type diffKind int

const (
	diffDone       diffKind = iota // compared with the last snapshot
	diffFirst                      // no earlier snapshot of this document
	diffOtherDoc                   // the tab is on another document now
	diffMovedUnder                 // the document changed while it was read
)

// diffResult is a diff's answer: body from the page, for the fence, and
// trailer in BrowserPier's words.
type diffResult struct {
	kind          diffKind
	body, trailer string
}

// changes reads the active tab and diffs it against the tab's kept list.
// After a diff, the new list takes the old one's place; with nothing to
// compare with, keepWhole says whether the read becomes the tab's list
// (snapshot shows the whole page then; act doesn't, so a list it keeps is
// of a page the agent hasn't seen) — except that a list is always kept
// when there was none, and never when the document changed under the
// read. The read takes a whole-page read slot, like snapshot.
func (c *call) changes(ctx context.Context, keepWhole bool) (*page, diffResult, error) {
	p, moved, err := c.readDoc(ctx)
	if err != nil {
		return nil, diffResult{}, err
	}
	sid, target := c.sess.ID, c.tgt.ID
	navigated := diffResult{kind: diffOtherDoc, body: "navigated to " + capText(p.url, maxHeader), trailer: diffNavigated}
	if moved {
		c.t.dropBaseline(sid, target)
		navigated.kind = diffMovedUnder
		return p, navigated, nil
	}
	c.st.refs.dropStale(target, p.loader)
	b := c.t.baseline(sid, target)
	switch {
	case b == nil:
		c.t.setBaseline(sid, target, p.frame, p.loader, p.items)
		return p, diffResult{kind: diffFirst, trailer: diffNoBaseline}, nil
	case b.loader != p.loader || b.frame != p.frame:
		if keepWhole {
			c.t.setBaseline(sid, target, p.frame, p.loader, p.items)
		} else {
			c.t.dropBaseline(sid, target)
		}
		return p, navigated, nil
	}
	cur := compact(p.items)
	oldPage := &page{frame: b.frame, loader: b.loader}
	body, trailer := diffItems(b.items, cur).render(func(it item, old bool) string {
		if !it.control && !it.iframe {
			return capText(it.text, maxDiffText)
		}
		if old {
			return c.line(oldPage, it)
		}
		return c.line(p, it)
	})
	c.t.keepBaseline(sid, target, p.frame, p.loader, cur)
	return p, diffResult{kind: diffDone, body: body, trailer: trailer}, nil
}

// readDoc reads the active tab as snapshot does, then checks its document
// is still the one the read began on (moved if not), so a read that
// straddles a navigation is taken for neither document's.
func (c *call) readDoc(ctx context.Context) (p *page, moved bool, err error) {
	if p, err = c.read(ctx, true); err != nil {
		return nil, false, err
	}
	frame, loader, _, err := c.frameTree(ctx, c.tgt)
	if err != nil {
		return nil, false, err
	}
	return p, loader != p.loader || frame != p.frame, nil
}
