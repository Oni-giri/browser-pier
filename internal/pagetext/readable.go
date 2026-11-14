package pagetext

import (
	"context"
	"regexp"
	"strings"
	"unicode"
)

// Readable renders a page's main text as plain text, as a reader view
// would show it: without navigation, banners, sidebars, footers, forms
// and dialogs, and only the part of the page that reads as its article.
// Like Markdown, it takes time and memory linear in the tree and limit.
func Readable(ctx context.Context, root *Node, limit int) (string, bool, error) {
	b, err := bound(ctx, root)
	if err != nil {
		return "", false, err
	}
	chk := &checker{ctx: ctx}
	a := article(b, chk)
	if chk.err != nil {
		return "", false, chk.err
	}
	if b, err = bound(ctx, a); err != nil {
		return "", false, err
	}
	return render(ctx, b, nil, true, limit)
}

// checker looks at a context every few thousand nodes.
type checker struct {
	ctx context.Context
	n   int
	err error
}

func (c *checker) stop() bool {
	if c == nil || c.ctx == nil {
		return false
	}
	if c.n++; c.err == nil && c.n%4096 == 0 {
		c.err = c.ctx.Err()
	}
	return c.err != nil
}

// minArticle is how much text a page's article, main landmark or best
// scored block needs to be taken as its main text.
const minArticle = 200

// article finds a page's main content, after the heuristics of reader
// views such as Mozilla's Readability (written anew here, not ported):
//
//  1. Leave out what is never the article: navigation, headers and
//     footers outside an article, sidebars, forms, dialogs, buttons, and
//     blocks whose class or id names a banner, cookies, comments, a
//     footer… (as words: "cookie_notice", "siteFooter" and
//     "has-sidebar" all count), but never a block holding an article or
//     main that isn't itself such a block.
//  2. A page with exactly one <article> (or role=article) of some length
//     has its main content there; else one <main> (or role=main).
//  3. Otherwise score blocks: each paragraph of 25 characters or more
//     scores 1, plus its commas, plus 1 per 100 characters (up to 3), and
//     gives that to its parent, half to its grandparent and less further
//     up. A block's own tag and class adjust its score, and its share of
//     link text lowers it. The best block wins, with those of its
//     siblings that score close to it, or are paragraphs of prose.
//
// What is left out of the winner (lists of links, say) is pruned the
// same way. A page where nothing stands out comes back whole, cleaned.
//
// One retry, for a wrapper whose class misled step 1 (a page wrapped in
// <div id="community-page">): when the page has no article or main, what
// was found is under minArticle, and a single block left out by its class
// held more than half the page's text, the article is found again with
// that block kept, and taken only if it keeps everything found first.
func article(root *Node, chk *checker) *Node {
	body := find(root, func(n *Node) bool { return n.Tag == "body" })
	if body == nil {
		body = root
	}
	var dropped []*Node
	first := articleOf(body, chk, nil, &dropped)
	if body.hasMain || textSize(first) >= minArticle || chk.err != nil {
		return first
	}
	page := textSize(prune(body, false, false, chk, nil, nil))
	var wrapper *Node
	for _, d := range dropped {
		if d.tlen*2 > page && (wrapper == nil || d.tlen > wrapper.tlen) {
			wrapper = d
		}
	}
	if wrapper == nil {
		return first
	}
	second := articleOf(body, chk, wrapper, nil)
	if textSize(second) > textSize(first) && keepsAll(second, first) {
		return second
	}
	return first
}

// keepsAll reports whether every text of b is in a (pruning keeps texts'
// identity).
func keepsAll(a, b *Node) bool {
	in := map[*Node]bool{}
	stack := []*Node{a}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c.Tag == "" {
			in[c] = true
		}
		stack = append(stack, c.Kids...)
	}
	stack = []*Node{b}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c.Tag == "" && strings.TrimSpace(c.Text) != "" && !in[c] {
			return false
		}
		stack = append(stack, c.Kids...)
	}
	return true
}

// articleOf is steps 1 to 3. keep is a block left in despite its class;
// dropped, if not nil, collects the blocks left out by their class.
func articleOf(body *Node, chk *checker, keep *Node, dropped *[]*Node) *Node {
	clean := prune(body, false, true, chk, keep, dropped)
	if clean == nil {
		return &Node{Tag: "div"}
	}
	m := measure(clean, chk)
	pick := func(match func(*Node) bool) *Node {
		var found []*Node
		walk(clean, func(n *Node) bool {
			if match(n) {
				found = append(found, n)
				return false
			}
			return true
		})
		if len(found) == 1 && m.text[found[0]] >= minArticle {
			return found[0]
		}
		return nil
	}
	if a := pick(func(n *Node) bool { return n.Tag == "article" || n.Attr("role") == "article" }); a != nil {
		return m.final(a)
	}
	if a := pick(func(n *Node) bool { return n.Tag == "main" || n.Attr("role") == "main" }); a != nil {
		return m.final(a)
	}
	if a := m.best(clean); a != nil && m.text[a] >= minArticle {
		return m.final(a)
	}
	return m.final(clean)
}

// textSize counts a tree's non-space text, without recursion.
func textSize(n *Node) int {
	t := 0
	stack := []*Node{n}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c.Tag == "" {
			t += len(strings.TrimFunc(c.Text, isSpace))
			continue
		}
		if !skipped[c.Tag] {
			stack = append(stack, c.Kids...)
		}
	}
	return t
}

var (
	// unlikely class or id words: blocks that are never the article.
	unlikely = regexp.MustCompile(` (?:ad break|adverts?|advertisement|agegate|banners?|breadcrumbs?|comments?|community|consent|cookies?|disqus|footer|gdpr|masthead|menus?|modal|newsletter|pager|pagination|popup|promos?|related|remarks?|replies|share|sharing|shoutbox|sidebar|skip|social|sponsors?|sponsored|subscribe|toolbar|widgets?) `)
	// likely class or id words, which keep a block an unlikely word would drop.
	likely = regexp.MustCompile(`article|body|column|content|entry|main|post|story|text`)
	// positive and negative class or id words, for scoring.
	positive = regexp.MustCompile(`article|blog|body|content|entry|h ?entry|main|page|post|story|text`)
	negative = regexp.MustCompile(` (?:banners?|byline|comments?|contact|cookies?|foot|footer|footnotes?|hidden|masthead|media|menus?|meta|nav|promos?|related|share|sidebar|social|sponsors?|tags|tools?|widgets?) `)
)

// classWords is an element's class and id as spaced lowercase words, split
// at separators, camelCase and digits: "siteFooter cookie_notice" reads
// " site footer cookie notice ". Both are cut to MaxName first.
func classWords(n *Node) string {
	s := cut(n.Attr("class"), MaxName) + " " + cut(n.Attr("id"), MaxName)
	var b strings.Builder
	b.WriteByte(' ')
	prev := ' '
	for _, r := range s {
		switch {
		case unicode.IsUpper(r) && unicode.IsLower(prev),
			unicode.IsDigit(r) && unicode.IsLetter(prev), unicode.IsLetter(r) && unicode.IsDigit(prev):
			b.WriteByte(' ')
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
		prev = r
	}
	b.WriteByte(' ')
	return b.String()
}

// unlikelyClass reports whether an element's class or id says it is never
// the article.
func unlikelyClass(n *Node) bool {
	if n.Attrs == nil {
		return false
	}
	w := classWords(n)
	return unlikely.MatchString(w) && !likely.MatchString(w)
}

// boilerRoles are ARIA roles of what is never the article.
var boilerRoles = map[string]bool{
	"navigation": true, "complementary": true, "search": true, "menu": true, "menubar": true,
	"dialog": true, "alertdialog": true, "toolbar": true, "tablist": true, "form": true,
}

// prune copies a tree without its boilerplate. inArticle keeps headers
// and footers inside an article or main, which hold its title and notes;
// classes leaves out blocks by their class or id too, except keep, and
// adds what it leaves out so to dropped.
func prune(n *Node, inArticle, classes bool, chk *checker, keep *Node, dropped *[]*Node) *Node {
	if n.Tag == "" {
		return n
	}
	if skipped[n.Tag] || chk.stop() {
		return nil
	}
	role := n.Attr("role")
	switch {
	case n.Tag == "nav" || n.Tag == "aside" || n.Tag == "form" || n.Tag == "dialog" || n.Tag == "button" || n.Tag == "iframe":
		return nil
	case boilerRoles[role]:
		return nil
	case !inArticle && (n.Tag == "header" || n.Tag == "footer" || role == "banner" || role == "contentinfo"):
		return nil
	case classes && n != keep && !n.hasMain && n.Tag != "body" && unlikelyClass(n):
		if dropped != nil {
			*dropped = append(*dropped, n)
		}
		return nil
	}
	inArticle = inArticle || n.Tag == "article" || n.Tag == "main" || role == "article" || role == "main"
	c := *n
	c.Kids = nil
	for _, k := range n.Kids {
		if p := prune(k, inArticle, classes, chk, keep, dropped); p != nil {
			c.Kids = append(c.Kids, p)
		}
	}
	return &c
}

// measures holds each element's text length, link text length and
// parent in a pruned tree.
type measures struct {
	text, links, commas map[*Node]int
	parent              map[*Node]*Node
}

func measure(root *Node, chk *checker) *measures {
	m := &measures{text: map[*Node]int{}, links: map[*Node]int{}, commas: map[*Node]int{}, parent: map[*Node]*Node{}}
	var walk func(n *Node, inLink bool) (int, int, int)
	walk = func(n *Node, inLink bool) (int, int, int) {
		if n.Tag == "" {
			l := len(strings.TrimFunc(n.Text, isSpace))
			c := strings.Count(n.Text, ",") + strings.Count(n.Text, "，")
			if inLink {
				return l, l, c
			}
			return l, 0, c
		}
		inLink = inLink || n.Tag == "a"
		t, ln, cm := 0, 0, 0
		if chk.stop() {
			return 0, 0, 0
		}
		for _, k := range n.Kids {
			m.parent[k] = n
			kt, kl, kc := walk(k, inLink)
			t += kt
			ln += kl
			cm += kc
		}
		m.text[n], m.links[n], m.commas[n] = t, ln, cm
		return t, ln, cm
	}
	walk(root, false)
	return m
}

func (m *measures) linkDensity(n *Node) float64 {
	if m.text[n] == 0 {
		return 0
	}
	return float64(m.links[n]) / float64(m.text[n])
}

// paragraphs are what scores: prose blocks, and blocks holding no other
// block.
func paragraph(n *Node) bool {
	switch n.Tag {
	case "p", "pre", "td", "blockquote":
		return true
	case "div", "section", "li", "dd":
		for _, k := range n.Kids {
			if k.Tag != "" && isBlock(k) {
				return false
			}
		}
		return true
	}
	return false
}

// baseScore is what a block's tag and class say of it.
func baseScore(n *Node) float64 {
	s := 0.0
	switch n.Tag {
	case "div", "article", "main", "section":
		s = 5
	case "pre", "td", "blockquote":
		s = 3
	case "address", "ol", "ul", "dl", "dd", "dt", "li", "form":
		s = -3
	case "h1", "h2", "h3", "h4", "h5", "h6", "th":
		s = -5
	}
	ci := classWords(n)
	if positive.MatchString(ci) {
		s += 25
	}
	if negative.MatchString(ci) {
		s -= 25
	}
	return s
}

// best scores the blocks of a pruned tree and returns the article: the
// best block, with its siblings that read as part of it.
func (m *measures) best(root *Node) *Node {
	scores := map[*Node]float64{}
	var order []*Node
	walk(root, func(n *Node) bool {
		if n.Tag == "" || !paragraph(n) {
			return true
		}
		t := m.text[n]
		if t < 25 {
			return true
		}
		s := 1 + float64(m.commas[n]) + float64(min(t/100, 3))
		for level, a := 0, m.parent[n]; a != nil && level < 5; level, a = level+1, m.parent[a] {
			if _, ok := scores[a]; !ok {
				scores[a] = baseScore(a)
				order = append(order, a)
			}
			div := 1.0
			switch {
			case level == 1:
				div = 2
			case level > 1:
				div = float64(level * 3)
			}
			scores[a] += s / div
		}
		return true
	})
	var top *Node
	topScore := 0.0
	for _, a := range order {
		s := scores[a] * (1 - m.linkDensity(a))
		scores[a] = s
		if top == nil || s > topScore {
			top, topScore = a, s
		}
	}
	if top == nil {
		return nil
	}
	parent := m.parent[top]
	if parent == nil {
		return top
	}
	// The siblings that belong with it.
	threshold := max(10, topScore*0.2)
	out := &Node{Tag: "div"}
	for _, s := range parent.Kids {
		keep := s == top
		if !keep && s.Tag != "" {
			if sc, ok := scores[s]; ok && sc >= threshold {
				keep = true
			} else if s.Tag == "p" {
				t, ld := m.text[s], m.linkDensity(s)
				keep = t > 80 && ld < 0.25 || t > 0 && t <= 80 && ld == 0 && strings.ContainsAny(innerText(s), ".!?")
			}
		}
		if keep {
			out.Kids = append(out.Kids, s)
			m.parent[s] = out
		}
	}
	m.text[out], m.links[out] = 0, 0
	for _, k := range out.Kids {
		if k.Tag == "" {
			m.text[out] += len(strings.TrimSpace(k.Text))
			continue
		}
		m.text[out] += m.text[k]
		m.links[out] += m.links[k]
	}
	return out
}

// final is the article trimmed of its link lists, unless that leaves
// little of it: a page made of links (a news front page) keeps them.
func (m *measures) final(n *Node) *Node {
	t := m.trim(n)
	if left := textSize(t); left < minArticle || left*3 < m.text[n] {
		return n
	}
	return t
}

// trim copies the article without its blocks that are mostly links:
// lists, tables and divisions where links are more than half the text.
func (m *measures) trim(n *Node) *Node {
	if n.Tag == "" {
		return n
	}
	c := *n
	c.Kids = nil
	for _, k := range n.Kids {
		if k.Tag != "" && k != n {
			switch k.Tag {
			case "ul", "ol", "table", "div", "section", "dl":
				if m.linkDensity(k) > 0.5 {
					continue
				}
			}
		}
		c.Kids = append(c.Kids, m.trim(k))
	}
	return &c
}

// walk visits a tree's elements in order; f returns false to skip an
// element's children.
func walk(n *Node, f func(*Node) bool) {
	if !f(n) {
		return
	}
	for _, k := range n.Kids {
		if k.Tag != "" {
			walk(k, f)
		}
	}
}

// find returns the first element f matches.
func find(n *Node, f func(*Node) bool) *Node {
	var found *Node
	walk(n, func(n *Node) bool {
		if found != nil {
			return false
		}
		if f(n) {
			found = n
			return false
		}
		return true
	})
	return found
}
