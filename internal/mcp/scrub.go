package mcp

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// SecretsFunc gives the Scrubber for the secrets BrowserPier typed into
// the live browsers of the profiles allow accepts (sessions.Manager's
// Scrubber).
type SecretsFunc func(allow func(profile string) bool) *scrub.Scrubber

// SetSecrets makes every tool result scrubbed of those secrets (PROJ-125).
// The REST answers the tools read are scrubbed already; this is the last
// step again, after the tool has turned them into its text, so nothing a
// tool decodes or joins (a URL, an X-Downloads name) can bring one back.
func (s *Server) SetSecrets(f SecretsFunc) { s.secrets = f }

// scrubResults is the middleware every MCP server of this endpoint runs:
// it scrubs each tools/call result, whatever the tool, with the secrets of
// the token's own profiles as they are when the call comes in and when it
// ends. A tool can't skip it: it wraps the server's method handler, not
// the tools.
func (s *Server) scrubResults(tok auth.Token) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method != "tools/call" || s.secrets == nil {
				return next(ctx, method, req)
			}
			// The token as it is now: a deleted profile leaves its scope.
			// If it can't be read (revoked), the API refuses the call anyway.
			t := tok
			if now, err := s.web.Tokens().Get(ctx, tok.ID); err == nil {
				t = now
			}
			before := s.secrets(t.Allows)
			res, err := next(ctx, method, req)
			sc := scrub.Union(before, s.secrets(t.Allows))
			if sc == nil {
				return res, err
			}
			if err != nil {
				if msg := err.Error(); sc.String(msg) != msg {
					err = errors.New(sc.String(msg))
				}
			}
			if r, ok := res.(*sdk.CallToolResult); ok && r != nil {
				scrubResult(sc, r)
			}
			return res, err
		}
	}
}

// scrubResult scrubs a tool result's text in place: text blocks, a text
// resource, a resource link's words and any structured content. Images and
// a PDF's bytes go as they are: no secret can be taken out of them.
func scrubResult(sc *scrub.Scrubber, r *sdk.CallToolResult) {
	for _, c := range r.Content {
		switch c := c.(type) {
		case *sdk.TextContent:
			c.Text = sc.String(c.Text)
		case *sdk.EmbeddedResource:
			if c.Resource != nil {
				c.Resource.URI = sc.String(c.Resource.URI)
				c.Resource.Text = sc.String(c.Resource.Text)
			}
		case *sdk.ResourceLink:
			c.URI, c.Name, c.Title, c.Description = sc.String(c.URI), sc.String(c.Name), sc.String(c.Title), sc.String(c.Description)
		}
	}
	if r.StructuredContent != nil {
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil {
			r.StructuredContent = nil // fail closed
			return
		}
		var v any
		if json.Unmarshal(sc.JSON(raw), &v) == nil {
			r.StructuredContent = v
		} else {
			r.StructuredContent = nil
		}
	}
}
