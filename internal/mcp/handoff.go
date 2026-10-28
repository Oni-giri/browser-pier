package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// inCallWait is how long a tool call waits for a person before answering
// "still pending": long calls get cut by clients and reverse proxies, so
// the agent waits on in turns with wait_for_handoff.
const inCallWait = 4 * time.Minute

func (s *Server) addHandoffTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "request_handoff", Description: "Ask a person to take over the browser: for a login, 2FA, a CAPTCHA, a payment, a file upload or a check. " +
		"They get a one-time link; you're paused until they hand back. Clients that can open links show it to the user and wait; " +
		"otherwise send the link yourself and call wait_for_handoff."}, s.requestHandoff)
	sdk.AddTool(srv, &sdk.Tool{Name: "wait_for_handoff", Description: "Wait for the person to hand back. Returns at once if they have; call it again while it says pending."}, s.waitForHandoff)
}

type handoffArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	Reason    string `json:"reason" jsonschema:"sensitive_input (login, 2FA, payment), captcha, file_upload, review or manual_step"`
	Detail    string `json:"detail" jsonschema:"what the person should do, in one sentence: Enter the 2FA code Google sent to your phone"`
	Require   string `json:"require,omitempty" jsonschema:"ask for a stricter check than the profile's: pin or signin"`
}

type handoffOut struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	URL       string    `json:"url"`
	Origin    string    `json:"origin"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note"`
	Message   string    `json:"message"`
}

// The elicitation's ID in a multi-round-trip result.
const elicitKey = "handoff"

// requestHandoff opens a handoff. A client that can open URLs for its user
// (URL elicitation) shows the link and the call waits for the hand-back;
// any other gets the link to pass on itself.
func (s *Server) requestHandoff(ctx context.Context, req *sdk.CallToolRequest, in handoffArgs) (*sdk.CallToolResult, any, error) {
	// Second round: the client answered the elicitation.
	if resp, ok := req.Params.InputResponses[elicitKey]; ok && req.Params.RequestState != "" {
		id := req.Params.RequestState
		if res, ok := resp.(*sdk.ElicitResult); ok && res.Action != "accept" {
			var h handoffOut
			if err := s.callJSON(ctx, req.Extra, "DELETE", "/api/handoffs/"+url.PathEscape(id), map[string]string{"note": "the person declined it in the agent's app"}, &h); err != nil {
				return nil, nil, err
			}
			return text("The person declined to take over. You have the browser back. %s", h.Message), nil, nil
		}
		return s.waitAndReport(ctx, req, id, inCallWait)
	}

	var h handoffOut
	body := map[string]string{"reason": in.Reason, "detail": in.Detail, "require": in.Require}
	if err := s.callJSON(ctx, req.Extra, "POST", "/api/sessions/"+url.PathEscape(in.SessionID)+"/handoff", body, &h); err != nil {
		return nil, nil, err
	}
	if caps := req.ClientCapabilities(); caps != nil && caps.Elicitation != nil && caps.Elicitation.URL != nil {
		msg := "Your agent needs you in its browser"
		if in.Detail != "" {
			msg += ": " + in.Detail
		}
		if h.Origin != "" {
			msg += " (" + h.Origin + ")"
		}
		return &sdk.CallToolResult{
			InputRequests: sdk.InputRequestMap{elicitKey: &sdk.ElicitParams{
				Mode: "url", URL: h.URL, ElicitationID: h.ID,
				Message: msg + ". The link works once, for 15 minutes.",
			}},
			RequestState: h.ID,
		}, nil, nil
	}
	return text("Send this link to the person through your own channel: %s\n"+
		"It works once, for 15 minutes. handoff_id: %s. Then call wait_for_handoff; you're paused until they hand back.", h.URL, h.ID), nil, nil
}

type waitHandoffArgs struct {
	HandoffID string `json:"handoff_id" jsonschema:"from request_handoff"`
	Seconds   int    `json:"seconds,omitempty" jsonschema:"how long to wait at most, up to 240; default 240"`
}

func (s *Server) waitForHandoff(ctx context.Context, req *sdk.CallToolRequest, in waitHandoffArgs) (*sdk.CallToolResult, any, error) {
	wait := inCallWait
	if in.Seconds > 0 {
		wait = min(time.Duration(in.Seconds)*time.Second, inCallWait)
	}
	return s.waitAndReport(ctx, req, in.HandoffID, wait)
}

// waitAndReport long-polls the handoff, in short turns so it can send
// progress (which keeps some clients from timing out), and says how it
// ended or that it's still open.
func (s *Server) waitAndReport(ctx context.Context, req *sdk.CallToolRequest, id string, wait time.Duration) (*sdk.CallToolResult, any, error) {
	deadline := time.Now().Add(wait)
	token := req.Params.GetProgressToken()
	for turn := 1; ; turn++ {
		left := time.Until(deadline)
		step := min(left, 20*time.Second)
		var h handoffOut
		path := "/api/handoffs/" + url.PathEscape(id) + "?wait=" + strconv.Itoa(max(int(step/time.Second), 0))
		if err := s.callJSON(ctx, req.Extra, "GET", path, nil, &h); err != nil {
			return nil, nil, err
		}
		if h.Status != "pending" {
			if req.Session != nil {
				_ = req.Session.NotifyElicitationComplete(ctx, &sdk.ElicitationCompleteParams{ElicitationID: id})
			}
			out := fmt.Sprintf("Handoff %s: %s.", h.Status, h.Message)
			if h.Note != "" {
				out += " The person's note: " + h.Note
			}
			return text("%s", out), nil, nil
		}
		if time.Until(deadline) <= 0 || ctx.Err() != nil {
			return text("Still waiting for the person (handoff_id %s, the link expires at %s). Call wait_for_handoff again.",
				id, h.ExpiresAt.UTC().Format(time.RFC3339)), nil, nil
		}
		if token != nil && req.Session != nil {
			_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: token,
				Progress: float64(turn), Message: "Waiting for the person to hand back"})
		}
	}
}
