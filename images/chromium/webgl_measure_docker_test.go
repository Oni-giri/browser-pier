//go:build docker && measure

package chromium

import (
	"context"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// webglScene is a page that keeps WebGL busy: a full-window canvas
// redrawing {{N}} moving triangles every frame.
const webglScene = `data:text/html,<body style="margin:0;background:%23000"><canvas id="c"></canvas><script>
const c = document.getElementById('c'); c.width = innerWidth; c.height = innerHeight;
const gl = c.getContext('webgl2');
const sh = (t, s) => { const x = gl.createShader(t); gl.shaderSource(x, s); gl.compileShader(x); return x; };
const p = gl.createProgram();
gl.attachShader(p, sh(gl.VERTEX_SHADER, '%23version 300 es\nin vec2 a; uniform float t; out vec3 v; void main() { float r = t + float(gl_VertexID / 3) * 0.01; mat2 m = mat2(cos(r), sin(r), -sin(r), cos(r)); gl_Position = vec4(m * a, 0.0, 1.0); v = vec3(0.5 + 0.5 * sin(r), 0.5 + 0.5 * cos(r), 0.6); }'));
gl.attachShader(p, sh(gl.FRAGMENT_SHADER, '%23version 300 es\nprecision mediump float; in vec3 v; out vec4 o; void main() { o = vec4(v, 0.5); }'));
gl.linkProgram(p); gl.useProgram(p);
const n = {{N}}, d = new Float32Array(n * 6);
for (let i = 0; i < d.length; i++) d[i] = Math.random() * 2 - 1;
gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer()); gl.bufferData(gl.ARRAY_BUFFER, d, gl.STATIC_DRAW);
gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
gl.enable(gl.BLEND); gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
const u = gl.getUniformLocation(p, 't'); window.frames_ = 0;
const f = now => { gl.clearColor(0, 0, 0, 1); gl.clear(gl.COLOR_BUFFER_BIT); gl.uniform1f(u, now / 1000); gl.drawArrays(gl.TRIANGLES, 0, n * 3); window.frames_++; requestAnimationFrame(f); };
requestAnimationFrame(f);
</script>`

// TestMeasureWebGLCost measures what the image's WebGL renderer costs
// (PROJ-148): the time to the first page, then the container's CPU and
// memory with an idle page, a busy WebGL page, a heavy site being
// scrolled, and both of those again with the live stream watched. It's a
// measurement, not a check, so it only runs with the measure tag:
//
//	go test -tags 'docker measure' -run TestMeasureWebGLCost -v ./images/chromium/
//
// Compare images by running it once per BROWSERPIER_TEST_IMAGE on the same
// host. MEASURE_HEAVY_URL is the heavy site (it needs the internet),
// MEASURE_SECONDS each sample's length, MEASURE_TRIANGLES how many
// triangles the WebGL page draws a frame (3000 keeps every core busy), and MEASURE_ENV adds container
// settings (KEY=VALUE, separated by ";").
func TestMeasureWebGLCost(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	var env []string
	if e := os.Getenv("MEASURE_ENV"); e != "" {
		env = strings.Split(e, ";")
	}
	heavy := os.Getenv("MEASURE_HEAVY_URL")
	if heavy == "" {
		heavy = "https://en.wikipedia.org/wiki/World_War_II"
	}
	window := time.Duration(envInt("MEASURE_SECONDS", 20)) * time.Second

	start := time.Now()
	b := startBrowser(t, image, newVolume(t), env...)
	ready := time.Since(start)
	p := b.page()
	p.Navigate(s.URL + "/account")
	t.Logf("start: control socket ready in %v, first page loaded in %v", ready.Round(10*time.Millisecond), time.Since(start).Round(10*time.Millisecond))
	t.Logf("WebGL renderer: %v", p.Eval(`(() => { const gl = document.createElement('canvas').getContext('webgl');
		if (!gl) return 'no WebGL'; const d = gl.getExtension('WEBGL_debug_renderer_info');
		return gl.getParameter(d.UNMASKED_VENDOR_WEBGL) + ' / ' + gl.getParameter(d.UNMASKED_RENDERER_WEBGL); })()`))

	cpuUsec := func() int64 {
		out := docker(t, "exec", b.name, "sh", "-c", "grep '^usage_usec ' /sys/fs/cgroup/cpu.stat")
		n, _ := strconv.ParseInt(strings.TrimPrefix(out, "usage_usec "), 10, 64)
		return n
	}
	memMB := func() int64 {
		n, _ := strconv.ParseInt(docker(t, "exec", b.name, "cat", "/sys/fs/cgroup/memory.current"), 10, 64)
		return n >> 20
	}
	gpu := func() string {
		return docker(t, "exec", b.name, "sh", "-c", `for d in /proc/[0-9]*; do
			case $(tr '\0' ' ' < $d/cmdline 2>/dev/null) in /usr/lib/chromium/*--type=gpu-process*)
				grep -aE '^(VmRSS|Threads):' $d/status | tr -s '\t\n' '  ';; esac; done`)
	}
	// sample runs during for the window, while it reads the container's
	// CPU time and peak memory, and what the live stream costs if watch.
	// edge, if set, runs right as the window opens and right as it closes,
	// before the readings that follow it, so a rate it measures covers the
	// window alone.
	sample := func(name string, watch bool, during func(), edge func()) {
		ctx, cancel := context.WithTimeout(context.Background(), window)
		defer cancel()
		type result struct {
			n   int64
			err error
		}
		var stream chan result
		if watch {
			stream = make(chan result, 1)
			go func() {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/stream", nil)
				resp, err := b.http.Do(req)
				if err != nil {
					stream <- result{err: err}
					return
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusOK {
					stream <- result{err: fmt.Errorf("stream: %s", resp.Status)}
					return
				}
				n, _ := io.Copy(io.Discard, resp.Body) // ends with the window
				stream <- result{n: n}
			}()
		}
		if edge != nil {
			edge()
		}
		cpu0, t0 := cpuUsec(), time.Now()
		var peak int64
		for ctx.Err() == nil {
			during()
			if m := memMB(); m > peak {
				peak = m
			}
			time.Sleep(500 * time.Millisecond)
		}
		if edge != nil {
			edge()
		}
		cpu := float64(cpuUsec()-cpu0) / float64(time.Since(t0).Microseconds()) * 100
		var streamed int64
		if watch {
			r := <-stream
			if r.err != nil || r.n == 0 {
				t.Fatalf("%s: the live stream wasn't watched (%d bytes): %v", name, r.n, r.err)
			}
			streamed = r.n
		}
		stats := docker(t, "stats", "--no-stream", "--format", "{{.MemUsage}}", b.name)
		t.Logf("%-22s CPU %4.0f%% of a core, memory peak %4d MB, now %s (docker stats); GPU process %s; stream %d KB",
			name+":", cpu, peak, stats, gpu(), streamed>>10)
	}
	// frames reads the WebGL page's frame counter at each edge of a
	// window; rate is the frames a second between the last two reads.
	var marks []struct {
		f  float64
		at time.Time
	}
	frames := func() {
		f, _ := p.Eval("window.frames_").(float64)
		marks = append(marks, struct {
			f  float64
			at time.Time
		}{f, time.Now()})
	}
	rate := func() float64 {
		a, z := marks[len(marks)-2], marks[len(marks)-1]
		return (z.f - a.f) / z.at.Sub(a.at).Seconds()
	}
	settle := func() { time.Sleep(5 * time.Second) }

	settle()
	sample("idle page", false, func() {}, nil)

	p.Navigate(strings.NewReplacer("\n", "", "{{N}}", strconv.Itoa(envInt("MEASURE_TRIANGLES", 3000))).Replace(webglScene))
	settle()
	sample("WebGL page", false, func() {}, frames)
	t.Logf("WebGL page: %.0f frames a second", rate())
	sample("WebGL page, watched", true, func() {}, nil)
	// What the X display, and so the stream, shows of the canvas.
	if c, ok := screenAt(t, b, 640, 450); ok {
		t.Logf("WebGL page on the X display at its centre: %v (black is 0,0,0)", c)
	}

	p.Call("Page.navigate", map[string]any{"url": heavy})
	time.Sleep(10 * time.Second)
	scroll := func() { b.input(t, "/input/scroll", `{"x": 640, "y": 400, "dy": 2}`) }
	sample("heavy site, scrolled", false, scroll, nil)
	p.Call("Page.navigate", map[string]any{"url": heavy})
	time.Sleep(10 * time.Second)
	sample("heavy site, watched", true, scroll, nil)
	p.Close()
	t.Logf("stop: exit %s", b.stop())
}

// screenAt is the screenshot's colour at (x, y).
func screenAt(t *testing.T, b *browser, x, y int) (color.RGBA, bool) {
	t.Helper()
	resp, err := b.http.Get("http://control/screenshot")
	if err != nil {
		return color.RGBA{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	img, err := png.Decode(resp.Body)
	if err != nil {
		return color.RGBA{}, false
	}
	return color.RGBAModel.Convert(img.At(x, y)).(color.RGBA), true
}
