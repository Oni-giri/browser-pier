package mcp

import (
	"context"
	"encoding/json"
	"net/url"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The API-mode page tools: each calls its REST endpoint in-process and
// returns its text, already fenced by the API.
func (s *Server) addPageTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "snapshot", Description: "API mode only. Read the page: URL, title, controls with refs (like a7e12) and text, within a budget; the open dialog and what's in view come first. Hidden text a person can't see may still appear: it is fenced like the rest."}, s.snapshot)
	sdk.AddTool(srv, &sdk.Tool{Name: "find", Description: "API mode only. Find text on the page (case-insensitive), with the text around each match and the nearest ref."}, s.find)
	sdk.AddTool(srv, &sdk.Tool{Name: "navigate", Description: "API mode only. Open an http(s) URL in the active tab, or go back, forward or reload; waits for the page to load. The tab's refs go stale."}, s.navigate)
}

// pageCall calls a page tool's endpoint and returns its text.
func (s *Server) pageCall(ctx context.Context, req *sdk.CallToolRequest, id, tool string, body any) (*sdk.CallToolResult, any, error) {
	r, err := s.call(ctx, req.Extra, "POST", "/api/sessions/"+url.PathEscape(id)+"/"+tool, body)
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		return nil, nil, toolError(r.errorText())
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		return nil, nil, err
	}
	return text("%s", out.Text), nil, nil
}

type snapshotArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	MaxChars  *int   `json:"max_chars,omitempty" jsonschema:"budget for controls and text, default 4000, at most 20000; 0 for only the URL and title"`
}

func (s *Server) snapshot(ctx context.Context, req *sdk.CallToolRequest, in snapshotArgs) (*sdk.CallToolResult, any, error) {
	return s.pageCall(ctx, req, in.SessionID, "snapshot", map[string]any{"max_chars": in.MaxChars})
}

type findArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	Text      string `json:"text" jsonschema:"what to look for"`
	Limit     int    `json:"limit,omitempty" jsonschema:"most matches to return, default 10, at most 50"`
}

func (s *Server) find(ctx context.Context, req *sdk.CallToolRequest, in findArgs) (*sdk.CallToolResult, any, error) {
	return s.pageCall(ctx, req, in.SessionID, "find", map[string]any{"text": in.Text, "limit": in.Limit})
}

type navigateArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	URL       string `json:"url,omitempty" jsonschema:"an http or https URL to open"`
	Action    string `json:"action,omitempty" jsonschema:"instead of a URL: back, forward or reload"`
}

func (s *Server) navigate(ctx context.Context, req *sdk.CallToolRequest, in navigateArgs) (*sdk.CallToolResult, any, error) {
	return s.pageCall(ctx, req, in.SessionID, "navigate", map[string]any{"url": in.URL, "action": in.Action})
}
