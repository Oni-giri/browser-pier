//go:build docker

// These tests run the real browser image. They need Docker and an image built
// with `make image-chromium`; BROWSERPIER_TEST_IMAGE names it. Set
// BROWSERPIER_TEST_ARTIFACTS to a directory to keep a screenshot.
package chromium

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
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
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o777); err != nil { // the container's user creates the socket
		t.Fatal(err)
	}
	seccomp, err := filepath.Abs("seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bp-test-%d", time.Now().UnixNano())
	docker(t, "run", "-d", "--name", name,
		"--security-opt", "seccomp="+seccomp,
		"--shm-size=1g",
		"--add-host=host.docker.internal:host-gateway",
		"-e", "BROWSERPIER_SCREEN=1280x800",
		"-v", volume+":/profile",
		"-v", sockDir+":/run/browserpier",
		image)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	sock := filepath.Join(sockDir, "control.sock")
	b := &browser{t: t, name: name, sockDir: sockDir, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
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

// cdp is a connection to the browser's first page target.
type cdp struct {
	t    *testing.T
	conn *websocket.Conn
	mu   sync.Mutex
	id   int
}

func (b *browser) page() *cdp {
	b.t.Helper()
	resp, err := b.http.Get("http://control/cdp/json/list")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var targets []struct{ Type, WebSocketDebuggerURL string }
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		b.t.Fatal(err)
	}
	for _, tg := range targets {
		if tg.Type != "page" {
			continue
		}
		// ws://127.0.0.1:9222/devtools/page/ID → the same path behind the relay.
		path := tg.WebSocketDebuggerURL[strings.Index(tg.WebSocketDebuggerURL, "/devtools/"):]
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		//nolint:bodyclose // after an upgrade the connection owns the body
		conn, _, err := websocket.Dial(ctx, "ws://control/cdp"+path, &websocket.DialOptions{HTTPClient: b.http})
		if err != nil {
			b.t.Fatal(err)
		}
		conn.SetReadLimit(64 << 20)
		c := &cdp{t: b.t, conn: conn}
		b.t.Cleanup(c.close)
		return c
	}
	b.t.Fatalf("no page target in %+v", targets)
	return nil
}

func (c *cdp) close() { _ = c.conn.Close(websocket.StatusNormalClosure, "") }

func (c *cdp) call(method string, params any) json.RawMessage {
	c.t.Helper()
	r, err := c.try(method, params)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return r
}

// try is call that returns CDP errors, which are normal while a page navigates.
func (c *cdp) try(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg, _ := json.Marshal(map[string]any{"id": c.id, "method": method, "params": params})
	if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return nil, err
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     int
			Result json.RawMessage
			Error  *struct{ Message string }
		}
		if err := json.Unmarshal(data, &resp); err != nil || resp.ID != c.id {
			continue // an event
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// eval runs a JavaScript expression in the page over CDP and returns its value.
// It returns nil if the page is between documents.
func (c *cdp) eval(expr string) any {
	c.t.Helper()
	raw, err := c.try("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return nil
	}
	var r struct {
		Result struct{ Value any }
	}
	_ = json.Unmarshal(raw, &r)
	return r.Result.Value
}

func (c *cdp) navigate(url string) {
	c.t.Helper()
	c.call("Page.navigate", map[string]any{"url": url})
	deadline := time.Now().Add(30 * time.Second)
	for c.eval("location.href") != url || c.eval("document.readyState") != "complete" {
		if time.Now().After(deadline) {
			c.t.Fatalf("%s did not load; at %v", url, c.eval("location.href"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// site is a test site with a password login that sets a persistent cookie.
type site struct {
	url string
	mu  sync.Mutex
	// issued is the session token from the last login, seen the cookie on the last /account visit.
	issued, seen string
}

func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<form method="post" action="/login"><input name="user"><input name="password" type="password"><button>Sign in</button></form>`)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("user") != "pier" || r.FormValue("password") != "hunter2" {
			http.Error(w, "wrong password", http.StatusUnauthorized)
			return
		}
		token := fmt.Sprintf("tok-%d", time.Now().UnixNano())
		s.mu.Lock()
		s.issued = token
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", MaxAge: 86400, HttpOnly: true})
		_, _ = io.WriteString(w, "<p>Signed in</p>")
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		v := ""
		if c, err := r.Cookie("session"); err == nil {
			v = c.Value
		}
		s.mu.Lock()
		s.seen = v
		s.mu.Unlock()
		if v == "" {
			_, _ = io.WriteString(w, "<h1>Signed out</h1>")
			return
		}
		_, _ = io.WriteString(w, "<h1>Signed in as pier</h1><p>BrowserPier persistence test</p>")
	})
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("site: %s %s", r.Method, r.URL.Path)
		// Content sniffing doesn't take a page starting with <form> as HTML.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: logged, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.url = fmt.Sprintf("http://host.docker.internal:%d", ln.Addr().(*net.TCPAddr).Port)
	return s
}

func (s *site) state() (issued, seen string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued, s.seen
}

// login signs in through the page's form and returns once the cookie is set.
func (s *site) login(t *testing.T, p *cdp) string {
	t.Helper()
	before, _ := s.state()
	p.navigate(s.url + "/login")
	p.eval(`document.querySelector('[name=user]').value = 'pier';
		document.querySelector('[name=password]').value = 'hunter2';
		setTimeout(() => document.forms[0].submit(), 0)`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if issued, _ := s.state(); issued != before {
			// The response carrying the cookie must have reached the browser.
			for p.eval("document.body && document.body.innerText") != "Signed in" {
				if time.Now().After(deadline) {
					t.Fatal("no signed-in page after login")
				}
				time.Sleep(50 * time.Millisecond)
			}
			return issued
		}
		if time.Now().After(deadline) {
			t.Fatalf("login form was not submitted; page at %v says %q", p.eval("location.href"), p.eval("document.body && document.body.innerText"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// account visits /account and returns the session cookie the site received.
func (s *site) account(t *testing.T, p *cdp) string {
	t.Helper()
	p.navigate(s.url + "/account")
	_, seen := s.state()
	return seen
}

// TestLoginSurvivesStopStartAndRebuild is PROJ-4's done-when: a login made just
// before a stop survives a restart and an image rebuild. Chromium flushes
// cookies lazily, so a cookie set a moment before the stop only survives if
// Chromium was closed gracefully.
func TestLoginSurvivesStopStartAndRebuild(t *testing.T) {
	image := testImage(t)
	s := newSite(t)
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	token := s.login(t, p)
	p.close()
	if code := b.stop(); code != "0" {
		t.Fatalf("exit code after stop = %s, want 0\n%s", code, b.logs())
	}
	if logs := b.logs(); !strings.Contains(logs, "Chromium closed") || strings.Contains(logs, "sending SIGTERM") {
		t.Fatalf("Chromium wasn't closed through CDP\n%s", logs)
	}

	b = startBrowser(t, image, vol)
	if got := s.account(t, b.page()); got != token {
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
	if got := s.account(t, b.page()); got != token {
		t.Fatalf("after rebuild the site saw session %q, want %q", got, token)
	}
	b.stop()
}

// TestKillLosesFreshLogin is the negative control for the test above: without
// a graceful close, a cookie set a moment before the stop is lost. If this
// starts failing, the test above no longer proves anything.
func TestKillLosesFreshLogin(t *testing.T) {
	image := testImage(t)
	s := newSite(t)
	vol := newVolume(t)

	b := startBrowser(t, image, vol)
	p := b.page()
	token := s.login(t, p)
	p.close()
	docker(t, "kill", b.name)

	// The new container also proves a stale profile lock from a killed one is cleared.
	b = startBrowser(t, image, vol)
	if got := s.account(t, b.page()); got == token {
		t.Fatal("a cookie set just before docker kill survived; the graceful-stop test can't tell graceful from not")
	}
	b.stop()
}

func TestBrowserEnvironment(t *testing.T) {
	image := testImage(t)
	s := newSite(t)
	b := startBrowser(t, image, newVolume(t))
	p := b.page()

	t.Run("sandbox", func(t *testing.T) {
		p.navigate("chrome://sandbox/")
		deadline := time.Now().Add(10 * time.Second)
		for {
			text, _ := p.eval("document.body.innerText").(string)
			if strings.Contains(text, "You are adequately sandboxed") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("chrome://sandbox says:\n%s", text)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	p.navigate(s.url + "/account")
	t.Run("no automation flag", func(t *testing.T) {
		if v := p.eval("navigator.webdriver"); v != false {
			t.Fatalf("navigator.webdriver = %v", v)
		}
	})
	t.Run("webgl", func(t *testing.T) {
		v := p.eval(`(() => { const gl = document.createElement('canvas').getContext('webgl');
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
		v := p.eval(`new Promise(resolve => {
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
	t.Run("window covers the display", func(t *testing.T) {
		// Visual-mode coordinates are screen coordinates, so the window must sit at 0,0.
		// Without a window manager Chromium comes out a pixel short of --window-size.
		out := docker(t, "exec", b.name, "xdotool", "search", "--onlyvisible", "--class", "chromium", "getwindowgeometry", "%@")
		for _, m := range regexp.MustCompile(`Position: 0,0 .*\n\s*Geometry: (\d+)x(\d+)`).FindAllStringSubmatch(out, -1) {
			var w, h int
			_, _ = fmt.Sscan(m[1], &w)
			_, _ = fmt.Sscan(m[2], &h)
			if w >= 1278 && w <= 1280 && h >= 798 && h <= 800 {
				return
			}
		}
		t.Fatalf("no window at 0,0 covering the 1280x800 display:\n%s", out)
	})
	t.Run("xdotool input", func(t *testing.T) {
		docker(t, "exec", b.name, "xdotool", "mousemove", "40", "50")
		if out := docker(t, "exec", b.name, "xdotool", "getmouselocation"); !strings.HasPrefix(out, "x:40 y:50 ") {
			t.Fatalf("mouse at %q, want x:40 y:50", out)
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
