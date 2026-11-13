package pagetext

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// maxURL caps a link or image address; a longer one (a data: URL, a
// tracking link) is left out and only the text kept.
const maxURL = 512

// Markdown renders a page as Markdown: headings, paragraphs, lists,
// quotes, code, simple tables, links and images, with addresses resolved
// against base. It stops once the output passes limit bytes, and says so.
func Markdown(root *Node, base *url.URL, limit int) (string, bool) {
	w := &writer{base: base, limit: limit}
	w.node(root)
	w.flush()
	return w.String(), w.full
}

// Plain renders a page as plain text: paragraphs, headings and list
// items on their own lines, no links, images or markup.
func Plain(root *Node, limit int) (string, bool) {
	w := &writer{plain: true, limit: limit}
	w.node(root)
	w.flush()
	return w.String(), w.full
}

// writer renders blocks line by line. Inline content gathers in buf
// until a block ends; prefix holds each open quote ("> ") or list item
// (its indent), and marker the list marker the next line starts with.
type writer struct {
	plain bool
	base  *url.URL
	limit int

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
}

func (w *writer) String() string { return strings.Join(w.out, "\n") }

// want asks for at least gap before the next block. A list item's first
// block keeps the item's own line break.
func (w *writer) want(gap int) {
	if w.marker != "" && gap > 1 {
		gap = 1
	}
	w.gap = max(w.gap, gap)
}

func (w *writer) add(line string) {
	if w.full {
		return
	}
	if w.limit > 0 && w.size+len(line)+1 > w.limit {
		w.full = true
		return
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

var spaces = regexp.MustCompile(`[ \t\f\r\v]+`)

// flush ends the inline content gathered so far as one block.
func (w *writer) flush() {
	raw := string(w.buf)
	w.buf = w.buf[:0]
	heading := w.heading
	w.heading = 0
	var lines []string
	blank := true
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSpace(spaces.ReplaceAllString(l, " "))
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
	if n.Tag == "" {
		w.text(n)
		return
	}
	if skipped[n.Tag] {
		return
	}
	switch n.Tag {
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
			if innerText(n) == "" {
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
			if src := n.Attr("src"); src != "" {
				w.block(func() { w.emit([]string{"[iframe: " + src + "]"}) })
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

// text adds a text's content, whitespace collapsed unless it keeps line
// breaks, escaped for Markdown outside code.
func (w *writer) text(n *Node) {
	s := n.Text
	if n.Pre {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	} else {
		s = strings.Join(strings.FieldsFunc(s, isSpace), " ")
		if s == "" {
			if strings.TrimFunc(n.Text, isSpace) == "" && n.Text != "" {
				w.buf = append(w.buf, ' ')
			}
			return
		}
		if isSpace(rune(n.Text[0])) {
			s = " " + s
		}
		if isSpace(rune(n.Text[len(n.Text)-1])) {
			s += " "
		}
	}
	if !w.plain && w.code == 0 {
		s = escape(s)
	}
	w.buf = append(w.buf, s...)
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
}

var escaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`)

func escape(s string) string { return escaper.Replace(s) }

var (
	ordered   = regexp.MustCompile(`^(\d{1,9})([.)])(\s|$)`)
	lineStart = regexp.MustCompile(`^(#{1,6}(\s|$)|>|[-+](\s|$)|[-=]{2,}\s*$)`)
)

// escapeStart escapes what would make a line a heading, a quote, a list
// item or a rule.
func escapeStart(l string) string {
	if m := ordered.FindStringSubmatchIndex(l); m != nil {
		return l[:m[3]] + `\` + l[m[3]:]
	}
	if lineStart.MatchString(l) {
		return `\` + l
	}
	return l
}

// hasBlock reports whether an element holds a block, which inline
// markup (a link, emphasis) can't span.
func hasBlock(n *Node) bool {
	for _, k := range n.Kids {
		if k.Tag == "" || skipped[k.Tag] {
			continue
		}
		if k.Tag != "br" && isBlock(k) || hasBlock(k) {
			return true
		}
	}
	return false
}

// inline renders an element's children and returns them, trimmed, with
// the whitespace that was around them; the caller writes them back
// wrapped. ok is false when there is nothing to wrap.
func (w *writer) inline(n *Node) (lead, inner, trail string, ok bool) {
	start := len(w.buf)
	w.kids(n)
	s := string(w.buf[start:])
	w.buf = w.buf[:start]
	t := strings.TrimFunc(s, isSpace)
	if t == "" {
		if s != "" {
			w.buf = append(w.buf, ' ')
		}
		return "", "", "", false
	}
	if s[0] != t[0] {
		lead = " "
	}
	if s[len(s)-1] != t[len(t)-1] {
		trail = " "
	}
	return lead, t, trail, true
}

// wrap renders emphasis: the children between two marks.
func (w *writer) wrap(n *Node, mark string) {
	if w.plain || w.code > 0 || hasBlock(n) {
		w.kids(n)
		return
	}
	lead, inner, trail, ok := w.inline(n)
	if !ok {
		return
	}
	if strings.Contains(inner, "\n") {
		w.buf = append(w.buf, lead+inner+trail...)
		return
	}
	w.buf = append(w.buf, lead+mark+inner+mark+trail...)
}

func (w *writer) inlineCode(n *Node) {
	if w.plain || w.code > 0 || hasBlock(n) {
		w.kids(n)
		return
	}
	w.code++
	lead, inner, trail, ok := w.inline(n)
	w.code--
	if !ok {
		return
	}
	inner = strings.ReplaceAll(inner, "\n", " ")
	fence := strings.Repeat("`", longestRun(inner, '`')+1)
	if strings.HasPrefix(inner, "`") || strings.HasSuffix(inner, "`") {
		inner = " " + inner + " "
	}
	w.buf = append(w.buf, lead+fence+inner+fence+trail...)
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

// link renders a link as [text](address); a link the agent can't follow
// (a script, a fragment of this page, too long) keeps only its text.
func (w *writer) link(n *Node) {
	if w.plain || w.code > 0 || hasBlock(n) {
		w.kids(n)
		return
	}
	lead, inner, trail, ok := w.inline(n)
	if !ok {
		return
	}
	href := ""
	if raw := strings.TrimSpace(n.Attr("href")); !strings.HasPrefix(raw, "#") {
		href = w.resolve(raw, "http", "https", "mailto")
	}
	if href == "" || strings.Contains(inner, "\n") {
		w.buf = append(w.buf, lead+inner+trail...)
		return
	}
	w.buf = append(w.buf, lead+"["+inner+"]("+href+")"+trail...)
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
		w.buf = append(w.buf, alt...)
		return
	}
	alt = escape(alt)
	if src := w.resolve(n.Attr("src"), "http", "https"); src != "" {
		w.buf = append(w.buf, "!["+alt+"]("+src+")"...)
		return
	}
	w.buf = append(w.buf, alt...)
}

// resolve makes raw absolute against the page's address, if its scheme
// is one of schemes and it isn't too long; else "".
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
	if !ok {
		return ""
	}
	s := u.String()
	s = strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace(s)
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
	w.kids(n)
	w.flush()
	w.marker = "" // an empty item writes nothing
	w.prefix = w.prefix[:len(w.prefix)-1]
	w.want(1)
}

// pre renders preformatted text as a fenced code block, as it is.
func (w *writer) pre(n *Node) {
	w.flush()
	w.want(2)
	var b strings.Builder
	var walk func(*Node)
	walk = func(n *Node) {
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
	lines := strings.Split(strings.TrimRight(b.String(), " \t\n"), "\n")
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
