package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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
		"otherwise send the link yourself and call wait_for_handoff. " +
		"If you know where the task ends (the account page after a sign-in), give until_url: the handoff then also ends by itself, as done, " +
		"once the tab in front navigates to an address with that text in its origin and path, after the person opened the link, so you don't wait for a person who forgot to tap Done."}, s.requestHandoff)
	sdk.AddTool(srv, &sdk.Tool{Name: "wait_for_handoff", Description: "Wait for the person to hand back. Returns at once if they have; call it again while it says pending."}, s.waitForHandoff)
	sdk.AddTool(srv, &sdk.Tool{Name: "cancel_handoff", Description: "Withdraw a handoff nobody will take: the link stops working and you get the browser back."}, s.cancelHandoff)
}

type handoffArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	Reason    string `json:"reason" jsonschema:"sensitive_input (login, 2FA, payment), captcha, file_upload, review or manual_step"`
	Detail    string `json:"detail" jsonschema:"what the person should do, in one sentence: Enter the 2FA code Google sent to your phone"`
	Require   string `json:"require,omitempty" jsonschema:"ask for a stricter check than the profile's: pin or signin"`
	UntilURL  string `json:"until_url,omitempty" jsonschema:"optional: end the handoff by itself when the tab in front navigates to an address whose origin and path contain this text, such as example.com/account or /dashboard; never the query or fragment, so no ? or #; 4 to 200 characters, naming a page (not just / or https://). A page already there when the person opens the link doesn't count"`
}

type handoffOut struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	URL       string    `json:"url"`
	Origin    string    `json:"origin"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note"`
	Message   string    `json:"message"`
	Reached   bool      `json:"until_url_reached"`
}

// requestHandoff opens a handoff. A client that can open URLs for its user
// (URL elicitation) is asked to show the link, and the call waits for the
// hand-back; any other, or one that doesn't show it, gives the agent the
// link to pass on itself.
func (s *Server) requestHandoff(ctx context.Context, req *sdk.CallToolRequest, in handoffArgs) (*sdk.CallToolResult, any, error) {
	var h handoffOut
	body := map[string]string{"reason": in.Reason, "detail": in.Detail, "require": in.Require, "until_url": in.UntilURL}
	if err := s.callJSON(ctx, req.Extra, "POST", "/api/sessions/"+url.PathEscape(in.SessionID)+"/handoff", body, &h); err != nil {
		return nil, nil, err
	}
	fallback := func(why string) (*sdk.CallToolResult, any, error) {
		return text("%sSend this link to the person through your own channel: %s\n"+
			"It works once, for 15 minutes. Then call wait_for_handoff with handoff_id %s; you're paused until they hand back. "+
			"If nobody will take it, call cancel_handoff.", why, h.URL, h.ID), nil, nil
	}
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil || caps.Elicitation.URL == nil || req.Session == nil {
		return fallback("")
	}
	msg := "Your agent needs you in its browser"
	if in.Detail != "" {
		msg += ": " + in.Detail
	}
	if h.Origin != "" {
		msg += " (" + h.Origin + ")"
	}
	ectx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	res, err := req.Session.Elicit(ectx, &sdk.ElicitParams{Mode: "url", URL: h.URL, ElicitationID: h.ID,
		Message: msg + ". The link works once, for 15 minutes."})
	cancel()
	switch {
	case err != nil:
		// The handoff is open whatever the client did: never leave the
		// agent without its ID, or the browser waits 15 minutes for nobody.
		return fallback("Your app couldn't show the link. ")
	case res.Action != "accept":
		// Headless clients decline without asking anyone.
		return fallback("The link wasn't opened in your app (" + res.Action + "). If the person is elsewhere: ")
	}
	return s.waitAndReport(ctx, req, h.ID, inCallWait)
}

type handoffIDArgs struct {
	HandoffID string `json:"handoff_id" jsonschema:"from request_handoff"`
}

func (s *Server) cancelHandoff(ctx context.Context, req *sdk.CallToolRequest, in handoffIDArgs) (*sdk.CallToolResult, any, error) {
	var h handoffOut
	if err := s.callJSON(ctx, req.Extra, "DELETE", "/api/handoffs/"+url.PathEscape(in.HandoffID), nil, &h); err != nil {
		return nil, nil, err
	}
	if h.Status != "cancelled" {
		return text("It had already ended: %s. %s", h.Status, h.Message), nil, nil
	}
	return text("Withdrawn. %s", h.Message), nil, nil
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
		secs := 0
		if step > 0 {
			secs = int((step + time.Second - 1) / time.Second) // up: a zero wait would spin
		}
		path := "/api/handoffs/" + url.PathEscape(id) + "?wait=" + strconv.Itoa(secs)
		if err := s.callJSON(ctx, req.Extra, "GET", path, nil, &h); err != nil {
			return nil, nil, err
		}
		if h.Status != "pending" {
			if req.Session != nil {
				_ = req.Session.NotifyElicitationComplete(ctx, &sdk.ElicitationCompleteParams{ElicitationID: id})
			}
			out := fmt.Sprintf("Handoff %s: %s", h.Status, h.Message)
			if !strings.HasSuffix(out, ".") {
				out += "."
			}
			if h.Note != "" && !h.Reached { // a reached until_url's note is BrowserPier's, said in the message
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
