package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
)

// The file chooser tool (PROJ-152): a page's file chooser waits, with no
// dialog on the screen, until choose_files answers it with uploads or
// cancels it.
func (s *Server) addChooserTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "choose_files", Description: "Answer a page's file chooser, either mode: when a click opens one (a file input, or a page's own upload button), no dialog shows; the answer of that action, or the next, ends with a file chooser is open (c3), and list_files lists the open ones with what they accept. " +
		"Give the chooser's id and the names of files you put in the uploads with upload_file (one file unless the chooser takes several, at most 10); an empty files list cancels it. Only uploads can be chosen. An unanswered chooser is cancelled after 5 minutes. Refused while a person has the browser."}, s.chooseFiles)
}

type chooseArgs struct {
	SessionID string   `json:"session_id" jsonschema:"from start_browser"`
	ID        string   `json:"id" jsonschema:"the chooser's id, like c3"`
	Files     []string `json:"files" jsonschema:"names returned by upload_file; empty cancels the chooser"`
}

func (s *Server) chooseFiles(ctx context.Context, req *sdk.CallToolRequest, in chooseArgs) (*sdk.CallToolResult, any, error) {
	body := map[string]any{"id": in.ID, "files": in.Files}
	if len(in.Files) == 0 {
		body = map[string]any{"id": in.ID, "cancel": true}
	}
	r, err := s.call(ctx, req.Extra, "POST", "/api/sessions/"+url.PathEscape(in.SessionID)+"/chooser", body)
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		s.forgetGone(in.SessionID, r)
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

// choosersText is the open choosers of the profile's running session, in
// words (the accept lists already fenced by the API), or "" when it has
// none, isn't running, or can't say.
func (s *Server) choosersText(ctx context.Context, extra *sdk.RequestExtra, profile string) string {
	var list struct {
		Sessions []struct {
			ID      string `json:"id"`
			Profile string `json:"profile"`
			State   string `json:"state"`
		} `json:"sessions"`
	}
	if s.callJSON(ctx, extra, "GET", "/api/sessions", nil, &list) != nil {
		return ""
	}
	for _, se := range list.Sessions {
		if se.Profile != profile || (se.State != "running" && se.State != "needs_help") {
			continue
		}
		var out struct {
			Choosers []json.RawMessage `json:"choosers"`
			Text     string            `json:"text"`
		}
		if s.callJSON(ctx, extra, "GET", "/api/sessions/"+url.PathEscape(se.ID)+"/chooser", nil, &out) != nil || len(out.Choosers) == 0 {
			return ""
		}
		return out.Text
	}
	return ""
}

// chooserText is a Visual action's chooser notice (the API's
// X-File-Chooser header), BrowserPier's own line; "" if there is none.
func chooserText(h http.Header) string {
	line := h.Get(api.ChooserHeader)
	if !strings.HasPrefix(line, "a file chooser is open (") && !strings.HasPrefix(line, "file choosers are open (") {
		return ""
	}
	return line
}
