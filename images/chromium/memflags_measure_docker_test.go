//go:build docker && measure

package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/docker/memusage"
)

// TestMeasureMemoryFlags measures what the back/forward cache and the spare
// renderer cost a browser (PROJ-160), in the number admission counts
// (internal/docker's MemoryUsage). It's a measurement, not a check, so it
// only runs with the measure tag:
//
//	go test -tags 'docker measure' -run TestMeasureMemoryFlags -v -timeout 90m ./images/chromium/
//
// Each run passes Chromium more switches than the image does, through a
// wrapper mounted over its binary that appends MEASURE_CHROMIUM_FLAGS; the
// binary itself is the image's, copied out once to MEASURE_DIR (default
// the user cache directory: it's 300 MB, too much for a tmpfs /tmp).
//
// "bfcache" opens /hello, then the same tab goes to /heavy (MEASURE_MB MB
// held by the page, 200 by default), then to /hello again, and the
// container's memory is read every 5 s for MEASURE_SECONDS (240), then
// Back. Once with the image's switches, once with the cache off; with
// /hello on the same site as /heavy and on another (another address of
// the test site); in Visual mode (xdotool, no CDP client) and in API mode
// (a CDP client attached throughout, Page.navigate and history.back()).
// "spare" reads the memory of a browser idle on /hello for 60 s, with and
// without SpareRendererForSitePerProcess, MEASURE_RUNS (3) times each.
// "lastwins" checks what Chromium does with two --disable-features
// switches. MEASURE_PARTS picks the parts (all by default).
func TestMeasureMemoryFlags(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = api.Close() }()
	wrap := chromiumWrapper(t, image)
	parts := os.Getenv("MEASURE_PARTS")
	if parts == "" {
		parts = "bfcache,spare,lastwins"
	}
	window := time.Duration(envInt("MEASURE_SECONDS", 240)) * time.Second
	mb := envInt("MEASURE_MB", 200)

	version := docker(t, "run", "--rm", "--entrypoint", "sh", image, "-c", "/usr/lib/chromium/chromium --version; uname -m")
	t.Logf("image %s: %s", image, strings.ReplaceAll(version, "\n", ", "))

	memMiB := func(b *browser) int64 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		n, err := memusage.Read(ctx, api, b.name)
		if err != nil {
			t.Fatal(err)
		}
		return n >> 20
	}
	renderers := func(b *browser) string {
		return docker(t, "exec", b.name, "sh", "-c", `n=0; rss=0; for d in /proc/[0-9]*; do
			case $(tr '\0' ' ' < $d/cmdline 2>/dev/null) in *--type=renderer*)
				n=$((n+1)); rss=$((rss + $(sed -n 's/^VmRSS:[^0-9]*\([0-9]*\).*/\1/p' $d/status)));; esac; done
			echo "$n renderers, $((rss/1024)) MiB RSS"`)
	}
	// driver moves the browser's one tab: by xdotool in Visual mode, by a
	// CDP client in API mode.
	type driver struct {
		visit func(url string)
		back  func()
	}
	drive := func(b *browser, mode string) driver {
		if mode == "visual" {
			return driver{
				visit: func(url string) {
					b.input(t, "/input/key", `{"keys": "ctrl+l"}`)
					b.input(t, "/input/type", fmt.Sprintf(`{"text": %q, "pace": "instant"}`, url))
					b.input(t, "/input/key", `{"keys": "Return"}`)
				},
				back: func() { b.input(t, "/input/key", `{"keys": "alt+Left"}`) },
			}
		}
		p := b.page()
		return driver{
			visit: func(url string) { p.Call("Page.navigate", map[string]any{"url": url}) },
			back:  func() { p.Eval("history.back()") },
		}
	}
	// open has d visit url, and waits for page to report it was shown.
	open := func(d driver, url, page string) browsertest.Shown {
		n := len(s.Shows(page))
		d.visit(url)
		return s.WaitShown(t, page, n+1, 60*time.Second)
	}
	start := func(flags string) *browser {
		return startBrowserWith(t, image, newVolume(t), wrap, "MEASURE_CHROMIUM_FLAGS="+flags)
	}
	var summary []string

	if strings.Contains(parts, "bfcache") {
		marks := []time.Duration{0, 15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, window}
		header := "| mode | leaves to | flags | before | on /heavy |"
		for _, m := range marks {
			header += fmt.Sprintf(" +%v |", m)
		}
		summary = append(summary, header+" Back |")
		u, err := url.Parse(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		other := ""
		for _, mode := range []string{"visual", "api"} {
			for _, to := range []string{"same site", "other site"} {
				for _, flags := range []string{"", "--disable-features=BackForwardCache"} {
					b := start(flags)
					d := drive(b, mode)
					hello := s.URL + "/hello"
					if to == "other site" {
						if other == "" {
							other = browsertest.OtherAddress(t, b.name, u.Hostname(), u.Port())
						}
						hello = "http://" + net.JoinHostPort(other, u.Port()) + "/hello"
					}
					open(d, hello, "/hello")
					time.Sleep(15 * time.Second)
					before := memMiB(b)
					open(d, fmt.Sprintf("%s/heavy?mb=%d", s.URL, mb), "/heavy")
					time.Sleep(5 * time.Second)
					heavy := memMiB(b)
					t.Logf("%s, %s, %q: before %d MiB, on /heavy %d MiB (%s)", mode, to, flags, before, heavy, renderers(b))
					open(d, hello, "/hello")
					left := time.Now()
					at := map[time.Duration]int64{}
					var series []string
					for i := time.Duration(0); i <= window; i += 5 * time.Second {
						time.Sleep(time.Until(left.Add(i)))
						m := memMiB(b)
						at[i] = m
						series = append(series, strconv.FormatInt(m, 10))
					}
					t.Logf("%s, %s, %q: every 5 s after leaving /heavy, MiB: %s; then %s", mode, to, flags, strings.Join(series, " "), renderers(b))
					row := fmt.Sprintf("| %s | %s | %s | %d | %d |", mode, to, orNone(flags), before, heavy)
					for _, m := range marks {
						row += fmt.Sprintf(" %d |", at[m])
					}
					summary = append(summary, row+" "+describeShown(back(t, s, d.back, "/heavy"))+" |")
					t.Logf("stop: exit %s", b.stop())
				}
			}
		}
	}

	if strings.Contains(parts, "spare") {
		summary = append(summary, "", "| flags | run | idle 60 s on /hello, MiB | renderers |")
		for _, flags := range []string{"", "--disable-features=SpareRendererForSitePerProcess"} {
			for i := range envInt("MEASURE_RUNS", 3) {
				b := start(flags)
				open(drive(b, "visual"), s.URL+"/hello", "/hello")
				time.Sleep(60 * time.Second)
				summary = append(summary, fmt.Sprintf("| %s | %d | %d | %s |", orNone(flags), i+1, memMiB(b), renderers(b)))
				t.Logf("stop: exit %s", b.stop())
			}
		}
	}

	if strings.Contains(parts, "lastwins") {
		summary = append(summary, "", "| flags | Back to /back/a |")
		for _, flags := range []string{
			"--disable-features=BackForwardCache --disable-features=Translate",
			"--disable-features=Translate --disable-features=BackForwardCache",
		} {
			b := start(flags)
			d := drive(b, "visual")
			open(d, s.URL+"/back/a", "/back/a")
			open(d, s.URL+"/back/b", "/back/b")
			summary = append(summary, fmt.Sprintf("| %s | %s |", flags, describeShown(back(t, s, d.back, "/back/a"))))
			t.Logf("stop: exit %s", b.stop())
		}
	}

	t.Log("summary:\n" + strings.Join(summary, "\n"))
}

// shownBack is what a page reported when Back returned to it, whether it
// was a new document, and whether the site served it again (Back takes a
// page from the HTTP cache when it can).
type shownBack struct {
	browsertest.Shown
	newDocument, servedAgain bool
}

// back goes Back with goBack and waits for page to report a pageshow.
func back(t *testing.T, s *browsertest.Site, goBack func(), page string) shownBack {
	t.Helper()
	served, shows := s.Served(page), s.Shows(page)
	goBack()
	sh := s.WaitShown(t, page, len(shows)+1, 60*time.Second)
	return shownBack{sh, sh.Load != shows[len(shows)-1].Load, s.Served(page) > served}
}

func describeShown(b shownBack) string {
	var reasons any
	_ = json.Unmarshal([]byte(b.Reasons), &reasons)
	r, _ := json.Marshal(reasons)
	return fmt.Sprintf("persisted %t, type %s, new document %t, served again %t, notRestoredReasons %s",
		b.Persisted, b.Type, b.newDocument, b.servedAgain, r)
}

func orNone(flags string) string {
	if flags == "" {
		return "(the image's)"
	}
	return "`" + flags + "`"
}

// chromiumWrapper returns `docker run` options that put a wrapper over the
// image's Chromium binary, which runs the binary with
// $MEASURE_CHROMIUM_FLAGS after the entrypoint's switches.
func chromiumWrapper(t *testing.T, image string) []string {
	t.Helper()
	dir := os.Getenv("MEASURE_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		dir = cache
	}
	dir, err := os.MkdirTemp(dir, "bp-measure-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the container's user reads it
		t.Fatal(err)
	}
	name := fmt.Sprintf("bp-test-cp-%d", time.Now().UnixNano())
	docker(t, "create", "--name", name, image)
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()
	real := filepath.Join(dir, "chromium-real")
	docker(t, "cp", name+":/usr/lib/chromium/chromium", real)
	wrapper := filepath.Join(dir, "chromium")
	//nolint:gosec // an executable the container runs
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /usr/lib/chromium/chromium-real \"$@\" $MEASURE_CHROMIUM_FLAGS\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{
		"-v", real + ":/usr/lib/chromium/chromium-real:ro",
		"-v", wrapper + ":/usr/lib/chromium/chromium:ro",
	}
}
