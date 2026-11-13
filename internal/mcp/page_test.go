package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// pageTools are the API-mode page tools, which say so, with valid arguments.
var pageTools = map[string]map[string]any{
	"snapshot": {},
	"find":     {"text": "x"},
	"navigate": {"url": "https://example.com"},
	"act":      {"steps": []any{map[string]any{"action": "press", "keys": "Enter"}}},
	"wait_for": {"text": "x"},
	"tabs":     {},
}

func TestPageToolsAreAPIModeOnly(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]string{}
	for _, tl := range list.Tools {
		desc[tl.Name] = tl.Description
	}
	for name := range pageTools {
		if !strings.HasPrefix(desc[name], "API mode only.") {
			t.Errorf("%s: %q", name, desc[name])
		}
	}
	// The fixture's browsers start in Visual mode: the API answers 409.
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	for name, args := range pageTools {
		args["session_id"] = s.ID
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if !res.IsError || !strings.Contains(text, "Visual mode") {
			t.Errorf("%s in Visual mode: %v %s", name, res.IsError, text)
		}
	}
}

func TestPageContent(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	// Visual mode: the text formats and PDF need CDP; a screenshot doesn't.
	for _, format := range []string{"markdown", "readability", "pdf"} {
		res, out := call(t, cs, "page_content", map[string]any{"session_id": s.ID, "format": format})
		if !res.IsError || !strings.Contains(out, "Visual mode") {
			t.Errorf("%s in Visual mode: %v %s", format, res.IsError, out)
		}
	}
	res, out := call(t, cs, "page_content", map[string]any{"session_id": s.ID, "format": "screenshot"})
	if res.IsError || !strings.Contains(out, "1152×720") || len(res.Content) != 2 {
		t.Fatalf("screenshot: %s", out)
	}
	if _, ok := res.Content[1].(*sdk.ImageContent); !ok {
		t.Errorf("no image: %T", res.Content[1])
	}
	if res, out := call(t, cs, "page_content", map[string]any{"session_id": s.ID, "format": "html"}); !res.IsError || !strings.Contains(out, "format") {
		t.Errorf("format html: %s", out)
	}
}
