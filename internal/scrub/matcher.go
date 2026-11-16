package scrub

import (
	"math/bits"
	"slices"
	"unicode/utf8"
)

// matcher finds every occurrence of a set of patterns in one pass
// (Aho-Corasick, as a DFA over byte classes), so scrubbing costs the same
// however many secrets and forms there are: a table lookup a byte. It
// matches the text's normalized shape (normalize.go) as it reads it.
type matcher struct {
	class   [256]uint16 // byte → class; 0 is every byte no pattern has; normFlag marks a byte normalizing may change
	classes int
	next    []int32 // state*classes+class → state; negated when a pattern ends there
	longest []int32 // the longest pattern ending at each state, 0 if none
	maxLen  int     // the longest pattern
}

// normFlag, in a class, marks a byte with a normKind.
const normFlag = 0x8000

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
		m.maxLen = max(m.maxLen, len(p))
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
	for c := range m.class {
		if normKind[c] != 0 {
			m.class[c] |= normFlag
		}
	}
	return m
}

// find returns the spans of text to hide, sorted and merged, or nil
// (without allocating) when no pattern occurs. It reads text in its
// normalized shape, remembering where each of the last maxLen normalized
// bytes started in text, so a match maps back to the bytes it stands for.
func (m *matcher) find(text string) []span {
	nc := int32(m.classes) //nolint:gosec // at most 257
	class := m.class       // on the stack: the hot loop reads it once a byte
	next := m.next
	var buf [1024]int // ring of the normalized bytes' starts in text
	ring := buf[:]
	if m.maxLen > len(buf) {
		ring = make([]int, 1<<bits.Len(uint(m.maxLen)))
	}
	mask := len(ring) - 1
	st := int32(0)
	n := 0         // normalized bytes read
	space := false // the last one was a normalized space
	var spans []span
	for i := 0; i < len(text); {
		c := text[i]
		cl := class[c]
		if cl&normFlag == 0 {
			ring[n&mask] = i
			n++
			i++
			space = false
			st = next[st*nc+int32(cl)]
			if st < 0 {
				st = -st
				spans = append(spans, span{ring[(n-int(m.longest[st]))&mask], i})
			}
			continue
		}
		r, end := norm(text, i)
		var enc [utf8.UTFMax]byte
		var out []byte
		switch {
		case r == ' ' && space:
			i = end // collapsed into the space before
			continue
		case r < 0:
			enc[0] = c
			out = enc[:1]
		default:
			out = enc[:utf8.EncodeRune(enc[:], r)]
		}
		space = r == ' '
		for _, b := range out {
			ring[n&mask] = i
			n++
			st = next[st*nc+int32(class[b]&^normFlag)]
			if st < 0 {
				st = -st
				spans = append(spans, span{ring[(n-int(m.longest[st]))&mask], end})
			}
		}
		i = end
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
