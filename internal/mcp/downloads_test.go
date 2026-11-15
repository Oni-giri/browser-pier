package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/files"
)

// click says what it downloaded (the API's X-Downloads), fenced, with or
// without the screenshot after it.
func TestClickSaysWhatItDownloaded(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	dir := filepath.Join(f.m.Layout().FilesDir("google"), files.Downloads)
	n := 0
	f.mu.Lock()
	f.onClick = func() {
		n++
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "report-"+string(rune('0'+n))+".csv"), []byte("a,b\n1,2\n"), 0o600)
	}
	f.mu.Unlock()

	res, out := call(t, cs, "click", map[string]any{"session_id": s.ID, "x": 10, "y": 10, "download_wait_ms": 0})
	if res.IsError || !strings.HasPrefix(out, "Clicked.\nDownloads:\n<untrusted-content-") || !strings.Contains(out, "\ndownloaded report-1.csv (8 B)\n") {
		t.Fatalf("click: %s", out)
	}
	res, _ = call(t, cs, "click", map[string]any{"session_id": s.ID, "x": 10, "y": 10, "screenshot": true})
	if res.IsError || len(res.Content) != 3 {
		t.Fatalf("click with a screenshot: %d parts", len(res.Content))
	}
	if _, ok := res.Content[1].(*sdk.ImageContent); !ok {
		t.Fatalf("no screenshot: %T", res.Content[1])
	}
	if tc, ok := res.Content[2].(*sdk.TextContent); !ok || !strings.Contains(tc.Text, "downloaded report-2.csv (8 B)") {
		t.Fatalf("no notice after the screenshot: %+v", res.Content[2])
	}
	// Nothing downloaded: the answer is as it was.
	f.mu.Lock()
	f.onClick = nil
	f.mu.Unlock()
	if _, out := call(t, cs, "click", map[string]any{"session_id": s.ID, "x": 10, "y": 10}); out != "Clicked." {
		t.Fatalf("click without a download: %q", out)
	}
	// The browser never sees download_wait_ms.
	f.mu.Lock()
	clicks := append([]string(nil), f.clicks...)
	f.mu.Unlock()
	for _, c := range clicks {
		if strings.Contains(c, "download") {
			t.Errorf("the browser got %s", c)
		}
	}
	if res, out := call(t, cs, "click", map[string]any{"session_id": s.ID, "x": 10, "y": 10, "download_wait_ms": 40000}); !res.IsError || !strings.Contains(out, "download_wait_ms") {
		t.Errorf("download_wait_ms 40000: %s", out)
	}
}
