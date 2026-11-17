package mcp

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Oni-giri/browser-pier/internal/docs"
)

func TestReadDocs(t *testing.T) {
	f := newFixture(t)
	f.mcp.docs = docs.New(fstest.MapFS{
		"index.html":                    {Data: []byte("<h1>Docs</h1>")},
		"index.md":                      {Data: []byte("# BrowserPier docs\n\n> What it is.\n\nIntro.\n")},
		"reference/handoffs/index.html": {Data: []byte("<h1>Handoffs API</h1>")},
		"reference/handoffs.md": {Data: []byte("# Handoffs API\n\n> Ask a person to take over.\n\n" +
			"## Ask for a handoff\n\n`POST /api/sessions/{id}/handoff`\n\n" +
			"## Errors\n\n| 409 | `no handoff PIN is set` |\n\n" + strings.Repeat("Long line of reference text.\n", 2000))},
	})
	cs := f.connect(f.scoped, versions[0], nil)

	// The list, with each page's description.
	res, out := call(t, cs, "read_docs", nil)
	if res.IsError || !strings.Contains(out, "BrowserPier test docs") ||
		!strings.Contains(out, "- index: BrowserPier docs. What it is.") ||
		!strings.Contains(out, "- reference/handoffs: Handoffs API. Ask a person to take over.") {
		t.Fatalf("list: %q", out)
	}

	// A page, by its path or its link; a long one is cut.
	for _, page := range []string{"reference/handoffs", "/docs/reference/handoffs/#errors"} {
		res, out = call(t, cs, "read_docs", map[string]any{"page": page})
		if res.IsError || !strings.HasPrefix(out, "# Handoffs API") || !strings.Contains(out, "The page goes on") || len(out) > maxDocsPage+200 {
			t.Fatalf("page %q: error %v, %d characters", page, res.IsError, len(out))
		}
	}
	res, out = call(t, cs, "read_docs", map[string]any{"page": "nothing"})
	if !res.IsError || !strings.Contains(out, `no docs page "nothing"`) {
		t.Fatalf("unknown page: %v %q", res.IsError, out)
	}

	// A search, by an error message: the section, and where it is.
	res, out = call(t, cs, "read_docs", map[string]any{"query": "no handoff PIN is set"})
	if res.IsError || !strings.HasPrefix(out, `From "Handoffs API" (page reference/handoffs#errors):`) || !strings.Contains(out, "## Errors") {
		t.Fatalf("search: %q", out)
	}
	if _, out = call(t, cs, "read_docs", map[string]any{"query": "zzzz"}); !strings.Contains(out, "Nothing in the docs matches") {
		t.Fatalf("no match: %q", out)
	}

	// A binary built without its docs says where they are.
	f.mcp.docs = docs.New(fstest.MapFS{".gitkeep": {}})
	if _, out = call(t, cs, "read_docs", map[string]any{"query": "handoff"}); !strings.Contains(out, "built without its docs") || !strings.Contains(out, docs.Online) {
		t.Fatalf("not built: %q", out)
	}
}

func TestCut(t *testing.T) {
	if got := cut("short", 10); got != "short" {
		t.Fatalf("short: %q", got)
	}
	// Never inside a character: é is two bytes.
	if got := cut("aé", 2); got != "a\n…" {
		t.Fatalf("utf-8: %q", got)
	}
	// At a line break near the end.
	if got := cut("line one\nline two\nline three", 22); got != "line one\nline two\n…" {
		t.Fatalf("line break: %q", got)
	}
}
