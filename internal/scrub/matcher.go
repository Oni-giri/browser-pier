package scrub

import "slices"

// matcher finds every occurrence of a set of patterns in one pass
// (Aho-Corasick, as a DFA over byte classes), so scrubbing costs the same
// however many secrets and forms there are: a table lookup a byte.
type matcher struct {
	class   [256]uint16 // byte → class; 0 is every byte no pattern has
	classes int
	next    []int32 // state*classes+class → state; negated when a pattern ends there
	longest []int32 // the longest pattern ending at each state, 0 if none
}

// span is a half-open byte range of the text to hide.
type span struct{ start, end int }

func newMatcher(pats []string) *matcher {
	m := &matcher{}
	for _, p := range pats {
		for i := 0; i < len(p); i++ {
			if m.class[p[i]] == 0 {
				m.classes++
				m.class[p[i]] = uint16(m.classes) //nolint:gosec // at most 256 distinct bytes
			}
		}
	}
	m.classes++
	nc := m.classes
	m.next = make([]int32, nc)
	m.longest = []int32{0}
	// The trie.
	for _, p := range pats {
		st := int32(0)
		for i := 0; i < len(p); i++ {
			c := int(m.class[p[i]])
			if m.next[int(st)*nc+c] == 0 {
				m.next = append(m.next, make([]int32, nc)...)
				m.longest = append(m.longest, 0)
				m.next[int(st)*nc+c] = int32(len(m.longest) - 1) //nolint:gosec // states ≤ the patterns' bytes; the table would fill memory long before 2³¹
			}
			st = m.next[int(st)*nc+c]
		}
		m.longest[st] = max(m.longest[st], int32(len(p))) //nolint:gosec // as above
	}
	// Failure links, breadth first, folded into the table so scanning
	// never follows them; each state also ends the longest pattern its
	// failure state ends.
	fail := make([]int32, len(m.longest))
	queue := make([]int32, 0, len(m.longest))
	for c := 0; c < nc; c++ {
		if s := m.next[c]; s != 0 {
			queue = append(queue, s) // fail[s] = 0
		}
	}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		m.longest[u] = max(m.longest[u], m.longest[fail[u]])
		for c := 0; c < nc; c++ {
			v := m.next[int(u)*nc+c]
			f := m.next[int(fail[u])*nc+c]
			if v == 0 {
				m.next[int(u)*nc+c] = f
				continue
			}
			fail[v] = f
			queue = append(queue, v)
		}
	}
	// Mark the transitions into a state that ends a pattern, so a scan
	// reads longest only on a match.
	for i, st := range m.next {
		if m.longest[st] > 0 {
			m.next[i] = -st
		}
	}
	return m
}

// find returns the spans of text to hide, sorted and merged, or nil
// (without allocating) when no pattern occurs.
func (m *matcher) find(text string) []span {
	nc := int32(m.classes) //nolint:gosec // at most 257
	class := m.class       // on the stack: the hot loop reads it once a byte
	next := m.next
	st := int32(0)
	var spans []span
	for i := 0; i < len(text); i++ {
		st = next[st*nc+int32(class[text[i]])]
		if st < 0 {
			st = -st
			n := int(m.longest[st])
			spans = append(spans, span{i + 1 - n, i + 1})
		}
	}
	if len(spans) < 2 {
		return spans
	}
	// Ends only grow, but a longer match can start before an earlier
	// shorter one; sort by start and merge whatever overlaps or touches.
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	out := spans[:1]
	for _, sp := range spans[1:] {
		last := &out[len(out)-1]
		if sp.start <= last.end {
			last.end = max(last.end, sp.end)
			continue
		}
		out = append(out, sp)
	}
	return out
}
