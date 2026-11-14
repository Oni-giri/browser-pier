//go:build docker && measure

package chromium

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestMeasureWindowAtLoad counts how often a page's first script sees a
// 0x0 window (PROJ-29). It's a measurement, not a check, so it only runs
// with the measure tag:
//
//	go test -tags 'docker measure' -run TestMeasureWindowAtLoad -v ./images/chromium/
//
// The detection page is opened in Visual mode as a person would: in fresh
// containers (MEASURE_FRESH, the antibot test's flow), in new tabs of one
// container (MEASURE_TABS) and again in the same tab (MEASURE_SAME); then
// in API mode with Page.navigate (MEASURE_API).
func TestMeasureWindowAtLoad(t *testing.T) {
	image := testImage(t)
	det := browsertest.NewDetector(t, siteHost(t))
	zero, total := 0, 0
	record := func(what string, s browsertest.Signals) {
		total++
		if s.OuterWidth == 0 || s.OuterHeight == 0 {
			zero++
		}
		t.Logf("%s: outer %dx%d at first script, %dx%d after load; screen %dx%d, avail %dx%d, focus %t",
			what, s.OuterWidth, s.OuterHeight, s.OuterWidthAfterLoad, s.OuterHeightAfterLoad,
			s.ScreenWidth, s.ScreenHeight, s.AvailWidth, s.AvailHeight, s.HasFocus)
	}
	open := func(b *browser, keys, url string) browsertest.Signals {
		b.input(t, "/input/key", fmt.Sprintf(`{"keys": %q}`, keys))
		b.input(t, "/input/type", fmt.Sprintf(`{"text": %q}`, url))
		b.input(t, "/input/key", `{"keys": "Return"}`)
		return det.Report(t)
	}
	summary := []string{}
	phase := func(name string, run func()) {
		z, n := zero, total
		run()
		summary = append(summary, fmt.Sprintf("%s: %d of %d loads saw 0x0", name, zero-z, total-n))
	}

	phase("Visual, fresh containers", func() {
		for i := range envInt("MEASURE_FRESH", 10) {
			b := startBrowser(t, image, newVolume(t))
			record(fmt.Sprintf("fresh %d", i), open(b, "ctrl+l", det.URL))
			docker(t, "rm", "-f", b.name)
		}
	})
	b := startBrowser(t, image, newVolume(t))
	phase("Visual, new tabs", func() {
		for i := range envInt("MEASURE_TABS", 20) {
			record(fmt.Sprintf("tab %d", i), open(b, "ctrl+t", fmt.Sprintf("%s?tab%d", det.URL, i)))
			b.input(t, "/input/key", `{"keys": "ctrl+w"}`)
		}
	})
	phase("Visual, same tab", func() {
		for i := range envInt("MEASURE_SAME", 20) {
			record(fmt.Sprintf("same %d", i), open(b, "ctrl+l", fmt.Sprintf("%s?same%d", det.URL, i)))
		}
	})
	phase("API, Page.navigate", func() {
		p := b.page()
		for i := range envInt("MEASURE_API", 20) {
			p.Call("Page.navigate", map[string]any{"url": fmt.Sprintf("%s?api%d", det.URL, i)})
			record(fmt.Sprintf("api %d", i), det.Report(t))
		}
	})
	for _, s := range summary {
		t.Log(s)
	}
	t.Logf("all: %d of %d loads saw 0x0", zero, total)
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

// siteHost is how a browser container reaches this machine, as the other
// packages' Docker tests find it: BROWSERPIER_TEST_SITE_HOST, or the
// bridge's gateway.
func siteHost(t *testing.T) string {
	if h := os.Getenv("BROWSERPIER_TEST_SITE_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		t.Fatalf("can't find the bridge gateway (%v); set BROWSERPIER_TEST_SITE_HOST", err)
	}
	return strings.TrimSpace(string(out))
}
