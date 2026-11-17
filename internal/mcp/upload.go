package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/files"
)

// MaxUpload caps one upload_file (PROJ-117): an MCP message carries the
// file whole, in base64 (a third larger), and is held in memory while it
// is decoded, so it is kept well under the REST endpoint's 100 MiB.
const MaxUpload = 20 << 20

func (s *Server) addUploadTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "upload_file", Description: fmt.Sprintf("Put a file into the session's uploads, at most %s, for a page's file input: then set it with act's upload step "+
		`({"action": "upload", "ref": <the <input type=file> or its label>, "files": [<the name this returns>]}). A person can also pick it in the file chooser during a handoff. The name may come back changed if one is taken.`, mib(s.maxUpload))}, s.uploadFile)
}

type uploadArgs struct {
	SessionID     string `json:"session_id" jsonschema:"from start_browser"`
	Name          string `json:"name" jsonschema:"the file's name, like report.pdf: no folders, not starting with a dot"`
	ContentBase64 string `json:"content_base64" jsonschema:"the file's bytes, in standard base64 (line breaks and spaces are ignored)"`
}

// uploadFile writes the file through POST /api/sessions/{id}/files, the
// REST endpoint's own code: its scopes, the profile's files cap and
// eviction, and its activity log entry.
func (s *Server) uploadFile(ctx context.Context, req *sdk.CallToolRequest, in uploadArgs) (*sdk.CallToolResult, any, error) {
	if !files.ValidName(in.Name) {
		return nil, nil, toolError("name: use a plain file name, like report.pdf: no folders, not starting with a dot")
	}
	max := s.maxUpload
	// Wrapped base64 (MIME's 76-character lines) is common: whitespace
	// is neither counted against the cap nor decoded.
	if strings.ContainsAny(in.ContentBase64, " \t\r\n") {
		in.ContentBase64 = strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, in.ContentBase64)
	}
	if int64(base64.StdEncoding.DecodedLen(len(in.ContentBase64))) > max+2 {
		return nil, nil, toolError(fmt.Sprintf("the file is over %s; upload_file takes at most that (POST /api/sessions/%s/files takes up to 100 MiB)", mib(max), in.SessionID))
	}
	b, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		// Padding is often left off.
		var rerr error
		if b, rerr = base64.RawStdEncoding.DecodeString(strings.TrimRight(in.ContentBase64, "=")); rerr != nil {
			return nil, nil, toolError("content_base64 isn't valid base64 (" + err.Error() + "); send the file's bytes in standard base64")
		}
	}
	if int64(len(b)) > max {
		return nil, nil, toolError(fmt.Sprintf("the file is over %s; upload_file takes at most that (POST /api/sessions/%s/files takes up to 100 MiB)", mib(max), in.SessionID))
	}
	r, err := s.callRaw(ctx, req.Extra, "POST", "/api/sessions/"+url.PathEscape(in.SessionID)+"/files?name="+url.QueryEscape(in.Name), "application/octet-stream", bytes.NewReader(b), nil)
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		s.forgetGone(in.SessionID, r)
		return nil, nil, toolError(r.errorText())
	}
	var out struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		return nil, nil, err
	}
	step, _ := json.Marshal(map[string]any{"action": "upload", "ref": "…", "files": []string{out.Name}})
	msg := fmt.Sprintf("Uploaded %s (%d bytes). name: %s", out.Name, out.Size, out.Name)
	if out.Name != in.Name {
		msg += fmt.Sprintf(" (%s was taken)", in.Name)
	}
	msg += fmt.Sprintf(".\nSet it on a file input with act: %s, the ref being the <input type=file> or its <label>.", step)
	return text("%s", msg), nil, nil
}

func mib(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}
