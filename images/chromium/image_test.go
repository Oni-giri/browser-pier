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
	"strconv"
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

// startBrowser runs the image on volume; env adds KEY=VALUE settings.
func startBrowser(t *testing.T, image, volume string, env ...string) *browser {
	t.Helper()
	return startBrowserEnv(t, image, volume, env...)
}

// startBrowserEnv is startBrowser with more environment for the
// container (KEY=value).
func startBrowserEnv(t *testing.T, image, volume string, env ...string) *browser {
	t.Helper()
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o777); err != nil { // the container's user creates the socket
		t.Fatal(err)
	}
	seccomp, err := filepath.Abs("seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bp-test-%d", time.Now().UnixNano())
	// The orchestrator's own confinement (internal/docker's browserConfig):
	// its seccomp profile, no new privileges, every capability dropped but
	// the one the sandbox uses, so what these tests see of the sandbox
	// doesn't depend on the runtime's defaults.
	run := append([]string{"run", "-d", "--name", name,
		"--security-opt", "seccomp=" + seccomp,
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL", "--cap-add", "SYS_CHROOT"},
		appArmor(t, image, seccomp)...)
	run = append(run,
		"--shm-size=1g",
		"-e", "BROWSERPIER_SCREEN=1280x800",
		// The test sites are on the Docker host: the local network.
		"-e", "BROWSERPIER_LOCAL_NETWORK=true",
		"-v", volume+":/profile",
		"-v", sockDir+":/run/browserpier")
	for _, e := range env {
		run = append(run, "-e", e)
	}
	docker(t, append(run, image)...)
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
	if d := time.Since(start); d > 5*time.Second {
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

// TestLoginSurvivesImageUpgrade: a profile made by an older image, with its
// GPU caches, starts cleanly on this one and keeps its login. It needs the
// older image, named by BROWSERPIER_TEST_OLD_IMAGE (0.2.1 for PROJ-148,
// whose WebGL renderer moved from SwiftShader to llvmpipe). CI doesn't set
// it: run it by hand when the image changes (CONTRIBUTING.md).
func TestLoginSurvivesImageUpgrade(t *testing.T) {
	image := testImage(t)
	old := os.Getenv("BROWSERPIER_TEST_OLD_IMAGE")
	if old == "" {
		t.Skip("BROWSERPIER_TEST_OLD_IMAGE not set")
	}
	s := browsertest.NewSite(t, browsertest.SiteHost(t))
	vol := newVolume(t)

	b := startBrowser(t, old, vol)
	p := b.page()
	token := s.Login(t, p)
	p.Navigate("about:blank")
	before := readWebGL(t, p)["webgl"]
	p.Close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	if before == nil {
		t.Fatalf("no WebGL on %s: the old profile has no renderer state to carry over", old)
	}
	// The old renderer left shader caches for the new one to find: looked
	// at before the new browser starts, which would create the folder itself.
	if out := docker(t, "run", "--rm", "--network", "none", "-v", vol+":/profile:ro", "--entrypoint", "sh", image,
		"-c", "find /profile/GPUCache /profile/Default/GPUCache -type f 2>/dev/null | head -n 1"); out == "" {
		t.Fatal("no GPUCache files in the old profile: the test isn't checking what it should")
	}

	b = startBrowser(t, image, vol)
	p = b.page()
	if got := s.Account(t, p); got != token {
		t.Fatalf("on the new image the site saw session %q, want %q", got, token)
	}
	p.Navigate("about:blank")
	after := readWebGL(t, p)
	p.Close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	checkGPUProcess(t, b.logs())
	for _, kind := range []string{"webgl", "webgl2"} {
		w := after[kind]
		if w == nil {
			t.Fatalf("no %s on the new image: %v", kind, after)
		}
		if !strings.Contains(w.Renderer, "llvmpipe") || w.Pixel != triangleRGBA {
			t.Errorf("%s on the new image: renderer %q, pixel %s, want llvmpipe drawing %s", kind, w.Renderer, w.Pixel, triangleRGBA)
		}
	}
	t.Logf("renderer on %s: %s / %s", old, before.Vendor, before.Renderer)
	t.Logf("renderer on %s: %s / %s", image, after["webgl"].Vendor, after["webgl"].Renderer)
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
		// WebGL and WebGL2 both render on llvmpipe, and draw (PROJ-148).
		for kind, gl := range readWebGL(t, p) {
			if gl == nil {
				t.Errorf("no %s context", kind)
				continue
			}
			t.Logf("%s: %s / %s", kind, gl.Vendor, gl.Renderer)
			if !strings.Contains(gl.Renderer, "llvmpipe") {
				t.Errorf("%s renders on %q, want llvmpipe", kind, gl.Renderer)
			}
			if strings.Contains(strings.ToLower(gl.Vendor+gl.Renderer), "swiftshader") {
				t.Errorf("%s is on SwiftShader: %s / %s", kind, gl.Vendor, gl.Renderer)
			}
			if gl.Pixel != triangleRGBA {
				t.Errorf("%s drew a triangle that reads back as %s, want %s", kind, gl.Pixel, triangleRGBA)
			}
		}
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
	t.Run("gpu", func(t *testing.T) {
		p.Navigate("chrome://gpu/")
		var text string
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(text, "GPU process crash count") {
			if time.Now().After(deadline) {
				t.Fatalf("chrome://gpu has no driver information:\n%s", text)
			}
			time.Sleep(200 * time.Millisecond)
			text, _ = p.Eval(deepText).(string)
		}
		row := func(name string) (string, bool) {
			m := regexp.MustCompile(`(?m)^\s*\*?\s*` + regexp.QuoteMeta(name) + `\s*:\s*(.*?)\s*$`).FindStringSubmatch(text)
			if m == nil {
				return "", false
			}
			return m[1], true
		}
		line := func(name string) string { v, _ := row(name); return v }
		// With software compositing Chromium says "Hardware accelerated but
		// at reduced performance"; blocked, it says "Disabled" or "Software
		// only, hardware acceleration unavailable".
		if w := line("WebGL"); !strings.HasPrefix(w, "Hardware accelerated") {
			t.Errorf("chrome://gpu says WebGL: %q", w)
		}
		// Chromium 156's report has no WebGL2 row (WebGL's covers both);
		// the webgl subtest checks a webgl2 context draws. If a WebGL2 row
		// comes back, it must say the same.
		if w, ok := row("WebGL2"); ok && !strings.HasPrefix(w, "Hardware accelerated") {
			t.Errorf("chrome://gpu says WebGL2: %q", w)
		}
		if g := line("GL implementation parts"); g != "(gl=egl-angle,angle=opengl)" {
			t.Errorf("GL implementation parts %q, want ANGLE's OpenGL backend", g)
		}
		if g := line("In-process GPU"); g != "false" {
			t.Errorf("In-process GPU: %q, want a GPU process of its own", g)
		}
		if g := line("GPU process crash count"); g != "0" {
			t.Errorf("GPU process crash count: %q", g)
		}
		// Chromium 156 here reports its GPU process unsandboxed whether
		// WebGL is on SwiftShader or llvmpipe: it has the container's
		// seccomp filter but not Chromium's own (ANTI-BOT.md, PROJ-165).
		// Pinned, so a change either way is noticed.
		switch sb := line("Sandboxed"); sb {
		case "false":
			t.Log("GPU process: Sandboxed false, as before PROJ-148 (PROJ-165)")
		case "true":
			t.Error("chrome://gpu now says the GPU process is sandboxed: tighten this test to require it (PROJ-165)")
		default:
			t.Errorf("chrome://gpu's Sandboxed line is %q; has the report changed?", sb)
		}
		// What it does have: a process of its own, no new privileges, and
		// at least the container's seccomp filter.
		status := docker(t, "exec", b.name, "sh", "-c", `for d in /proc/[0-9]*; do
			case $(tr '\0' ' ' < $d/cmdline 2>/dev/null) in /usr/lib/chromium/*--type=gpu-process*)
				grep -aE '^(NoNewPrivs|Seccomp|Seccomp_filters):' $d/status;; esac; done`)
		field := func(name string) int {
			m := regexp.MustCompile(`(?m)^` + name + `:\s*(\d+)`).FindStringSubmatch(status)
			if m == nil {
				return -1
			}
			n, _ := strconv.Atoi(m[1])
			return n
		}
		if field("NoNewPrivs") != 1 || field("Seccomp") != 2 || field("Seccomp_filters") < 1 {
			t.Errorf("the GPU process isn't under a seccomp filter with no new privileges:\n%s", status)
		}
		t.Logf("GPU process: %s", strings.Join(strings.Fields(status), " "))
	})
	// A canvas drawn with WebGL shows on the X display, which screenshots
	// and the live stream read, and isn't black or empty there.
	t.Run("webgl on screen", func(t *testing.T) {
		p.Navigate(trianglePage)
		b.onScreen(t, 640, pageTop(t, p)+200, color.RGBA{0x33, 0xcc, 0x66, 0xff})
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
	checkGPUProcess(t, b.logs())
}

// checkGPUProcess fails if the GPU process failed to start or crashed: then
// Chromium starts it again and again, and falls back to no WebGL.
func checkGPUProcess(t *testing.T, logs string) {
	t.Helper()
	for _, bad := range []string{"Exiting GPU process due to errors during initialization", "GPU process exited unexpectedly", "GPU process isn't usable"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("the GPU process failed (%s):\n%s", bad, logs)
		}
	}
}

// triangleRGBA is the colour readWebGL's triangle and trianglePage draw,
// (0.2, 0.8, 0.4) as bytes.
const triangleRGBA = "51,204,102,255"

// webGLInfo is what a page reads from one kind of WebGL context.
type webGLInfo struct {
	Vendor, Renderer string
	// Pixel is the middle of a triangle drawn over the whole canvas, read
	// back with readPixels as "R,G,B,A".
	Pixel string
}

// readWebGL reads the unmasked vendor and renderer of a webgl and a webgl2
// context, and has each draw a triangle and read it back. A kind with no
// context maps to nil.
func readWebGL(t *testing.T, p *browsertest.CDP) map[string]*webGLInfo {
	t.Helper()
	v, _ := p.Eval(`(() => {
		const out = {};
		for (const kind of ['webgl', 'webgl2']) {
			const c = document.createElement('canvas'); c.width = c.height = 64;
			const gl = c.getContext(kind);
			if (!gl) { out[kind] = null; continue; }
			const d = gl.getExtension('WEBGL_debug_renderer_info');
			const v2 = kind === 'webgl2';
			const vs = v2 ? '#version 300 es\nin vec2 p; void main() { gl_Position = vec4(p, 0.0, 1.0); }'
				: 'attribute vec2 p; void main() { gl_Position = vec4(p, 0.0, 1.0); }';
			const fs = v2 ? '#version 300 es\nprecision mediump float; out vec4 o; void main() { o = vec4(0.2, 0.8, 0.4, 1.0); }'
				: 'precision mediump float; void main() { gl_FragColor = vec4(0.2, 0.8, 0.4, 1.0); }';
			const prog = gl.createProgram();
			for (const [type, src] of [[gl.VERTEX_SHADER, vs], [gl.FRAGMENT_SHADER, fs]]) {
				const s = gl.createShader(type); gl.shaderSource(s, src); gl.compileShader(s); gl.attachShader(prog, s);
			}
			gl.linkProgram(prog); gl.useProgram(prog);
			gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
			gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
			const loc = gl.getAttribLocation(prog, 'p');
			gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
			gl.clearColor(0, 0, 0, 1); gl.clear(gl.COLOR_BUFFER_BIT); gl.drawArrays(gl.TRIANGLES, 0, 3);
			const px = new Uint8Array(4); gl.readPixels(32, 32, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, px);
			out[kind] = { vendor: d ? gl.getParameter(d.UNMASKED_VENDOR_WEBGL) : '',
				renderer: d ? gl.getParameter(d.UNMASKED_RENDERER_WEBGL) : '', pixel: Array.from(px).join(',') };
		}
		return JSON.stringify(out);
	})()`).(string)
	var out map[string]*webGLInfo
	if err := json.Unmarshal([]byte(v), &out); err != nil || len(out) != 2 {
		t.Fatalf("reading WebGL: %q: %v", v, err)
	}
	return out
}

// trianglePage fills the window with a canvas and draws readWebGL's
// triangle on it with WebGL, covering the whole canvas.
const trianglePage = `data:text/html,<body style="margin:0"><canvas id="c" style="display:block;width:100vw;height:100vh"></canvas><script>` +
	`const c = document.getElementById('c'); c.width = innerWidth; c.height = innerHeight; const gl = c.getContext('webgl');` +
	`const sh = (t, s) => { const x = gl.createShader(t); gl.shaderSource(x, s); gl.compileShader(x); return x; };` +
	`const p = gl.createProgram(); gl.attachShader(p, sh(gl.VERTEX_SHADER, 'attribute vec2 a; void main() { gl_Position = vec4(a, 0.0, 1.0); }'));` +
	`gl.attachShader(p, sh(gl.FRAGMENT_SHADER, 'precision mediump float; void main() { gl_FragColor = vec4(0.2, 0.8, 0.4, 1.0); }'));` +
	`gl.linkProgram(p); gl.useProgram(p); gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());` +
	`gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);` +
	`gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);` +
	`gl.clearColor(0, 0, 0, 1); gl.clear(gl.COLOR_BUFFER_BIT); gl.drawArrays(gl.TRIANGLES, 0, 3);</script>`

// deepText is a page's text, shadow roots included (chrome://gpu keeps
// its report in one), a block per line.
const deepText = `(() => { const walk = n => { let s = ''; if (n.shadowRoot) s += walk(n.shadowRoot);
	for (const c of n.childNodes) {
		if (c.nodeType === 3) s += c.textContent;
		else if (c.nodeType === 1 && !/^(STYLE|SCRIPT)$/.test(c.tagName) || c.nodeType === 11) { s += walk(c); if (c.nodeType === 1 && /^(DIV|LI|TR|P|H\d)$/.test(c.tagName)) s += '\n'; }
	}
	return s; }; return walk(document.body); })()`

// pageTop is the height of the browser's UI above the page.
func pageTop(t *testing.T, p *browsertest.CDP) int {
	t.Helper()
	h, _ := p.Eval("window.outerHeight - window.innerHeight").(float64)
	return int(h)
}

// TestWebGLRendererIsStable: a profile sees the same WebGL renderer each
// time it starts, and so does every profile on the same host and image
// (PROJ-148). There's one renderer and no fallback to change to.
func TestWebGLRendererIsStable(t *testing.T) {
	image := testImage(t)
	vol := newVolume(t)
	read := func(volume string) string {
		t.Helper()
		b := startBrowser(t, image, volume)
		p := b.page()
		p.Navigate("about:blank")
		gl := readWebGL(t, p)
		p.Close()
		if code := b.stop(); code != "0" {
			t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
		}
		checkGPUProcess(t, b.logs())
		if gl["webgl"] == nil || gl["webgl2"] == nil {
			t.Fatalf("no WebGL: %v", gl)
		}
		r := gl["webgl"].Vendor + " / " + gl["webgl"].Renderer
		if r2 := gl["webgl2"].Vendor + " / " + gl["webgl2"].Renderer; r2 != r {
			t.Errorf("webgl says %q, webgl2 %q", r, r2)
		}
		return r
	}
	first := read(vol)
	t.Logf("renderer: %s", first)
	if again := read(vol); again != first {
		t.Errorf("after a restart the profile's renderer is %q, was %q", again, first)
	}
	if other := read(newVolume(t)); other != first {
		t.Errorf("another profile's renderer is %q, the first's %q", other, first)
	}
}

// TestNoPortalNoBrowser (PROJ-181): a browser whose session bus can't
// start doesn't start, rather than fall back to Chromium's GTK dialog,
// which lets whoever drives it hand a page any file in the container
// (PROJ-183). Here the bus's folder is taken by one of the wrong mode.
func TestNoPortalNoBrowser(t *testing.T) {
	image := testImage(t)
	seccomp, err := filepath.Abs("seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	uid := docker(t, "run", "--rm", "--entrypoint", "id", image, "-u")
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o777); err != nil { // the container's user creates the socket
		t.Fatal(err)
	}
	name := fmt.Sprintf("bp-test-%d", time.Now().UnixNano())
	run := append([]string{"run", "-d", "--name", name, "--security-opt", "seccomp=" + seccomp}, appArmor(t, image, seccomp)...)
	docker(t, append(run, "--shm-size=1g", "-v", newVolume(t)+":/profile", "-v", sockDir+":/run/browserpier",
		"--mount", "type=tmpfs,destination=/tmp/browserpier-bus-"+uid+",tmpfs-mode=0755", image)...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	for deadline := time.Now().Add(60 * time.Second); docker(t, "inspect", "-f", "{{.State.Running}}", name) == "true"; time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the browser kept running without its session bus")
		}
	}
	out, _ := exec.Command("docker", "logs", name).CombinedOutput()
	logs := string(out)
	if code := docker(t, "inspect", "-f", "{{.State.ExitCode}}", name); code == "0" || !strings.Contains(logs, "the session bus for the file chooser portal: session bus folder") || strings.Contains(logs, `msg=ready`) {
		t.Fatalf("exit %s, want a failure naming the session bus, before ready:\n%s", code, logs)
	}
}

// TestSessionBus (PROJ-152): the browser container's session bus and
// file chooser portal. The bus runs, listens on one path socket in a
// 0700 folder and nowhere else (no TCP, no abstract socket); only
// Chromium has its address; no desktop portal or terminal is in the
// image; Chromium's renderers can't reach the socket; the sandbox holds.
func TestSessionBus(t *testing.T) {
	image := testImage(t)
	b := startBrowser(t, image, newVolume(t)) // the defaults: the portal is always on (PROJ-181)
	sh := func(script string) string {
		t.Helper()
		return docker(t, "exec", b.name, "sh", "-c", script)
	}
	pidOf := func(pattern string) string {
		t.Helper()
		pid := sh("for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null | grep -q -- '" + pattern + "' && { echo ${p#/proc/}; break; }; done")
		if pid == "" {
			t.Fatalf("no process matches %s", pattern)
		}
		return pid
	}
	bus := pidOf("^dbus-daemon --config-file=/tmp/browserpier-bus-")
	uid := sh("id -u")
	dir := "/tmp/browserpier-bus-" + uid
	if got := sh("stat -c '%a %u %F' " + dir); got != "700 "+uid+" directory" {
		t.Errorf("bus folder: %s", got)
	}
	// The bus's own sockets, by inode, against the kernel's tables.
	inodes := strings.Fields(sh("ls -l /proc/" + bus + "/fd 2>/dev/null | sed -n 's/.*socket:\\[\\([0-9]*\\)\\]/\\1/p'"))
	if len(inodes) == 0 {
		t.Fatal("the bus has no sockets")
	}
	unix := sh("cat /proc/net/unix")
	listening := 0
	for _, line := range strings.Split(unix, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 || !slices.Contains(inodes, f[6]) {
			continue
		}
		path := ""
		if len(f) > 7 {
			path = f[7]
		}
		if strings.HasPrefix(path, "@") {
			t.Errorf("the bus has an abstract socket: %s", line)
		}
		if f[3] == "00010000" { // listening
			listening++
			if path != dir+"/bus" {
				t.Errorf("the bus listens on %q", path)
			}
		}
	}
	if listening != 1 {
		t.Errorf("the bus listens on %d unix sockets, want 1", listening)
	}
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		for _, line := range strings.Split(sh("cat /proc/net/"+table+" 2>/dev/null || true"), "\n") {
			f := strings.Fields(line)
			if len(f) > 9 && slices.Contains(inodes, f[9]) {
				t.Errorf("the bus has a %s socket: %s", table, line)
			}
		}
	}
	// Only Chromium is told where the bus is. Its own environment can't
	// be read (it makes itself undumpable), so: it is on the bus.
	env := func(pid string) string { return sh("tr '\\0' '\\n' < /proc/" + pid + "/environ") }
	browserPid := pidOf("^/usr/lib/chromium/chromium --user-data-dir")
	call := func(method string, args ...string) string {
		return sh("dbus-send --bus=unix:path=" + dir + "/bus --print-reply --dest=org.freedesktop.DBus / org.freedesktop.DBus." + method + " " + strings.Join(args, " ") + " 2>/dev/null || true")
	}
	onBus := false
	for _, name := range regexp.MustCompile(`string "(:[0-9.]+)"`).FindAllStringSubmatch(call("ListNames"), -1) {
		if pid := regexp.MustCompile(`uint32 ([0-9]+)`).FindStringSubmatch(call("GetConnectionUnixProcessID", "string:"+name[1])); pid != nil && pid[1] == browserPid {
			onBus = true
		}
	}
	if !onBus {
		t.Error("Chromium isn't on the session bus")
	}
	for _, p := range []string{"^Xvfb ", "^jwm ", "^/usr/local/bin/browserpier-entrypoint"} {
		if strings.Contains(env(pidOf(p)), "DBUS_SESSION_BUS_ADDRESS") {
			t.Errorf("%s has the bus's address", p)
		}
	}
	// Nothing else to answer for the portal, nothing to open a terminal.
	if out := sh("ls /usr/libexec/xdg-desktop-portal* /usr/lib/xdg-desktop-portal* 2>/dev/null; command -v xdg-desktop-portal x-terminal-emulator xterm 2>/dev/null; true"); out != "" {
		t.Errorf("in the image: %s", out)
	}
	// Each renderer is sandboxed: seccomp, and its root an empty folder,
	// where no path reaches the bus's socket.
	renderers := strings.Fields(sh("for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null | grep -q -- '--type=renderer' && echo ${p#/proc/}; done; true"))
	if len(renderers) == 0 {
		t.Fatal("no renderer")
	}
	checked := 0
	for _, r := range renderers {
		root := sh("readlink /proc/" + r + "/root 2>/dev/null || true")
		seccomp := sh("grep Seccomp: /proc/" + r + "/status 2>/dev/null || true")
		if root == "" && seccomp == "" {
			continue // exited meanwhile
		}
		checked++
		if root == "/" || !strings.Contains(seccomp, "2") {
			t.Errorf("renderer %s: root %q, %s: it could reach %s", r, root, seccomp, dir+"/bus")
		}
	}
	if checked == 0 {
		t.Error("no renderer to check")
	}
	// The portal answers on the control socket.
	resp, err := b.http.Get("http://control/chooser")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"choosers":[]`) {
		t.Errorf("GET /chooser: %d %s", resp.StatusCode, body)
	}
	if logs := b.logs(); strings.Contains(logs, "no session bus") || strings.Contains(logs, "no file chooser portal") {
		t.Errorf("the portal didn't start:\n%s", logs)
	}
}
