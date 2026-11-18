//go:build docker

package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestChooserAnsweredByAnAgentAPI (PROJ-152): in API mode, act clicks a
// page's own button that opens a file chooser, and choose_files answers
// it with an upload, over MCP.
func TestChooserAnsweredByAnAgentAPI(t *testing.T) {
	env := newPageEnv(t, "cha")
	site := browsertest.NewPageSite(t, env.host)
	tool := func(name string, args map[string]any) string {
		t.Helper()
		res, out := call(t, env.cs, name, args)
		if res.IsError {
			t.Fatalf("%s %v: %s", name, args, out)
		}
		return out
	}
	env.m.SetFileChooserPortal(true)
	tool("start_browser", map[string]any{"profile": env.name})
	live, _ := env.m.Live(env.name)
	id := live.ID
	content := make([]byte, 50<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	tool("upload_file", map[string]any{"session_id": id, "name": "cv.pdf", "content_base64": base64.StdEncoding.EncodeToString(content)})
	tool("navigate", map[string]any{"session_id": id, "url": site.URL + "/chooser"})
	out := tool("act", map[string]any{"session_id": id, "steps": []any{map[string]any{"action": "click", "target": `role=button[name="Choose a file"]`}}})
	t.Logf("act: %s", out)
	re := regexp.MustCompile(`a file chooser is open \((c[0-9]+)\): choose_files answers or cancels it`)
	m := re.FindStringSubmatch(out)
	for deadline := time.Now().Add(10 * time.Second); m == nil; time.Sleep(100 * time.Millisecond) {
		// It may open an action late: list_files lists it.
		files := tool("list_files", map[string]any{"profile": env.name})
		if mm := regexp.MustCompile(`(?m)^(c[0-9]+): one file`).FindStringSubmatch(files); mm != nil {
			m = mm
		}
		if time.Now().After(deadline) {
			t.Fatalf("no chooser opened: %s", files)
		}
	}
	if out := tool("choose_files", map[string]any{"session_id": id, "id": m[1], "files": []string{"cv.pdf"}}); !strings.Contains(out, m[1]) {
		t.Fatalf("choose_files: %s", out)
	}
	for deadline := time.Now().Add(10 * time.Second); !site.HasEvent("b change 1"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the page never got the file: %q", site.Events())
		}
	}
	tool("act", map[string]any{"session_id": id, "steps": []any{map[string]any{"action": "click", "target": `role=button[name="Send"]`}}})
	for deadline := time.Now().Add(10 * time.Second); len(site.Uploads()) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the form never posted")
		}
	}
	got := site.Uploads()
	if len(got) != 1 || got[0].Field != "b" || got[0].Name != "cv.pdf" || got[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the site got %+v", got)
	}
}
