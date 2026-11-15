package mcp

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/files"
)

func TestUploadFile(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	content := []byte("%PDF-1.7 \x00\xff binary bytes")
	res, out := call(t, cs, "upload_file", map[string]any{"session_id": s.ID, "name": "report.pdf", "content_base64": base64.StdEncoding.EncodeToString(content)})
	if res.IsError || !strings.Contains(out, "name: report.pdf") || !strings.Contains(out, `"action":"upload"`) || !strings.Contains(out, `"files":["report.pdf"]`) {
		t.Fatalf("upload_file: %s", out)
	}
	dir := filepath.Join(f.m.Layout().FilesDir("google"), files.Uploads)
	if got, err := os.ReadFile(filepath.Join(dir, "report.pdf")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("the file holds %q, %v", got, err)
	}
	// The same name again: kept apart, and the new name returned. Unpadded
	// base64 is taken too.
	res, out = call(t, cs, "upload_file", map[string]any{"session_id": s.ID, "name": "report.pdf", "content_base64": base64.RawStdEncoding.EncodeToString([]byte("v2"))})
	if res.IsError || !strings.Contains(out, "name: report (2).pdf") || !strings.Contains(out, "report.pdf was taken") {
		t.Fatalf("second upload_file: %s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "report (2).pdf")); string(got) != "v2" {
		t.Errorf("the second file holds %q", got)
	}
	// Two dots inside a name are just dots, as for the REST upload and
	// act's upload step.
	if res, out = call(t, cs, "upload_file", map[string]any{"session_id": s.ID, "name": "report..v2.pdf", "content_base64": base64.StdEncoding.EncodeToString([]byte("v2"))}); res.IsError || !strings.Contains(out, "name: report..v2.pdf") {
		t.Fatalf("upload_file report..v2.pdf: %s", out)
	}
	// The REST endpoint's activity entry.
	logged := false
	for _, a := range f.m.Activity(s.ID) {
		logged = logged || a.What == "Uploaded report.pdf"
	}
	if !logged {
		t.Errorf("activity: %+v", f.m.Activity(s.ID))
	}
}

func TestUploadFileRefusals(t *testing.T) {
	f := newFixture(t)
	f.mcp.maxUpload = 1000
	cs := f.connect(f.all, versions[0], nil)
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), n)) }
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"bad base64", map[string]any{"session_id": s.ID, "name": "a.txt", "content_base64": "not base64!!"}, "isn't valid base64"},
		{"over the cap", map[string]any{"session_id": s.ID, "name": "a.txt", "content_base64": b64(1001)}, "over 1000 bytes"},
		{"far over the cap", map[string]any{"session_id": s.ID, "name": "a.txt", "content_base64": b64(5000)}, "over 1000 bytes"},
		{"a path", map[string]any{"session_id": s.ID, "name": "../a.txt", "content_base64": b64(1)}, "plain file name"},
		{"dot dot", map[string]any{"session_id": s.ID, "name": "..", "content_base64": b64(1)}, "plain file name"},
		{"hidden", map[string]any{"session_id": s.ID, "name": ".bashrc", "content_base64": b64(1)}, "plain file name"},
		{"no session", map[string]any{"session_id": "nope", "name": "a.txt", "content_base64": b64(1)}, "no such session"},
	} {
		res, out := call(t, cs, "upload_file", c.args)
		if !res.IsError || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v %s", c.name, res.IsError, out)
		}
	}
	// At the cap exactly, it goes in.
	if res, out := call(t, cs, "upload_file", map[string]any{"session_id": s.ID, "name": "a.txt", "content_base64": b64(1000)}); res.IsError {
		t.Errorf("at the cap: %s", out)
	}
	// Wrapped base64 (MIME's 76-character lines, a space or a tab
	// besides) at the cap: the whitespace isn't counted, nor decoded.
	content := bytes.Repeat([]byte("y"), 1000)
	enc := base64.StdEncoding.EncodeToString(content)
	var wrapped strings.Builder
	for i := 0; i < len(enc); i += 76 {
		wrapped.WriteString(enc[i:min(i+76, len(enc))])
		wrapped.WriteString([]string{"\r\n", "\n", " \t"}[i/76%3])
	}
	if res, out := call(t, cs, "upload_file", map[string]any{"session_id": s.ID, "name": "wrapped.txt", "content_base64": wrapped.String()}); res.IsError || !strings.Contains(out, "(1000 bytes)") {
		t.Errorf("wrapped base64 at the cap: %s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(f.m.Layout().FilesDir("google"), files.Uploads, "wrapped.txt")); !bytes.Equal(got, content) {
		t.Errorf("the wrapped upload holds %q", got)
	}
	// Another token's session is 404, as everywhere: the scoped token
	// sees google only, so github's session doesn't exist for it.
	call(t, cs, "start_browser", map[string]any{"profile": "github"})
	gh, _ := f.m.Live("github")
	scoped := f.connect(f.scoped, versions[0], nil)
	res, out := call(t, scoped, "upload_file", map[string]any{"session_id": gh.ID, "name": "a.txt", "content_base64": b64(1)})
	if !res.IsError || !strings.Contains(out, "no such session") {
		t.Errorf("another token's session: %s", out)
	}
	if _, err := os.Stat(filepath.Join(f.m.Layout().FilesDir("github"), files.Uploads, "a.txt")); err == nil {
		t.Error("the scoped token wrote into github's uploads")
	}
}

// TestUploadFileFitsInAnMCPMessage: the SDK takes 4 MiB requests by
// default; a 5 MiB file (6.7 MiB of base64) still reaches the REST
// endpoint, whose answer here is the fixture's 1 MiB files cap.
func TestUploadFileFitsInAnMCPMessage(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	call(t, cs, "start_browser", map[string]any{"profile": "google"})
	s, _ := f.m.Live("google")
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 5<<20))
	res, out := call(t, cs, "upload_file", map[string]any{"session_id": s.ID, "name": "big.bin", "content_base64": big})
	if !res.IsError || !strings.Contains(out, "files cap") {
		t.Errorf("a 5 MiB upload: %v %s", res.IsError, out)
	}
}
