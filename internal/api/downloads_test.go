package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/files"
)

// downloadOn makes the fake browser download name (content) when an
// input action at path runs, as a click on a link would.
func (f *fixture) downloadOn(profile, path, name, content string) {
	f.t.Helper()
	dir := filepath.Join(f.m.Layout().FilesDir(profile), files.Downloads)
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	f.b.onInput = func(p string) {
		if p != path {
			return
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			f.t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			f.t.Error(err)
		}
	}
}

func notice(t *testing.T, h http.Header) downloadsNotice {
	t.Helper()
	raw := h.Get(DownloadsHeader)
	for _, r := range raw {
		if r > 0x7e || r < 0x20 {
			t.Fatalf("X-Downloads isn't plain ASCII: %q", raw)
		}
	}
	var n downloadsNotice
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("X-Downloads %q: %v", raw, err)
		}
	}
	return n
}

func TestComputerClickReportsADownload(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	f.downloadOn("google", "/input/click", "report.csv", "a,b\n1,2\n")
	code, h, raw := f.computer(s.ID, `{"action": "click", "x": 10, "y": 20}`)
	if code != http.StatusNoContent {
		t.Fatalf("click: %d %s", code, raw)
	}
	if n := notice(t, h); len(n.Files) != 1 || n.Files[0] != (files.Download{Name: "report.csv", Size: 8, State: files.Downloaded}) || n.More != 0 {
		t.Fatalf("notice %+v", n)
	}
	// download_wait_ms is BrowserPier's: the browser doesn't get it.
	if got := f.lastInput(); got != `/input/click {"x":10,"y":20}` {
		t.Errorf("forwarded %s", got)
	}
	if !slices.Contains(f.whats(s.ID), "claude: Downloaded report.csv (8 B)") {
		t.Errorf("activity %q", f.whats(s.ID))
	}

	// With a screenshot, the header comes with it.
	f.downloadOn("google", "/input/click", "second.csv", "x")
	code, h, raw = f.computer(s.ID, `{"action": "click", "x": 10, "y": 20, "screenshot": true, "download_wait_ms": 0}`)
	if code != http.StatusOK || h.Get("Content-Type") != "image/png" {
		t.Fatalf("click with screenshot: %d %.80s", code, raw)
	}
	if n := notice(t, h); len(n.Files) != 1 || n.Files[0].Name != "second.csv" {
		t.Fatalf("notice with screenshot %+v", n)
	}
}

func TestComputerKeyAndClickEndpointReportDownloads(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	// A name a page chose: bidi and zero-width characters go, and what
	// is past ASCII is escaped in the header.
	f.downloadOn("google", "/input/key", "r\u202eésumé\u200b.pdf", "pdf")
	code, h, raw := f.computer(s.ID, `{"action": "key", "keys": "Return"}`)
	if code != http.StatusNoContent {
		t.Fatalf("key: %d %s", code, raw)
	}
	if n := notice(t, h); len(n.Files) != 1 || n.Files[0].Name != "résumé.pdf" {
		t.Fatalf("notice %+v (%q)", n, h.Get(DownloadsHeader))
	}

	f.downloadOn("google", "/input/click", "via-click.csv", "x")
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/sessions/"+s.ID+"/click", strings.NewReader(`{"x": 3, "y": 4, "download_wait_ms": 500}`))
	req.Header.Set("Authorization", "Bearer "+f.all)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("/click: %d", resp.StatusCode)
	}
	if n := notice(t, resp.Header); len(n.Files) != 1 || n.Files[0].Name != "via-click.csv" {
		t.Fatalf("/click notice %+v", n)
	}
	if got := f.lastInput(); got != `/input/click {"x":3,"y":4}` {
		t.Errorf("/click forwarded %s", got)
	}
}

func TestComputerDownloadsQuietOtherwise(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	// No download: no header.
	code, h, _ := f.computer(s.ID, `{"action": "click", "x": 1, "y": 2}`)
	if code != http.StatusNoContent || h.Get(DownloadsHeader) != "" {
		t.Fatalf("click without a download: %d %q", code, h.Get(DownloadsHeader))
	}
	// Only clicks and keys are watched.
	f.downloadOn("google", "/input/scroll", "scrolled.csv", "x")
	code, h, _ = f.computer(s.ID, `{"action": "scroll", "x": 1, "y": 2, "dy": 1}`)
	if code != http.StatusNoContent || h.Get(DownloadsHeader) != "" {
		t.Fatalf("scroll: %d %q", code, h.Get(DownloadsHeader))
	}
	for _, body := range []string{
		`{"action": "click", "x": 1, "y": 2, "download_wait_ms": 30001}`,
		`{"action": "key", "keys": "Return", "download_wait_ms": -1}`,
	} {
		if code, _, raw := f.computer(s.ID, body); code != http.StatusBadRequest || !strings.Contains(string(raw), "download_wait_ms") {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 1, "y": 2, "download_wait_ms": "soon"}`, nil); code != http.StatusBadRequest {
		t.Errorf("/click with a bad download_wait_ms: %d", code)
	}
}

func TestDownloadsHeaderCapsAndEscapes(t *testing.T) {
	var ds []files.Download
	for range 7 {
		ds = append(ds, files.Download{Name: "😀.txt", Size: 1, State: files.Downloaded})
	}
	h := downloadsHeader(ds)
	if strings.Contains(h, "😀") || strings.Contains(h, "\n") || !strings.Contains(h, `\ud83d`+`\ude00`) {
		t.Fatalf("header %q", h)
	}
	var n downloadsNotice
	if err := json.Unmarshal([]byte(h), &n); err != nil || len(n.Files) != files.MaxNotices || n.More != 2 || n.Files[0].Name != "😀.txt" {
		t.Fatalf("header %q: %+v %v", h, n, err)
	}
}

// A download that finishes after a click has answered is told with the
// session's next click, once; a new session starts afresh.
func TestComputerTellsALateDownloadNextTime(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")
	if code, h, _ := f.computer(s.ID, `{"action": "click", "x": 1, "y": 2}`); code != http.StatusNoContent || h.Get(DownloadsHeader) != "" {
		t.Fatalf("first click: %d %q", code, h.Get(DownloadsHeader))
	}
	dir := filepath.Join(f.m.Layout().FilesDir("google"), files.Downloads)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "late.csv"), []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, h, _ := f.computer(s.ID, `{"action": "key", "keys": "Tab"}`)
	if n := notice(t, h); len(n.Files) != 1 || n.Files[0] != (files.Download{Name: "late.csv", Size: 4, State: files.Downloaded, Earlier: true}) {
		t.Fatalf("notice %+v", n)
	}
	if _, h, _ := f.computer(s.ID, `{"action": "key", "keys": "Tab"}`); h.Get(DownloadsHeader) != "" {
		t.Fatalf("told twice: %q", h.Get(DownloadsHeader))
	}
	// The session ends: what was kept about it goes.
	if code := f.call(f.all, http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "between.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = f.start(f.all, "google", "visual")
	if _, h, _ := f.computer(s.ID, `{"action": "click", "x": 1, "y": 2}`); h.Get(DownloadsHeader) != "" {
		t.Fatalf("a new session's first click: %q", h.Get(DownloadsHeader))
	}
}
