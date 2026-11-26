package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Asking how calls went (PROJ-156): each tool reads its REST endpoint in
// the caller's name, so it sees the token's own calls only.
func (s *Server) addCallTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "call_status", Description: "How a call you sent with a call_id went: waiting or running (still under way), ok (it ran), error (it answered with an error after it began, so some of it may have happened) or interrupted (cut short: a person took the browser, it stopped, or the call was cancelled; how far it got isn't known). Its result isn't kept: read the page again. An unknown id doesn't prove it never ran (its receipt goes 1,000 calls later, or when the browser restarts): read the page before sending it again."}, s.callStatus)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_status", Description: "A browser's state and mode, your calls under way in it, and what your last calls did there (newest first, up to 20, with each one's call_id and how it ended), for when an answer was lost."}, s.sessionStatus)
}

type callStatusArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	CallID    string `json:"call_id" jsonschema:"the call_id you sent"`
}

type callOut struct {
	CallID  string     `json:"call_id"`
	Tool    string     `json:"tool"`
	State   string     `json:"state"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended"`
}

// callAdvice is what an agent does next about a call in state.
var callAdvice = map[string]string{
	"waiting":     "It is waiting for its turn; ask again shortly.",
	"running":     "It is still running; ask again shortly.",
	"ok":          "It ran. Its result isn't kept: read the page again to see where things are.",
	"error":       "It answered with an error after it began, so some of it may have happened: read the page, then send what is still needed under a new call_id.",
	"interrupted": "It was cut short; how far it got isn't known: read the page, then send what is still needed under a new call_id.",
}

func (s *Server) callStatus(ctx context.Context, req *sdk.CallToolRequest, in callStatusArgs) (*sdk.CallToolResult, any, error) {
	var out struct {
		Call callOut `json:"call"`
	}
	if err := s.callJSON(ctx, req.Extra, "GET", "/api/sessions/"+url.PathEscape(in.SessionID)+"/calls/"+url.PathEscape(in.CallID), nil, &out); err != nil {
		return nil, nil, err
	}
	c := out.Call
	msg := fmt.Sprintf("Call %s (%s): %s. Started %s", c.CallID, c.Tool, c.State, c.Started.UTC().Format(time.RFC3339))
	if c.Ended != nil {
		msg += ", ended " + c.Ended.UTC().Format(time.RFC3339)
	}
	msg += ". " + callAdvice[c.State]
	return text("%s", msg), nil, nil
}

func (s *Server) sessionStatus(ctx context.Context, req *sdk.CallToolRequest, in sessionArgs) (*sdk.CallToolResult, any, error) {
	var out struct {
		ID           string `json:"id"`
		Profile      string `json:"profile"`
		Mode         string `json:"mode"`
		State        string `json:"state"`
		RunningCalls []struct {
			Tool    string    `json:"tool"`
			CallID  string    `json:"call_id"`
			State   string    `json:"state"`
			Started time.Time `json:"started"`
		} `json:"running_calls"`
		RecentCalls []struct {
			What   string    `json:"what"`
			At     time.Time `json:"at"`
			CallID string    `json:"call_id"`
			State  string    `json:"state"`
		} `json:"recent_calls"`
	}
	if err := s.callJSON(ctx, req.Extra, "GET", "/api/sessions/"+url.PathEscape(in.SessionID), nil, &out); err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s (%s): %s, %s mode.\n", out.ID, out.Profile, out.State, out.Mode)
	if len(out.RunningCalls) == 0 {
		b.WriteString("None of your calls is under way.\n")
	} else {
		b.WriteString("Your calls under way:\n")
		for _, c := range out.RunningCalls {
			fmt.Fprintf(&b, "- %s, %s since %s", c.Tool, c.State, c.Started.UTC().Format(time.RFC3339))
			if c.CallID != "" {
				fmt.Fprintf(&b, " (call_id %s)", c.CallID)
			}
			b.WriteByte('\n')
		}
	}
	if len(out.RecentCalls) == 0 {
		b.WriteString("Nothing of yours in its log yet.")
		return text("%s", b.String()), nil, nil
	}
	b.WriteString("What your last calls did, newest first:\n")
	for _, c := range out.RecentCalls {
		fmt.Fprintf(&b, "- %s %s", c.At.UTC().Format(time.RFC3339), c.What)
		switch {
		case c.CallID != "" && c.State != "":
			fmt.Fprintf(&b, " (call_id %s: %s)", c.CallID, c.State)
		case c.CallID != "":
			fmt.Fprintf(&b, " (call_id %s)", c.CallID)
		}
		b.WriteByte('\n')
	}
	return text("%s", strings.TrimRight(b.String(), "\n")), nil, nil
}
