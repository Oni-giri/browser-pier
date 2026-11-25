//go:build docker

package sessions

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestMeasureRecordingCost is PROJ-23's step 8, run by hand with
// BROWSERPIER_MEASURE=1: the recording pipeline's CPU, memory and bytes
// per hour for a still page, a page scrolling, and video playing.
func TestMeasureRecordingCost(t *testing.T) {
	if os.Getenv("BROWSERPIER_MEASURE") != "1" {
		t.Skip("set BROWSERPIER_MEASURE=1 to measure")
	}
	secs := 60
	if s, err := strconv.Atoi(os.Getenv("BROWSERPIER_MEASURE_SECONDS")); err == nil && s > 0 {
		secs = s
	}
	pages := map[string]string{
		"still": `<h1>Signed in as pier</h1><p>A page that doesn't move.</p>`,
		"scroll": `<style>p{font:16px sans-serif;margin:0 20px 12px}</style>` + strings.Repeat(`<p>Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris.</p>`, 400) +
			`<script>setInterval(() => { scrollBy(0, 60); if (innerHeight + scrollY >= document.body.scrollHeight) scrollTo(0, 0) }, 250)</script>`,
		"video": `<canvas id=c width=640 height=360></canvas><script>
const c = document.getElementById('c').getContext('2d'); let t = 0;
(function f() { t++; for (let i = 0; i < 40; i++) { c.fillStyle = 'hsl(' + ((t * 3 + i * 9) % 360) + ',70%,50%)';
c.fillRect((i * 53 + t * 7) % 640, (i * 31 + t * 5) % 360, 80, 60) } requestAnimationFrame(f) })()</script>`,
	}
	mux := http.NewServeMux()
	for name, body := range pages {
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, body)
		})
	}
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	site := fmt.Sprintf("http://%s:%d", browsertest.SiteHost(t), ln.Addr().(*net.TCPAddr).Port)

	for _, scene := range []string{"still", "scroll", "video"} {
		t.Run(scene, func(t *testing.T) {
			r := newRecRig(t, true)
			s := startRunning(t, r.m, r.name)
			p, done := r.agent(t, s)
			defer done()
			p.Navigate(site + "/" + scene)
			time.Sleep(5 * time.Second)
			pid := pipelinePID(t, r.name)
			cpu0, before := cpuTicks(t, r.name, pid), usage(r)
			start := time.Now()
			time.Sleep(time.Duration(secs) * time.Second)
			cpu1, after := cpuTicks(t, r.name, pid), usage(r)
			elapsed := time.Since(start)
			rss := status(t, r.name, pid)
			core := float64(cpu1-cpu0) / 100 / elapsed.Seconds() * 100
			perHour := float64(after-before) / elapsed.Hours()
			t.Logf("MEASURE %s: pipeline CPU %.1f%% of a core, memory %s, %d bytes in %s = %.1f MB/hour",
				scene, core, rss, after-before, elapsed.Round(time.Second), perHour/1e6)
			done()
			stopCleanly(t, r.m, s.ID)
		})
	}
}

func usage(r *recRig) int64 {
	total, _ := r.store.Usage()
	return total
}

func containerSh(t *testing.T, name, script string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", containerName(name), "sh", "-c", script+" 2>/dev/null").Output()
	if err != nil {
		t.Fatalf("%s: %v: %s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

func pipelinePID(t *testing.T, name string) string {
	t.Helper()
	pid := containerSh(t, name, `for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q mp4mux && echo ${p#/proc/}; done | head -1`)
	if pid == "" {
		t.Fatal("no recording pipeline")
	}
	return pid
}

// cpuTicks is the CPU time the recording pipelines have used, in clock
// ticks: the running one's, and those that ended (each segment has its
// own), which the entrypoint has waited for (its children's times).
func cpuTicks(t *testing.T, name, pid string) int64 {
	t.Helper()
	_ = pid
	now := pipelinePID(t, name)
	ep := containerSh(t, name, `for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q '^/usr/local/bin/browserpier-entrypoint' && echo ${p#/proc/}; done | head -1`)
	f := strings.Fields(containerSh(t, name, "cat /proc/"+now+"/stat"))
	e := strings.Fields(containerSh(t, name, "cat /proc/"+ep+"/stat"))
	n := func(fs []string, i int) int64 { v, _ := strconv.ParseInt(fs[i], 10, 64); return v }
	return n(f, 13) + n(f, 14) + n(e, 15) + n(e, 16)
}

func status(t *testing.T, name, pid string) string {
	t.Helper()
	now := pipelinePID(t, name)
	return containerSh(t, name, "grep -E 'VmRSS|VmHWM' /proc/"+now+"/status | tr -s ' \\t' ' ' | tr '\\n' ' '")
}
