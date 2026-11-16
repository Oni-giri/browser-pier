//go:build docker

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/files"
)

// downloadsDir is a profile's downloads folder on the host.
func (a agent) downloadsDir(profile string) string {
	return filepath.Join(a.m.Layout().FilesDir(profile), files.Downloads)
}

// listed is the profile's files as the API lists them, by name.
func (a agent) listed(t *testing.T, profile string) map[string]int64 {
	t.Helper()
	var out struct{ Files []files.File }
	if code := a.do(http.MethodGet, "/api/profiles/"+profile+"/files", "", &out); code != http.StatusOK {
		t.Fatalf("list files: %d", code)
	}
	m := map[string]int64{}
	for _, f := range out.Files {
		m[f.Name] = f.Size
	}
	return m
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// TestActReportsDownloads is PROJ-116 in API mode: act's click on a link
// that downloads says so; a slow download is in progress when the wait
// is over and listed once it has finished; a click that downloads
// nothing says nothing and isn't held up.
func TestActReportsDownloads(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	s := a.startAPIBrowser(t, "d")
	a.mustTool(s.ID, "navigate", map[string]string{"url": site.URL + "/downloads"})
	snap := a.mustTool(s.ID, "snapshot", nil)

	start := time.Now()
	out := a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `link "Export CSV"`)}))
	t.Logf("act click on Export CSV: %v\n%s", time.Since(start).Round(time.Millisecond), out)
	want := fmt.Sprintf("downloaded export.csv (%d B)", len(browsertest.ExportCSV))
	if !strings.Contains(out, "\n### Downloads\n") || !fencedIn(out, want) {
		t.Fatalf("no %q in\n%s", want, out)
	}
	if got := a.listed(t, s.Profile); got["export.csv"] != int64(len(browsertest.ExportCSV)) {
		t.Errorf("files listed %v", got)
	}

	// 5 MB over 5 s: still under way after the 3 s wait. Chrome names it
	// "Unconfirmed N.crdownload" until it has finished.
	start = time.Now()
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": refFor(t, snap, `link "Big file"`)}))
	took := time.Since(start)
	t.Logf("act click on Big file: %v\n%s", took.Round(time.Millisecond), out)
	if !fencedIn(out, "download in progress (") || !strings.Contains(out, "list_files to check later") {
		t.Fatalf("no download in progress in\n%s", out)
	}
	if took < 2500*time.Millisecond || took > 8*time.Second {
		t.Errorf("act took %v; the wait is 3 s", took)
	}
	for deadline := time.Now().Add(20 * time.Second); a.listed(t, s.Profile)["big.csv"] != browsertest.BigSize; {
		if time.Now().After(deadline) {
			var there []string
			entries, _ := os.ReadDir(a.downloadsDir(s.Profile))
			for _, e := range entries {
				info, _ := e.Info()
				there = append(there, fmt.Sprintf("%s (%d B)", e.Name(), info.Size()))
			}
			t.Fatalf("big.csv isn't listed whole: %v; the folder has %v", a.listed(t, s.Profile), there)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The next action tells it has finished, once.
	nothing := refFor(t, snap, `button "Nothing"`)
	out = a.mustTool(s.ID, "act", steps(map[string]any{"action": "click", "ref": nothing, "pace": "instant"}))
	if !fencedIn(out, "downloaded big.csv (5.0 MB), after the last action's answer") {
		t.Errorf("the next action doesn't tell big.csv finished:\n%s", out)
	}

	// Nothing downloaded: no section, and no wait. The check itself, two
	// listings of the folder, is what the notice adds to a click, with
	// StartGrace (40 ms) when it doesn't overlap the changes' read.
	var acts, checks []time.Duration
	for range 5 {
		start = time.Now()
		out = a.mustTool(s.ID, "act", map[string]any{"steps": []map[string]any{{"action": "click", "ref": nothing, "pace": "instant"}}, "changes": false})
		acts = append(acts, time.Since(start))
		if strings.Contains(out, "Downloads") {
			t.Fatalf("a click that downloads nothing:\n%s", out)
		}
		start = time.Now()
		m := files.New(a.m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()).Mark(s.Profile, "")
		m.Ended()
		m.Downloads(context.Background(), files.DefaultDownloadWait)
		checks = append(checks, time.Since(start))
	}
	t.Logf("act click on Nothing: median %v; the download check: median %v", median(acts), median(checks))
	if median(checks) > 50*time.Millisecond {
		t.Errorf("the download check takes %v, over 50 ms", median(checks))
	}
	if median(acts) > 1500*time.Millisecond {
		t.Errorf("a click that downloads nothing took %v: it waited", median(acts))
	}
	if !site.HasEvent("nothing clicked") {
		t.Errorf("the button wasn't clicked: %v", site.Events())
	}
}

// TestVisualClickReportsADownload is PROJ-116 in Visual mode, where no
// CDP client attaches: a /computer click on a link that downloads says
// so in X-Downloads, and the activity log has it.
func TestVisualClickReportsADownload(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewPageSite(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("v%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "visual"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.Mode != "visual" {
		t.Fatalf("start: %d %+v", code, s)
	}
	computer := func(body string) http.Header {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+s.ID+"/computer", strings.NewReader(body))
		req.Header = a.header()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: %d", body, resp.StatusCode)
		}
		return resp.Header
	}
	for _, body := range []string{
		`{"action": "key", "keys": "ctrl+l", "pace": "instant"}`,
		fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, site.URL+"/downloads"),
		`{"action": "key", "keys": "Return", "pace": "instant"}`,
	} {
		if h := computer(body); h.Get(DownloadsHeader) != "" {
			t.Fatalf("%s says it downloaded %s", body, h.Get(DownloadsHeader))
		}
	}
	// The page's blocks are fractions of the window below the toolbar;
	// the button reports a click, so it says when the page is there.
	sw, sh := a.screenSize(t, s.ID)
	top := 100
	vh := sh - 32 - top
	at := func(frac float64) string { return fmt.Sprintf(`"x": %d, "y": %d`, sw/2, top+int(frac*float64(vh))) }
	for deadline := time.Now().Add(30 * time.Second); !site.HasEvent("nothing clicked"); {
		if h := computer(`{"action": "click", ` + at(0.8) + `, "pace": "instant"}`); h.Get(DownloadsHeader) != "" {
			t.Fatalf("a click on Nothing says it downloaded %s", h.Get(DownloadsHeader))
		}
		if time.Now().After(deadline) {
			t.Fatal("the page never loaded")
		}
		time.Sleep(300 * time.Millisecond)
	}

	// The click expects a download, so it says so (SPEC §8 Downloads):
	// without download_wait_ms the look lasts 40 ms, which a slow
	// machine's server can miss, and the notice then comes with the next
	// action instead (TestComputerTellsALateDownloadNextTime).
	start := time.Now()
	h := computer(`{"action": "click", ` + at(0.2) + `, "download_wait_ms": 10000}`)
	t.Logf("click on Export CSV: %v, X-Downloads %s", time.Since(start).Round(time.Millisecond), h.Get(DownloadsHeader))
	if h.Get(DownloadsHeader) == "" {
		// Say when the file did come, to tell a late download from none.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if entries, _ := os.ReadDir(a.downloadsDir(name)); len(entries) > 0 {
				t.Logf("%v after the click returned, the folder has %s", time.Since(start), entries[0].Name())
				break
			}
		}
	}
	var n downloadsNotice
	if err := json.Unmarshal([]byte(h.Get(DownloadsHeader)), &n); err != nil {
		t.Fatalf("X-Downloads %q: %v", h.Get(DownloadsHeader), err)
	}
	if want := (files.Download{Name: "export.csv", Size: int64(len(browsertest.ExportCSV)), State: files.Downloaded}); len(n.Files) != 1 || n.Files[0] != want {
		t.Fatalf("notice %+v, want %+v", n, want)
	}
	logged := false
	for _, e := range a.m.Activity(s.ID) {
		logged = logged || e.What == fmt.Sprintf("Downloaded export.csv (%d B)", len(browsertest.ExportCSV))
	}
	if !logged {
		t.Errorf("activity %+v", a.m.Activity(s.ID))
	}
	// Still Visual mode's rule: nothing attached to the page.
	for _, tg := range targets(t, a.m.Layout().ControlSocket(name)) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("a CDP client is attached to %s", tg.URL)
		}
	}
}
