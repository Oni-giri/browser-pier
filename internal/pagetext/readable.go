package pagetext

import (
	"regexp"
	"strings"
)

// Readable renders a page's main text as plain text, as a reader view
// would show it: without navigation, banners, sidebars, footers, forms
// and dialogs, and only the part of the page that reads as its article.
func Readable(root *Node, limit int) (string, bool) {
	return Plain(Article(root), limit)
}

// minArticle is how much text a page's article, main landmark or best
// scored block needs to be taken as its main text.
const minArticle = 200

// Article finds a page's main content, after the heuristics of reader
// views such as Mozilla's Readability (written anew here, not ported):
//
//  1. Leave out what is never the article: navigation, headers and
//     footers outside an article, sidebars, forms, dialogs, buttons, and
//     blocks whose class or id says banner, cookie, comment, share…
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
func Article(root *Node) *Node {
	body := find(root, func(n *Node) bool { return n.Tag == "body" })
	if body == nil {
		body = root
	}
	clean := prune(body, false)
	if clean == nil {
		return &Node{Tag: "div"}
	}
	m := measure(clean)
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
		return m.trim(a)
	}
	if a := pick(func(n *Node) bool { return n.Tag == "main" || n.Attr("role") == "main" }); a != nil {
		return m.trim(a)
	}
	if a := m.best(clean); a != nil && m.text[a] >= minArticle {
		return m.trim(a)
	}
	return m.trim(clean)
}

var (
	// unlikely class or id words: blocks that are never the article.
	unlikely = regexp.MustCompile(`(?i)ad-break|advert|agegate|banner|breadcrumb|comment|community|consent|cookie|disqus|footer|gdpr|masthead|menu|modal|newsletter|pager|pagination|popup|promo|related|remark|replies|share|shoutbox|sidebar|skip|social|sponsor|subscribe|toolbar|widget`)
	// likely class or id words, which keep a block an unlikely word would drop.
	likely = regexp.MustCompile(`(?i)article|body|column|content|entry|main|post|story|text`)
	// positive and negative class or id words, for scoring.
	positive = regexp.MustCompile(`(?i)article|blog|body|content|entry|h-entry|hentry|main|page|post|story|text`)
	negative = regexp.MustCompile(`(?i)banner|byline|comment|contact|cookie|foot|hidden|masthead|media|menu|meta|nav|promo|related|share|sidebar|social|sponsor|tags|tool|widget`)
)

// boilerRoles are ARIA roles of what is never the article.
var boilerRoles = map[string]bool{
	"navigation": true, "complementary": true, "search": true, "menu": true, "menubar": true,
	"dialog": true, "alertdialog": true, "toolbar": true, "tablist": true, "form": true,
}

// prune copies a tree without its boilerplate. inArticle keeps headers
// and footers inside an article or main, which hold its title and notes.
func prune(n *Node, inArticle bool) *Node {
	if n.Tag == "" {
		return n
	}
	if skipped[n.Tag] {
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
	case n.Tag != "body" && n.Tag != "article" && n.Tag != "main":
		if ci := n.Attr("class") + " " + n.Attr("id"); unlikely.MatchString(ci) && !likely.MatchString(ci) {
			return nil
		}
	}
	inArticle = inArticle || n.Tag == "article" || n.Tag == "main" || role == "article" || role == "main"
	c := *n
	c.Kids = nil
	for _, k := range n.Kids {
		if p := prune(k, inArticle); p != nil {
			c.Kids = append(c.Kids, p)
		}
	}
	return &c
}

// measures holds each element's text length, link text length and
// parent in a pruned tree.
type measures struct {
	text, links map[*Node]int
	parent      map[*Node]*Node
}

func measure(root *Node) *measures {
	m := &measures{text: map[*Node]int{}, links: map[*Node]int{}, parent: map[*Node]*Node{}}
	var walk func(n *Node, inLink bool) (int, int)
	walk = func(n *Node, inLink bool) (int, int) {
		if n.Tag == "" {
			l := len(strings.Join(strings.Fields(n.Text), " "))
			if inLink {
				return l, l
			}
			return l, 0
		}
		inLink = inLink || n.Tag == "a"
		t, ln := 0, 0
		for _, k := range n.Kids {
			m.parent[k] = n
			kt, kl := walk(k, inLink)
			t += kt
			ln += kl
		}
		m.text[n], m.links[n] = t, ln
		return t, ln
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
	ci := n.Attr("class") + " " + n.Attr("id")
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
		t := innerText(n)
		if len(t) < 25 {
			return true
		}
		s := 1 + float64(strings.Count(t, ",")+strings.Count(t, "，")) + float64(min(len(t)/100, 3))
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
