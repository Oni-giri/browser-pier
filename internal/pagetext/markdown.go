package pagetext

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxURL caps a link or image address; a longer one (a data: URL, a
// tracking link) is left out and only the text kept.
const maxURL = 512

// Markdown renders a page as Markdown: headings, paragraphs, lists,
// quotes, code, simple tables, links and images, with addresses resolved
// against base. It stops once the output reaches limit bytes (keeping
// what fits), and says so with full; it stops with ctx's error when ctx
// ends. Rendering takes time and memory linear in the tree and the limit.
func Markdown(ctx context.Context, root *Node, base *url.URL, limit int) (string, bool, error) {
	b, err := bound(ctx, root)
	if err != nil {
		return "", false, err
	}
	return render(ctx, b, base, false, limit)
}

// Plain renders a page as plain text: paragraphs, headings and list
// items on their own lines, no links, images or markup.
func Plain(ctx context.Context, root *Node, limit int) (string, bool, error) {
	b, err := bound(ctx, root)
	if err != nil {
		return "", false, err
	}
	return render(ctx, b, nil, true, limit)
}

func render(ctx context.Context, root *Node, base *url.URL, plain bool, limit int) (string, bool, error) {
	w := &writer{ctx: ctx, base: base, plain: plain, limit: limit}
	w.node(root)
	w.flush()
	return w.String(), w.full, w.err
}

// writer renders blocks line by line. Inline content gathers in buf
// until a block ends; prefix holds each open quote ("> ") or list item
// (its indent), and marker the list marker the next line starts with.
type writer struct {
	ctx   context.Context
	plain bool
	base  *url.URL
	limit int
	err   error
	seen  int // nodes visited, to look at ctx now and then

	out  []string
	size int
	full bool

	prefix  []string
	marker  string
	gap     int // before the next block: 0 nothing, 1 a line break, 2 a blank line
	buf     []byte
	heading int  // the level of the heading in buf, 0 for a paragraph
	code    int  // inside inline code: no escaping
	opened  bool // a quote just opened: no blank line before its first block
	lists   int  // open lists
	items   int  // open list items
	// marks are the open emphases and links, innermost last; each writes
	// its opening mark only once some text comes, so an empty one leaves
	// nothing and nothing is copied back.
	marks []*mark
}

type mark struct {
	open    string
	written bool
}

// String is the output, with every "<!--" defused: the untrusted-content
// fence drops HTML comments, and a page's "<!--" would otherwise take
// the rest of the text with it.
func (w *writer) String() string {
	return strings.ReplaceAll(strings.Join(w.out, "\n"), "<!--", `<\!--`)
}

// room is how many more bytes the output may take, counting what is
// gathered in buf; -1 without a limit.
func (w *writer) room() int {
	if w.limit <= 0 {
		return -1
	}
	return max(0, w.limit-w.size-len(w.buf))
}

// cut shortens s to at most n bytes, on a rune boundary.
func cut(s string, n int) string {
	if n < 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// want asks for at least gap before the next block. Inside a list item
// blocks are a line apart, not a blank line: menus and tables of
// contents are lists of blocks, and blank lines would double them.
func (w *writer) want(gap int) {
	if w.items > 0 && gap > 1 {
		gap = 1
	}
	w.gap = max(w.gap, gap)
}

// add writes a line, or what fits of it.
func (w *writer) add(line string) {
	if w.size >= w.limit && w.limit > 0 {
		w.full = true
		return
	}
	if w.limit > 0 && w.size+len(line)+1 > w.limit {
		line = cut(line, w.limit-w.size-1)
		w.full = true
	}
	w.out = append(w.out, line)
	w.size += len(line) + 1
}

// emit writes a block's lines under the open prefixes, verbatim.
func (w *writer) emit(lines []string) {
	if len(lines) == 0 {
		return
	}
	if len(w.out) > 0 && w.gap == 2 && !w.opened {
		w.add(strings.TrimRight(w.quotes(), " "))
	}
	w.gap, w.opened = 0, false
	for _, l := range lines {
		p := strings.Join(w.prefix, "")
		if w.marker != "" {
			p = strings.Join(w.prefix[:len(w.prefix)-1], "") + w.marker
			w.marker = ""
		}
		w.add(strings.TrimRight(p+l, " "))
	}
}

// quotes is the open quotes' prefix, for a blank line inside them.
func (w *writer) quotes() string {
	var b strings.Builder
	for _, p := range w.prefix {
		if p == "> " {
			b.WriteString(p)
		}
	}
	return b.String()
}

// collapse turns each run of whitespace (nl too, if lines) into one
// space, in one pass.
func collapse(s string, lines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\f' || r == '\r' || r == '\v' || lines && r == '\n' {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

// flush ends the inline content gathered so far as one block.
func (w *writer) flush() {
	// Marks still open span the block's end: write them closed here and
	// open again after, as Markdown needs.
	for i := len(w.marks) - 1; i >= 0; i-- {
		if m := w.marks[i]; m.written {
			w.closeMark(m, closing(m.open))
			m.written = false
		}
	}
	raw := string(w.buf)
	w.buf = w.buf[:0]
	heading := w.heading
	w.heading = 0
	var lines []string
	blank := true
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSpace(collapse(l, false))
		if l == "" {
			if !blank {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		blank = false
		if !w.plain {
			l = escapeStart(l)
		}
		lines = append(lines, l)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return
	}
	if heading > 0 {
		h := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
		if !w.plain {
			h = strings.Repeat("#", heading) + " " + h
		}
		lines = []string{h}
	}
	w.emit(lines)
}

// closing is the mark that closes open: the same for emphasis; a link's
// is written by the link itself.
func closing(open string) string {
	if open == "[" {
		return "]"
	}
	return open
}

// block runs f as a block of its own, a blank line before and after.
func (w *writer) block(f func()) {
	w.flush()
	w.want(2)
	f()
	w.flush()
	w.want(2)
}

func (w *writer) kids(n *Node) {
	for _, k := range n.Kids {
		if w.full {
			return
		}
		w.node(k)
	}
}

func (w *writer) node(n *Node) {
	if w.full {
		return
	}
	if w.seen++; w.seen%1024 == 0 && w.ctx != nil {
		if err := w.ctx.Err(); err != nil {
			w.err, w.full = err, true
			return
		}
	}
	if n.Tag == "" {
		w.text(n)
		return
	}
	if skipped[n.Tag] {
		return
	}
	switch n.Tag {
	case noteTag:
		w.put(n.Text)
	case "br":
		w.buf = append(w.buf, '\n')
	case "hr":
		w.block(func() {
			if !w.plain {
				w.emit([]string{"---"})
			}
		})
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.block(func() {
			w.heading = int(n.Tag[1] - '0')
			w.kids(n)
		})
	case "ul", "ol", "menu":
		w.list(n)
	case "li":
		w.item(n, "- ")
	case "blockquote":
		w.block(func() {
			if w.plain {
				w.kids(n)
				return
			}
			if n.tlen == 0 {
				return
			}
			// The blank line before the quote is outside it.
			if len(w.out) > 0 && w.gap == 2 {
				w.add(strings.TrimRight(w.quotes(), " "))
			}
			w.gap, w.opened = 0, true
			w.prefix = append(w.prefix, "> ")
			w.kids(n)
			w.flush()
			w.prefix = w.prefix[:len(w.prefix)-1]
		})
	case "pre":
		w.pre(n)
	case "table":
		w.table(n)
	case "iframe":
		if !w.plain {
			if src := strings.Join(strings.Fields(n.Attr("src")), " "); src != "" {
				w.block(func() { w.emit([]string{"[iframe: " + escape(src) + "]"}) })
			}
		}
	case "a":
		w.link(n)
	case "img":
		w.image(n)
	case "code", "kbd", "samp", "tt":
		w.inlineCode(n)
	case "strong", "b":
		w.wrap(n, "**")
	case "em", "i", "cite", "var", "dfn":
		w.wrap(n, "_")
	case "del", "s", "strike":
		w.wrap(n, "~~")
	default:
		if isBlock(n) {
			w.block(func() { w.kids(n) })
			return
		}
		w.kids(n)
	}
}

// put adds inline content, writing first the opening marks still
// pending, after any leading space, and keeping within the limit.
func (w *writer) put(s string) {
	if s == "" {
		return
	}
	if pending := w.pending(); pending != "" {
		t := strings.TrimLeft(s, " ")
		if t == "" {
			w.buf = append(w.buf, ' ')
			return
		}
		lead := s[:len(s)-len(t)]
		s = lead + pending + t
		for _, m := range w.marks {
			m.written = true
		}
	}
	if r := w.room(); r >= 0 && len(s) > r {
		s = cut(s, r)
		w.full = true
	}
	w.buf = append(w.buf, s...)
}

// pending is the opening marks not written yet.
func (w *writer) pending() string {
	var b strings.Builder
	for _, m := range w.marks {
		if !m.written {
			b.WriteString(m.open)
		}
	}
	return b.String()
}

// text adds a text's content, whitespace collapsed unless it keeps line
// breaks, escaped for Markdown outside code.
func (w *writer) text(n *Node) {
	s := n.Text
	if r := w.room(); r >= 0 && len(s) > r+16 {
		s = cut(s, r+16) // the rest can't fit
	}
	if n.Pre {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	} else {
		if s = collapse(s, true); s == " " {
			w.buf = append(w.buf, ' ')
			return
		}
	}
	if !w.plain && w.code == 0 {
		s = escape(s)
	}
	w.put(s)
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
}

var escaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`)

func escape(s string) string { return escaper.Replace(s) }

var (
	ordered   = regexp.MustCompile(`^(\d{1,9})([.)])(\s|$)`)
	lineStart = regexp.MustCompile(`^(#{1,6}(\s|$)|>|[-+](\s|$)|[-=]{2,}\s*$|~{3,}|\|)`)
)

// escapeStart escapes what would make a line a heading, a quote, a list
// item, a rule, a code fence or a table row.
func escapeStart(l string) string {
	if m := ordered.FindStringSubmatchIndex(l); m != nil {
		return l[:m[3]] + `\` + l[m[3]:]
	}
	if lineStart.MatchString(l) {
		return `\` + l
	}
	return l
}

// openMark starts emphasis or a link: its mark waits for the first text.
func (w *writer) openMark(open string) *mark {
	m := &mark{open: open}
	w.marks = append(w.marks, m)
	return m
}

// closeMark ends the innermost mark: if it was written, closing goes
// after the text, before its trailing space.
func (w *writer) closeMark(m *mark, closing string) {
	if !m.written {
		return
	}
	end := len(w.buf)
	for end > 0 && w.buf[end-1] == ' ' {
		end--
	}
	trail := len(w.buf) - end
	w.buf = append(w.buf[:end], closing...)
	if trail > 0 {
		w.buf = append(w.buf, ' ')
	}
}

func (w *writer) popMark() *mark {
	m := w.marks[len(w.marks)-1]
	w.marks = w.marks[:len(w.marks)-1]
	return m
}

// wrap renders emphasis: the children between two marks.
func (w *writer) wrap(n *Node, mk string) {
	if w.plain || w.code > 0 || n.hasBlock {
		w.kids(n)
		return
	}
	w.openMark(mk)
	w.kids(n)
	w.closeMark(w.popMark(), mk)
}

// inlineCode renders code in backticks, as it is: its text is gathered
// apart, once, to choose a fence longer than any run of backticks in it.
func (w *writer) inlineCode(n *Node) {
	if w.plain || w.code > 0 || n.hasBlock {
		w.kids(n)
		return
	}
	marks := w.marks
	w.marks = nil
	start := len(w.buf)
	w.code++
	w.kids(n)
	w.code--
	s := string(w.buf[start:])
	w.buf = w.buf[:start]
	w.marks = marks
	inner := strings.TrimFunc(strings.ReplaceAll(s, "\n", " "), isSpace)
	if inner == "" {
		if s != "" {
			w.buf = append(w.buf, ' ')
		}
		return
	}
	lead, trail := "", ""
	if isSpace(rune(s[0])) {
		lead = " "
	}
	if isSpace(rune(s[len(s)-1])) {
		trail = " "
	}
	fence := strings.Repeat("`", longestRun(inner, '`')+1)
	if strings.HasPrefix(inner, "`") || strings.HasSuffix(inner, "`") {
		inner = " " + inner + " "
	}
	w.put(lead + fence + inner + fence + trail)
}

func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}

// href is a link's address for the agent, "" for one it can't follow (a
// script, a fragment of this page, a user name in it, too long).
func (w *writer) href(n *Node) string {
	raw := strings.TrimSpace(n.Attr("href"))
	if strings.HasPrefix(raw, "#") {
		return ""
	}
	return w.resolve(raw, "http", "https", "mailto")
}

// link renders a link as [text](address). A link around blocks (a card on
// a front page) has its blocks, then its address on a line of its own.
func (w *writer) link(n *Node) {
	if w.plain || w.code > 0 {
		w.kids(n)
		return
	}
	href := w.href(n)
	if href == "" {
		w.kids(n)
		return
	}
	if n.hasBlock {
		w.kids(n)
		if n.tlen > 0 {
			w.flush()
			w.want(1)
			w.emit([]string{"<" + href + ">"})
			w.want(2)
		}
		return
	}
	w.openMark("[")
	w.kids(n)
	w.closeMark(w.popMark(), "]("+href+")")
}

// image renders an image with alt text as ![alt](address); one without
// is decoration, and left out.
func (w *writer) image(n *Node) {
	if w.plain {
		return
	}
	alt := strings.Join(strings.Fields(n.Attr("alt")), " ")
	if alt == "" {
		return
	}
	if w.code > 0 {
		w.put(alt)
		return
	}
	alt = escape(alt)
	if src := w.resolve(n.Attr("src"), "http", "https"); src != "" {
		w.put("![" + alt + "](" + src + ")")
		return
	}
	w.put(alt)
}

// resolve makes raw absolute against the page's address, if its scheme
// is one of schemes, it names no user and it isn't too long; else "".
func (w *writer) resolve(raw string, schemes ...string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURL*2 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if w.base != nil {
		u = w.base.ResolveReference(u)
	}
	ok := false
	for _, s := range schemes {
		ok = ok || u.Scheme == s
	}
	// https://good.example@evil.example/ reads as good.example's.
	if !ok || u.User != nil {
		return ""
	}
	s := u.String()
	s = strings.NewReplacer("(", "%28", ")", "%29", " ", "%20", "<", "%3C", ">", "%3E", "\n", "", "\r", "").Replace(s)
	if len(s) > maxURL {
		return ""
	}
	return s
}

func (w *writer) list(n *Node) {
	w.flush()
	if w.lists > 0 {
		w.want(1)
	} else {
		w.want(2)
	}
	w.lists++
	i := 1
	if n.Tag == "ol" {
		if s, err := strconv.Atoi(n.Attr("start")); err == nil && s >= 0 && s < 1e6 {
			i = s
		}
	}
	for _, k := range n.Kids {
		if w.full {
			break
		}
		if k.Tag != "li" {
			w.node(k)
			continue
		}
		marker := "- "
		if n.Tag == "ol" {
			marker = strconv.Itoa(i) + ". "
			i++
		}
		w.item(k, marker)
	}
	w.lists--
	w.flush()
	w.want(2)
}

// item renders a list item: its first line after marker, the rest
// indented under it.
func (w *writer) item(n *Node, marker string) {
	w.flush()
	w.want(1)
	w.prefix = append(w.prefix, strings.Repeat(" ", len(marker)))
	w.marker = marker
	w.items++
	w.kids(n)
	w.flush()
	w.items--
	w.marker = "" // an empty item writes nothing
	w.prefix = w.prefix[:len(w.prefix)-1]
	w.want(1)
}

// pre renders preformatted text as a fenced code block, as it is.
func (w *writer) pre(n *Node) {
	w.flush()
	w.want(2)
	room := w.room()
	var b strings.Builder
	var walk func(*Node)
	walk = func(n *Node) {
		if room >= 0 && b.Len() > room {
			return
		}
		switch {
		case n.Tag == "":
			b.WriteString(strings.ReplaceAll(n.Text, "\r\n", "\n"))
		case n.Tag == "br":
			b.WriteByte('\n')
		case !skipped[n.Tag]:
			for _, k := range n.Kids {
				walk(k)
			}
		}
	}
	walk(n)
	lines := strings.Split(strings.TrimRight(cut(b.String(), room), " \t\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	if !w.plain {
		fence := strings.Repeat("`", max(3, longestRun(strings.Join(lines, "\n"), '`')+1))
		lines = append(append([]string{fence + language(n)}, lines...), fence)
	}
	w.emit(lines)
	w.want(2)
}

var langClass = regexp.MustCompile(`(?:^|\s)(?:language|lang)-([A-Za-z0-9_+#-]{1,20})(?:\s|$)`)

// language is a code block's language, from a language-xxx class on the
// pre or its code element.
func language(n *Node) string {
	for _, c := range append([]*Node{n}, n.Kids...) {
		if m := langClass.FindStringSubmatch(c.Attr("class")); m != nil {
			return m[1]
		}
	}
	return ""
}
