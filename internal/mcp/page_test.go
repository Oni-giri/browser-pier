package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/pagetools"
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
	// PROJ-151: what an agent must do itself after a shortcut.
	if !strings.Contains(desc["press_keys"], "look at the screen before you type") {
		t.Errorf("press_keys: %q", desc["press_keys"])
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

// act's MCP steps carry every field the REST step takes, by the same
// name, and a cover_wait_ms of 0 (don't wait) gets through.
func TestActStepsMatchREST(t *testing.T) {
	names := func(v any) []string {
		var out []string
		ty := reflect.TypeOf(v)
		for i := range ty.NumField() {
			out = append(out, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
		}
		slices.Sort(out)
		return out
	}
	if m, r := names(stepArgs{}), names(pagetools.Step{}); !slices.Equal(m, r) {
		t.Errorf("MCP step fields %v, REST step fields %v", m, r)
	}
	// So do act's and snapshot's own options (PROJ-114), and wait_for's
	// (PROJ-122), but session_id.
	without := func(v any) []string {
		return slices.DeleteFunc(names(v), func(n string) bool { return n == "session_id" })
	}
	if m, r := without(actArgs{}), names(pagetools.ActIn{}); !slices.Equal(m, r) {
		t.Errorf("MCP act fields %v, REST act fields %v", m, r)
	}
	if m, r := without(snapshotArgs{}), names(pagetools.SnapshotIn{}); !slices.Equal(m, r) {
		t.Errorf("MCP snapshot fields %v, REST snapshot fields %v", m, r)
	}
	if m, r := without(waitForArgs{}), names(pagetools.WaitIn{}); !slices.Equal(m, r) {
		t.Errorf("MCP wait_for fields %v, REST wait fields %v", m, r)
	}
	// changes: false gets through, unlike an unset one.
	no := false
	raw, _ := json.Marshal(map[string]any{"steps": nil, "changes": actArgs{Changes: &no}.Changes})
	var in pagetools.ActIn
	if err := json.Unmarshal(raw, &in); err != nil || in.Changes == nil || *in.Changes {
		t.Errorf("changes false became %s", raw)
	}
	raw, _ = json.Marshal(map[string]any{"steps": nil, "changes": actArgs{}.Changes})
	if in = (pagetools.ActIn{}); json.Unmarshal(raw, &in) != nil || in.Changes != nil {
		t.Errorf("unset changes became %s", raw)
	}
	zero := 0
	raw, _ = json.Marshal(stepArgs{Action: "click", Ref: "a1e2", CoverWaitMS: &zero})
	var st pagetools.Step
	if err := json.Unmarshal(raw, &st); err != nil || st.CoverWaitMS == nil || *st.CoverWaitMS != 0 {
		t.Errorf("cover_wait_ms 0 became %s", raw)
	}
}
