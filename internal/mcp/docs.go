package mcp

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/docs"
)

// Limits on what read_docs returns at once.
const (
	maxDocsPage = 40000 // characters of one page
	maxDocsHits = 5     // sections for a query
	maxDocsHit  = 3000  // characters of one section
)

func (s *Server) addDocsTool(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "read_docs", Description: "Read BrowserPier's own docs, for the version this server runs: " +
		"how a tool, endpoint or setting works, what an error means, what to do next. " +
		"With no argument, the list of pages; with page, that page as Markdown; with query, the sections that match it, best first."}, s.readDocs)
}

type docsArgs struct {
	Page  string `json:"page,omitempty" jsonschema:"a page's path from the list, such as reference/handoffs"`
	Query string `json:"query,omitempty" jsonschema:"words to look for, such as until_url, or an error message"`
}

// readDocs answers from the docs built into this binary. They're
// BrowserPier's own text, not a page's, so they aren't fenced.
func (s *Server) readDocs(_ context.Context, _ *sdk.CallToolRequest, in docsArgs) (*sdk.CallToolResult, any, error) {
	if !s.docs.Built() {
		return text("This BrowserPier (%s) was built without its docs. Read them at %sllms.txt; "+
			"they describe the latest release, which may be newer than this server.", s.version, docs.Online), nil, nil
	}
	switch {
	case strings.TrimSpace(in.Page) != "":
		p, ok := s.docs.Page(in.Page)
		if !ok {
			return nil, nil, fmt.Errorf("no docs page %q; call read_docs with no argument for the list", in.Page)
		}
		md := p.Markdown
		if len(md) > maxDocsPage {
			md = cut(md, maxDocsPage) + "\n\n[The page goes on; search it with query for the part you need.]"
		}
		return text("%s", md), nil, nil
	case strings.TrimSpace(in.Query) != "":
		hits := s.docs.Search(in.Query, maxDocsHits)
		if len(hits) == 0 {
			return text("Nothing in the docs matches %q. Call read_docs with no argument for the list of pages.", in.Query), nil, nil
		}
		var b strings.Builder
		for i, h := range hits {
			if i > 0 {
				b.WriteString("\n\n---\n\n")
			}
			where := h.Page.Path
			if h.Anchor != "" {
				where += "#" + h.Anchor
			}
			fmt.Fprintf(&b, "From %q (page %s):\n\n%s", h.Page.Title, where, cut(h.Text, maxDocsHit))
		}
		return text("%s", b.String()), nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "BrowserPier %s docs. Read a page with page, or search them all with query.\n\n", s.version)
	for _, p := range s.docs.Pages() {
		fmt.Fprintf(&b, "- %s: %s", p.Path, p.Title)
		if p.Description != "" {
			fmt.Fprintf(&b, ". %s", p.Description)
		}
		b.WriteByte('\n')
	}
	return text("%s", b.String()), nil, nil
}

// cut shortens text to at most n bytes, at a line break when there's one
// in the last quarter, and never inside a UTF-8 character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // back to the start of the character the cut falls in
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, '\n'); i > n*3/4 {
		s = s[:i]
	}
	return s + "\n…"
}
