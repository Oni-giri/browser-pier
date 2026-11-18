//go:build docker

// These tests run the real browser image. They need Docker and an image built
// with `make image-chromium`; BROWSERPIER_TEST_IMAGE names it. The browser
// reaches the test site as browsertest.SiteHost says, like the orchestrator's
// tests. Set BROWSERPIER_TEST_ARTIFACTS to a directory to keep a screenshot.
package chromium

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

func testImage(t *testing.T) string {
	img := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if img == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; build with `make image-chromium` and run `make test-docker`")
	}
	return img
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

var (
	appArmorOnce sync.Once
	appArmorArgs []string
	appArmorErr  error
)

// appArmor is the AppArmor option the browser tests run under, as
// `docker run` arguments: what BrowserPier would choose on this host
// (internal/docker), found with the image's sandbox check in the same
// order: BrowserPier's profile if it's loaded, the daemon's default (which
// refuses the sandbox on Debian 13 with Docker 29.1 and earlier), then
// unconfined. BROWSERPIER_TEST_APPARMOR sets it instead: a profile's name,
// or "default" for the daemon's.
func appArmor(t *testing.T, image, seccomp string) []string {
	t.Helper()
	appArmorOnce.Do(func() {
		if set := os.Getenv("BROWSERPIER_TEST_APPARMOR"); set != "" {
			if set != "default" {
				appArmorArgs = []string{"--security-opt", "apparmor=" + set}
			}
			return
		}
		var tried []string
		for _, opt := range []string{"apparmor=browserpier", "", "apparmor=unconfined"} {
			args := []string{"run", "--rm", "--security-opt", "seccomp=" + seccomp}
			if opt != "" {
				args = append(args, "--security-opt", opt)
			}
			out, err := exec.Command("docker", append(args, image, "sandbox-check")...).CombinedOutput() //nolint:gosec // test arguments
			if err == nil {
				if opt != "" {
					appArmorArgs = []string{"--security-opt", opt}
				}
				return
			}
			tried = append(tried, fmt.Sprintf("%q: %s", opt, strings.TrimSpace(string(out))))
		}
		appArmorErr = fmt.Errorf("chromium's sandbox works under no AppArmor choice here; tried %s", strings.Join(tried, "; "))
	})
	if appArmorErr != nil {
		t.Fatal(appArmorErr)
	}
	t.Logf("AppArmor: %q (the daemon's default if empty)", appArmorArgs)
	return appArmorArgs
}

func newVolume(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("bp-test-%d", time.Now().UnixNano())
	docker(t, "volume", "create", name)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", name).Run() })
	return name
}

// browser is one running browser container.
type browser struct {
	t       *testing.T
	name    string
	sockDir string
	http    *http.Client
}

func startBrowser(t *testing.T, image, volume string) *browser {
	t.Helper()
	return startBrowserWith(t, image, volume, browserOptions{})
}

// browserOptions are what some tests add to the usual container.
type browserOptions struct {
	proxy string   // an upstream proxy URL, left in the control folder as the orchestrator does
	args  []string // more `docker run` options, before the image
}

func startBrowserWith(t *testing.T, image, volume string, opt browserOptions) *browser {
	t.Helper()
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o777); err != nil { // the container's user creates the socket
		t.Fatal(err)
	}
	if opt.proxy != "" {
		b, _ := json.Marshal(map[string]string{"url": opt.proxy})
		if err := os.WriteFile(filepath.Join(sockDir, "proxy.json"), b, 0o644); err != nil { //nolint:gosec // the container's user reads it
			t.Fatal(err)
		}
	}
	seccomp, err := filepath.Abs("seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bp-test-%d", time.Now().UnixNano())
	run := append([]string{"run", "-d", "--name", name,
		"--security-opt", "seccomp=" + seccomp},
		appArmor(t, image, seccomp)...)
	docker(t, slices.Concat(run, []string{
		"--shm-size=1g",
		"-e", "BROWSERPIER_SCREEN=1280x800",
		// The test sites are on the Docker host: the local network.
		"-e", "BROWSERPIER_LOCAL_NETWORK=true",
		"-v", volume + ":/profile",
		"-v", sockDir + ":/run/browserpier"},
		opt.args, []string{image})...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	sock := filepath.Join(sockDir, "control.sock")
	b := &browser{t: t, name: name, sockDir: sockDir, http: browsertest.ControlClient(sock)}
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := b.http.Get("http://control/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser not ready after 60s\n%s", b.logs())
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (b *browser) logs() string {
	out, _ := exec.Command("docker", "logs", b.name).CombinedOutput()
	return string(out)
}

// stop stops the container the way the orchestrator will, and returns its exit code.
func (b *browser) stop() string {
	b.t.Helper()
	docker(b.t, "stop", "-t", "30", b.name)
	return docker(b.t, "inspect", "-f", "{{.State.ExitCode}}", b.name)
}

// input posts a Visual-mode action to the control socket.
func (b *browser) input(t *testing.T, path, body string) {
	t.Helper()
	b.inputWithin(t, path, body, 5*time.Second)
}

// inputWithin is input for an action that may take up to most, such as
// typing a long text at human pace.
func (b *browser) inputWithin(t *testing.T, path, body string, most time.Duration) {
	t.Helper()
	start := time.Now()
	resp, err := b.http.Post("http://control"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s %s: %d %s", path, body, resp.StatusCode, msg)
	}
	// xdotool's mousemove --sync to where the pointer already was took 15 s.
	if d := time.Since(start); d > most {
		t.Fatalf("%s %s took %v", path, body, d)
	}
}

// onScreen waits until the screen shows want at (x, y). A page can load
// before Chrome paints it, and Chrome drops input on a page it hasn't
// painted yet, so a test checks the screen before it clicks or drags.
func (b *browser) onScreen(t *testing.T, x, y int, want color.RGBA) {
	t.Helper()
	near := func(a, b uint8) bool { d := int(a) - int(b); return d > -8 && d < 8 }
	var got color.RGBA
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := b.http.Get("http://control/screenshot")
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		got = color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
		if near(got.R, want.R) && near(got.G, want.G) && near(got.B, want.B) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the screen shows %v at (%d, %d), want %v", got, x, y, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// pageURLs lists the browser's tabs' URLs, from CDP's /json/list.
func (b *browser) pageURLs() []string {
	b.t.Helper()
	resp, err := b.http.Get("http://control/cdp/json/list")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var targets []struct{ Type, URL string }
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		b.t.Fatal(err)
	}
	var urls []string
	for _, tg := range targets {
		if tg.Type == "page" {
			urls = append(urls, tg.URL)
		}
	}
	return urls
}

func (b *browser) page() *browsertest.CDP {
	b.t.Helper()
	return browsertest.Page(b.t, b.http)
}

// TestLoginSurvivesStopStartAndRebuild is PROJ-4's done-when: a login made just
// before a stop survives a restart and an image rebuild. Chromium flushes
// cookies lazily, so a cookie set a moment before the stop only survives if
// Chromium was closed gracefully.
func TestLoginSurvivesStopStartAndRebuild(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	token := s.Login(t, p)
	p.Close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	if logs := b.logs(); !strings.Contains(logs, "Chromium closed") || strings.Contains(logs, "SIGTERM instead") {
		t.Fatalf("Chromium wasn't closed through CDP\n%s", logs)
	}

	b = startBrowser(t, image, vol)
	if got := s.Account(t, b.page()); got != token {
		t.Fatalf("after restart the site saw session %q, want %q", got, token)
	}
	b.stop()

	rebuilt := image + "-rebuilt"
	out, err := exec.Command("docker", "build", "--build-arg", fmt.Sprintf("REBUILD=%d", time.Now().UnixNano()),
		"-f", "Dockerfile", "-t", rebuilt, "../..").CombinedOutput()
	if err != nil {
		t.Fatalf("rebuild: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", rebuilt).Run() })
	if docker(t, "image", "inspect", "-f", "{{.Id}}", rebuilt) == docker(t, "image", "inspect", "-f", "{{.Id}}", image) {
		t.Fatal("rebuild produced the same image")
	}
	b = startBrowser(t, rebuilt, vol)
	if got := s.Account(t, b.page()); got != token {
		t.Fatalf("after rebuild the site saw session %q, want %q", got, token)
	}
	b.stop()
}

// TestKillLosesFreshLogin is the negative control for the test above: without
// a graceful close, a cookie set a moment before the stop is lost. If this
// starts failing, the test above no longer proves anything.
func TestKillLosesFreshLogin(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	token := s.Login(t, p)
	p.Close()
	docker(t, "kill", b.name)

	// The new container also proves a stale profile lock from a killed one is cleared.
	b = startBrowser(t, image, vol)
	if got := s.Account(t, b.page()); got == token {
		t.Fatal("a cookie set just before docker kill survived; the graceful-stop test can't tell graceful from not")
	}
	b.stop()
}

// TestSessionCookieLoginSurvivesStopStart is PROJ-112's check: a login kept
// in a session-only cookie (no Expires or Max-Age, as when "remember me" is
// left unticked) survives a graceful stop and start, as SPEC §5 promises for
// every login. TestLoginSurvivesStopStartAndRebuild can't tell: its cookie
// is persistent.
func TestSessionCookieLoginSurvivesStopStart(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	s.SessionCookie()
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	token := s.Login(t, p)
	// The premise: the browser holds the login as a session cookie.
	var got struct {
		Cookies []struct {
			Name    string
			Session bool
		}
	}
	if err := json.Unmarshal(p.Call("Network.getCookies", map[string]any{"urls": []string{s.URL}}), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Name != "session" || !got.Cookies[0].Session {
		t.Fatalf("want one session-only cookie named session, got %+v", got.Cookies)
	}
	if seen := s.Account(t, p); seen != token {
		t.Fatalf("before the stop the site saw session %q, want %q", seen, token)
	}
	p.Close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	if logs := b.logs(); !strings.Contains(logs, "Chromium closed") || strings.Contains(logs, "SIGTERM instead") {
		t.Fatalf("Chromium wasn't closed through CDP\n%s", logs)
	}

	b = startBrowser(t, image, vol)
	if seen := s.Account(t, b.page()); seen != token {
		t.Fatalf("after a graceful stop and start the site saw session %q, want %q: the session-only cookie was lost", seen, token)
	}
	b.stop()
}

// TestSessionStorageSurvivesStopStart is PROJ-112's second check: a token an
// app keeps in sessionStorage (OAuth implicit flows do) is still in its tab
// after a graceful stop and start. sessionStorage belongs to a tab, so this
// needs the tab back.
func TestSessionStorageSurvivesStopStart(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	p.Navigate(s.URL + "/account")
	p.Eval(`sessionStorage.setItem('token', 'kept-in-session-storage')`)
	p.Close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}

	b = startBrowser(t, image, vol)
	p = b.page()
	deadline := time.Now().Add(15 * time.Second)
	for {
		href, _ := p.Eval("location.href").(string)
		if strings.HasPrefix(href, s.URL) && p.Eval("document.readyState") == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a graceful stop and start the first tab is at %q, not the site's page: no tab was restored, so its sessionStorage is gone", href)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v := p.Eval(`sessionStorage.getItem('token')`); v != "kept-in-session-storage" {
		t.Fatalf("the restored tab's sessionStorage token is %v, want kept-in-session-storage", v)
	}
	b.stop()
}

// TestRestoredPostPageIsNotResubmitted: with "continue where you left off",
// a tab left on the answer to a form POST (a login, a payment) comes back
// after a stop, and Chromium must not send the form again by itself.
func TestRestoredPostPageIsNotResubmitted(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	token := s.Login(t, p) // the tab stays on POST /login's answer
	p.Close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	posts := s.Requests()

	b = startBrowser(t, image, vol)
	b.page().Close()
	// The tab on POST /login's answer is restored: without it, nothing
	// could be resubmitted and the check below would pass for nothing.
	start := time.Now()
	for !slices.Contains(b.pageURLs(), s.URL+"/login") {
		if time.Since(start) > 15*time.Second {
			t.Fatalf("no tab at %s/login after the restart; tabs %q", s.URL, b.pageURLs())
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Long enough for a restored tab to load, or post, several times over.
	time.Sleep(5 * time.Second)
	if got := s.Issued(); got != token {
		t.Fatalf("after the restart the login form was posted again: a new session %q was issued (was %q)", got, token)
	}
	t.Logf("requests during the restart: %d", s.Requests()-posts)
	b.stop()
}

func TestBrowserEnvironment(t *testing.T) {
	image := testImage(t)
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	b := startBrowser(t, image, newVolume(t))
	p := b.page()

	t.Run("sandbox", func(t *testing.T) {
		p.Navigate("chrome://sandbox/")
		deadline := time.Now().Add(10 * time.Second)
		for {
			text, _ := p.Eval("document.body.innerText").(string)
			if strings.Contains(text, "You are adequately sandboxed") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("chrome://sandbox says:\n%s", text)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	p.Navigate(s.URL + "/account")
	t.Run("no warning bar over the page", func(t *testing.T) {
		// Tabs and toolbar take about 87px. Without the managed policy the
		// unsupported-flag bar made this about 143.
		if ui, _ := p.Eval("window.outerHeight - window.innerHeight").(float64); ui > 120 {
			t.Fatalf("browser UI above the page is %vpx tall; is an infobar showing?", ui)
		}
	})
	t.Run("no automation flag", func(t *testing.T) {
		if v := p.Eval("navigator.webdriver"); v != false {
			t.Fatalf("navigator.webdriver = %v", v)
		}
	})
	t.Run("webgl", func(t *testing.T) {
		v := p.Eval(`(() => { const gl = document.createElement('canvas').getContext('webgl');
			if (!gl) return null;
			const d = gl.getExtension('WEBGL_debug_renderer_info');
			return d ? gl.getParameter(d.UNMASKED_RENDERER_WEBGL) : 'unknown'; })()`)
		if v == nil {
			t.Fatal("no WebGL context")
		}
		t.Logf("WebGL renderer: %v", v)
	})
	t.Run("webrtc sends no unproxied udp", func(t *testing.T) {
		// With no proxy, disable_non_proxied_udp leaves WebRTC no UDP candidates at all.
		v := p.Eval(`new Promise(resolve => {
			const pc = new RTCPeerConnection({iceServers: []});
			const found = [];
			pc.onicecandidate = e => { if (e.candidate) found.push(e.candidate.candidate); else resolve(found); };
			pc.createDataChannel('x');
			pc.createOffer().then(o => pc.setLocalDescription(o));
			setTimeout(() => resolve(found), 5000);
		})`)
		found, ok := v.([]any)
		if !ok {
			t.Fatalf("candidate gathering returned %v", v)
		}
		for _, c := range found {
			if strings.Contains(strings.ToLower(fmt.Sprint(c)), " udp ") {
				t.Fatalf("UDP ICE candidate gathered: %v", c)
			}
		}
		t.Logf("ICE candidates: %v", found)
	})
	t.Run("window fills the screen above the taskbar", func(t *testing.T) {
		// Visual-mode coordinates are screen coordinates, so the window must
		// sit at 0,0, unframed, and fill the screen but for the 32px taskbar.
		out := docker(t, "exec", b.name, "xdotool", "search", "--onlyvisible", "--class", "chromium", "getwindowgeometry", "%@")
		if !regexp.MustCompile(`Position: 0,0 .*\n\s*Geometry: 1280x768\b`).MatchString(out) {
			t.Fatalf("no window at 0,0 filling 1280x768 of the 1280x800 display:\n%s", out)
		}
		// The taskbar's strip is reserved, as on a person's desktop:
		// screen.availHeight equal to screen.height says headless.
		if got := p.Eval("[screen.width, screen.height, screen.availWidth, screen.availHeight, outerWidth, outerHeight].join()"); got != "1280,800,1280,768,1280,768" {
			t.Fatalf("screen, available screen and window are %v, want 1280,800,1280,768,1280,768", got)
		}
	})
	t.Run("xdotool input", func(t *testing.T) {
		docker(t, "exec", b.name, "xdotool", "mousemove", "40", "50")
		if out := docker(t, "exec", b.name, "xdotool", "getmouselocation"); !strings.HasPrefix(out, "x:40 y:50 ") {
			t.Fatalf("mouse at %q, want x:40 y:50", out)
		}
	})
	// Test pages paint their targets blue, so a test can see they're on screen.
	blue := color.RGBA{0x2f, 0x6b, 0xff, 0xff}
	t.Run("visual mode click and type", func(t *testing.T) {
		// A page with a text box; find it on screen, click it through xdotool
		// and type, then read the value back over CDP to check it landed.
		p.Navigate(`data:text/html,<input id="box" style="position:absolute;left:200px;top:150px;width:300px;height:40px;background:%232f6bff">`)
		chrome, _ := p.Eval("window.outerHeight - window.innerHeight").(float64)
		x := 200 + 150
		y := int(chrome) + 150 + 20
		b.onScreen(t, x, y, blue)
		post := func(path, body string) {
			resp, err := b.http.Post("http://control"+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("%s: %d", path, resp.StatusCode)
			}
		}
		post("/input/click", fmt.Sprintf(`{"x": %d, "y": %d}`, x, y))
		post("/input/type", `{"text": "hello pier"}`)
		deadline := time.Now().Add(5 * time.Second)
		for p.Eval("document.getElementById('box').value") != "hello pier" {
			if time.Now().After(deadline) {
				t.Fatalf("box holds %v after click at (%d, %d) and typing", p.Eval("document.getElementById('box').value"), x, y)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	// The computer actions, checked the same way: input through xdotool,
	// the outcome read back over CDP.
	chromeTop := func() int {
		h, _ := p.Eval("window.outerHeight - window.innerHeight").(float64)
		return int(h)
	}
	until := func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Run("visual mode scroll", func(t *testing.T) {
		p.Navigate(`data:text/html,<body style="margin:0;height:5000px;width:5000px">`)
		until(t, "the page didn't load", func() bool { return p.Eval("document.readyState") == "complete" })
		// A page just loaded can drop the first wheel events, so scroll
		// again until it moves (each scroll moves it when it takes them).
		scrollUntil := func(what, body, prop string) {
			t.Helper()
			deadline := time.Now().Add(8 * time.Second)
			for {
				b.input(t, "/input/scroll", body)
				end := time.Now().Add(time.Second)
				for time.Now().Before(end) {
					if v, _ := p.Eval(prop).(float64); v > 0 {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				if time.Now().After(deadline) {
					t.Fatal(what)
				}
			}
		}
		scrollUntil("window.scrollY is still 0 after scrolling down", `{"x": 640, "y": 400, "dy": 3}`, "window.scrollY")
		scrollUntil("window.scrollX is still 0 after scrolling right", `{"x": 640, "y": 400, "dx": 2}`, "window.scrollX")
	})
	t.Run("visual mode keys", func(t *testing.T) {
		p.Navigate(`data:text/html,<input id="box" value="old text" style="position:absolute;left:200px;top:150px;width:300px;height:40px;background:%232f6bff">`)
		top := chromeTop()
		b.onScreen(t, 450, top+170, blue)
		b.input(t, "/input/click", fmt.Sprintf(`{"x": 350, "y": %d}`, top+170))
		b.input(t, "/input/key", `{"keys": "ctrl+a"}`)
		b.input(t, "/input/type", `{"text": "new pier"}`)
		b.input(t, "/input/key", `{"keys": ["Left", "Left", "Left", "Left", "backspace"]}`)
		until(t, "ctrl+a, typing and the arrow keys didn't edit the box", func() bool {
			return p.Eval("document.getElementById('box').value") == "newpier"
		})
	})
	t.Run("visual mode drag", func(t *testing.T) {
		// The page records the pointer: press, moves with the button held, release.
		p.Navigate(`data:text/html,<div id="d" style="position:absolute;left:100px;top:100px;width:600px;height:300px;background:%232f6bff"></div>` +
			`<script>window.got = []; const d = document.getElementById('d');` +
			` d.onmousedown = e => got.push('down ' + e.clientX + ',' + e.clientY);` +
			` d.onmousemove = e => { if (e.buttons === 1 && got.at(-1) !== 'move') got.push('move'); };` +
			` d.onmouseup = e => got.push('up ' + e.clientX + ',' + e.clientY);</script>`)
		top := chromeTop()
		b.onScreen(t, 150, top+150, blue)
		b.input(t, "/input/drag", fmt.Sprintf(`{"from": {"x": 150, "y": %d}, "to": {"x": 500, "y": %d}}`, top+150, top+350))
		const want = "down 150,150 | move | up 500,350"
		var got any
		deadline := time.Now().Add(5 * time.Second)
		for got = p.Eval("got.join(' | ')"); got != want; got = p.Eval("got.join(' | ')") {
			if time.Now().After(deadline) {
				t.Fatalf("the page saw %q, want %q", got, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	t.Run("screenshot", func(t *testing.T) {
		for width, want := range map[string][2]int{"": {1280, 800}, "?width=640": {640, 400}} {
			resp, err := b.http.Get("http://control/screenshot" + width)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("screenshot%s isn't a PNG: %v", width, err)
			}
			if got := img.Bounds().Size(); got.X != want[0] || got.Y != want[1] {
				t.Fatalf("screenshot%s is %v, want %v", width, got, want)
			}
		}
	})
	t.Run("h264 stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://control/stream", nil)
		resp, err := b.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		types := map[byte]bool{}
		for i := 0; i+3 < len(data); i++ {
			if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
				types[data[i+3]&0x1f] = true
			}
		}
		// 7 SPS, 8 PPS, 5 IDR slice.
		if !types[7] || !types[8] || !types[5] {
			t.Fatalf("got %d bytes with NAL types %v, want SPS, PPS and an IDR frame", len(data), types)
		}
	})
	t.Run("no zombies", func(t *testing.T) {
		time.Sleep(time.Second) // let the stream's sidecar exit
		if out := docker(t, "top", b.name, "-eo", "pid,stat,args"); regexp.MustCompile(`(?m)^\s*\d+\s+Z`).MatchString(out) {
			t.Fatalf("zombie processes:\n%s", out)
		}
	})

	if dir := os.Getenv("BROWSERPIER_TEST_ARTIFACTS"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		docker(t, "exec", b.name, "gst-launch-1.0", "-q", "ximagesrc", "display-name=:99", "num-buffers=1", "!",
			"videoconvert", "!", "pngenc", "!", "filesink", "location=/tmp/screenshot.png")
		docker(t, "cp", b.name+":/tmp/screenshot.png", filepath.Join(dir, "screenshot.png"))
	}
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	if logs := b.logs(); bytes.Contains([]byte(logs), []byte("No usable sandbox")) {
		t.Fatalf("Chromium had no sandbox:\n%s", logs)
	}
}
