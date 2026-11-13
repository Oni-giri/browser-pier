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
