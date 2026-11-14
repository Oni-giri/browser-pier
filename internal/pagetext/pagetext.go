// Package pagetext turns a page, as the browser shows it, into text an
// agent can read cheaply: Markdown of the whole page, or the main text
// alone, as a reader view would show it (PROJ-20).
//
// It works on a Node tree the caller builds from what the browser laid
// out (internal/pagetools builds it from a DOM snapshot), so text that
// isn't rendered (display:none, visibility:hidden, zero-size) is already
// gone, and nothing here runs in the page. Form fields' values are never
// part of the tree: a page's content isn't where an agent reads them, and
// a password must not reach it by this road.
//
// Both renderers are written here rather than taken from a library: the
// libraries (html-to-markdown, go-readability) read HTML and can't see
// computed styles, so they would keep hidden text, and each brings half a
// dozen modules for what is a few hundred lines on a tree that is already
// cleaned. The output is for a model, inside the untrusted-content fence,
// so it favours few tokens over round-tripping to HTML.
package pagetext

import (
	"context"
	"strings"
)

// Node is an element or a text of a page, in the order the page shows
// them (shadow roots and slots already flattened).
type Node struct {
	// Tag is an element's lower-case name; "" for a text.
	Tag string
	// Text is a text node's content.
	Text string
	// Attrs are the element's attributes, by lower-case name. Only href,
	// src, alt, class, id, role, colspan and rowspan are read.
	Attrs map[string]string
	// Display is the element's computed display, "" when it isn't known
	// (not laid out, as with display: contents).
	Display string
	// Pre is set when white-space keeps line breaks (pre, pre-wrap,
	// pre-line, break-spaces).
	Pre  bool
	Kids []*Node

	// Worked out once by bound, bottom-up, so no renderer walks a subtree
	// twice: whether the element holds a block (inline markup can't span
	// it), a block a Markdown table cell can't hold, or an article or main
	// landmark; and how much non-space text it holds.
	hasBlock, hasTableBlock, hasMain bool
	tlen                             int
}

// maxDepth caps how deep a tree is rendered. Past it, an element's
// descendants are its text alone, after a marker saying so: real pages
// are a few dozen levels deep, and a page nesting 200,000 elements gets
// no more than this.
const maxDepth = 256

// The DOM snapshot's string table shares one string among every node that
// has it, so a page can give thousands of nodes the same megabyte of class
// names or text for the price of one. Work is kept linear in the nodes,
// not nodes × string length, by cutting what is read of each:
const (
	// MaxName caps a class or id, MaxAttr an alt, role or other short
	// attribute, and MaxAddr an href or src (longer addresses aren't kept
	// anyway).
	MaxName = 256
	MaxAttr = 1024
	MaxAddr = 2048
	// TextBudget is how many bytes of text are read from a whole page, and
	// MaxText of one text node: no more than the 2 MB of output could use.
	TextBudget = 8 << 20
	MaxText    = 2 << 20
)

// Notes BrowserPier adds to the text, outside the page's words.
const (
	// DeepNote marks where structure past maxDepth was flattened.
	DeepNote = "[BrowserPier: nested too deep; text only from here] "
)

// noteTag marks BrowserPier's own words in a tree: written as they are.
const noteTag = "#note"

// Skipped reports whether an element's content is never page text (a
// script, a style, a form field…): it spends no text budget.
func Skipped(tag string) bool { return skipped[tag] }

// Boilerplate reports whether an element is never part of a page's main
// text, by its tag or role (navigation, a sidebar, a form, a dialog): its
// text spends a budget of its own, MaxText, so a huge menu can't push the
// article out.
func Boilerplate(tag, role string) bool {
	switch tag {
	case "nav", "aside", "form", "dialog", "button":
		return true
	}
	return boilerRoles[role]
}

// CutAttr cuts an attribute's value to its cap.
func CutAttr(name, value string) string {
	switch name {
	case "class", "id":
		return cut(value, MaxName)
	case "href", "src":
		return cut(value, MaxAddr)
	}
	return cut(value, MaxAttr)
}

// Blank reports whether s is only whitespace.
func Blank(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isSpace(rune(s[i])) {
			return false
		}
	}
	return true
}

// bound copies a tree, at most maxDepth deep, with attributes cut to their
// caps and at most TextBudget of text (a blank text is one space), and
// works out each element's facts (see Node). Skipped elements (scripts,
// fields) are copied without their content, which spends no budget.
// truncated says text was left out past the budget. It runs without
// recursion, so any depth is safe, and stops with ctx's error when ctx
// ends.
func bound(ctx context.Context, root *Node) (top *Node, truncated bool, err error) {
	if root == nil {
		return &Node{Tag: "#document"}, false, nil
	}
	type frame struct {
		src, dst *Node
		depth    int
		boiler   bool // under boilerplate: its own budget
	}
	top = copyNode(root)
	var order []*Node // elements in pre-order, for the bottom-up pass
	stack := []frame{{src: root, dst: top}}
	budget, boilerBudget := TextBudget, MaxText
	for seen := 0; len(stack) > 0; seen++ {
		if seen%4096 == 0 && ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.src.Tag == "" {
			// Texts are read in document order, against one budget (and
			// boilerplate's against its own).
			b := &budget
			if f.boiler {
				b = &boilerBudget
			}
			if *b <= 0 {
				// Boilerplate past its budget isn't a cut: the main text
				// never shows it, and Markdown's output is full first.
				truncated = truncated || !f.boiler && !Blank(cut(f.src.Text, 64))
				f.dst.Text = ""
				continue
			}
			t := cut(f.src.Text, min(MaxText, *b))
			*b -= max(1, len(t))
			if !f.src.Pre && Blank(t) && t != "" {
				t = " "
			}
			f.dst.Text = t
			continue
		}
		order = append(order, f.dst)
		if skipped[f.src.Tag] {
			continue // its content is never rendered
		}
		if f.depth >= maxDepth {
			f.dst.Kids = append([]*Node{{Tag: noteTag, Text: DeepNote}}, flatText(f.src)...)
			continue
		}
		f.dst.Kids = make([]*Node, len(f.src.Kids))
		for i, k := range f.src.Kids {
			f.dst.Kids[i] = copyNode(k)
		}
		boiler := f.boiler || Boilerplate(f.src.Tag, f.src.Attr("role"))
		for i := len(f.src.Kids) - 1; i >= 0; i-- {
			stack = append(stack, frame{f.src.Kids[i], f.dst.Kids[i], f.depth + 1, boiler})
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		n.hasBlock, n.hasTableBlock, n.tlen = false, false, 0
		// A landmark counts unless its class says it is boilerplate (a
		// comment's <article class="comment-body">).
		n.hasMain = (n.Tag == "article" || n.Tag == "main" || n.Attr("role") == "article" || n.Attr("role") == "main") && (n.Attrs == nil || !unlikely.MatchString(classWords(n)))
		if skipped[n.Tag] {
			continue
		}
		for _, k := range n.Kids {
			if k.Tag == "" {
				n.tlen += len(strings.TrimFunc(k.Text, isSpace))
				continue
			}
			if skipped[k.Tag] {
				continue
			}
			n.tlen += k.tlen
			n.hasBlock = n.hasBlock || k.hasBlock || k.Tag != "br" && isBlock(k)
			n.hasMain = n.hasMain || k.hasMain
			switch k.Tag {
			case "table", "ul", "ol", "pre", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6":
				n.hasTableBlock = true
			}
			n.hasTableBlock = n.hasTableBlock || k.hasTableBlock
		}
	}
	return top, truncated, nil
}

func copyNode(n *Node) *Node {
	c := &Node{Tag: n.Tag, Text: n.Text, Display: n.Display, Pre: n.Pre} // a text is cut by bound, in document order
	if n.Tag == "" {
		return c
	}
	c.Attrs = n.Attrs
	for k, v := range n.Attrs {
		if cv := CutAttr(k, v); len(cv) != len(v) {
			c.Attrs = make(map[string]string, len(n.Attrs))
			for k, v := range n.Attrs {
				c.Attrs[k] = CutAttr(k, v)
			}
			break
		}
	}
	return c
}

// flatText is a subtree's texts, in order, without its elements, each cut
// to MaxAttr: past maxDepth, a page gets only its words.
func flatText(n *Node) []*Node {
	var out []*Node
	stack := []*Node{n}
	total := 0
	for len(stack) > 0 && total < MaxText {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c.Tag == "" {
			t := cut(c.Text, MaxAttr)
			total += len(t)
			out = append(out, &Node{Text: t + " ", Pre: c.Pre})
			continue
		}
		if skipped[c.Tag] {
			continue
		}
		for i := len(c.Kids) - 1; i >= 0; i-- {
			stack = append(stack, c.Kids[i])
		}
	}
	return out
}

// Attr is an attribute's value, "" if it isn't set.
func (n *Node) Attr(k string) string {
	if n == nil || n.Attrs == nil {
		return ""
	}
	return n.Attrs[k]
}

// skipped are elements whose content isn't page text: scripts, styles,
// embedded media, drawings and form fields (whose values never go out).
var skipped = map[string]bool{
	"head": true, "script": true, "style": true, "noscript": true, "template": true, "title": true, "meta": true, "link": true,
	"svg": true, "math": true, "canvas": true, "video": true, "audio": true, "object": true, "embed": true,
	"input": true, "select": true, "textarea": true, "option": true, "optgroup": true, "datalist": true, "output": true,
	"map": true, "area": true, "source": true, "track": true, "param": true,
}

// blockTags are elements that are blocks unless styled otherwise, used
// when the computed display isn't known.
var blockTags = map[string]bool{
	"html": true, "body": true, "address": true, "article": true, "aside": true, "blockquote": true, "details": true,
	"dialog": true, "dd": true, "div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "header": true,
	"hgroup": true, "hr": true, "li": true, "main": true, "nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"summary": true, "table": true, "tr": true, "td": true, "th": true, "thead": true, "tbody": true, "tfoot": true,
	"caption": true, "ul": true, "menu": true, "legend": true, "iframe": true,
}

// isBlock reports whether an element starts a new line: by its computed
// display when known, else by its tag.
func isBlock(n *Node) bool {
	switch n.Display {
	case "":
		return blockTags[n.Tag]
	case "inline", "inline-block", "inline-flex", "inline-grid", "inline-table", "contents", "ruby", "ruby-text":
		return false
	}
	return true
}

// innerText is a subtree's text, whitespace collapsed.
func innerText(n *Node) string {
	var b strings.Builder
	var walk func(*Node)
	walk = func(n *Node) {
		if n.Tag == "" {
			b.WriteString(n.Text)
			b.WriteByte(' ')
			return
		}
		if skipped[n.Tag] {
			return
		}
		for _, k := range n.Kids {
			walk(k)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
