//go:build docker

// End to end through the HTTP API with real browser containers; see
// internal/sessions/docker_test.go for the environment it needs.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

type agent struct {
	t     *testing.T
	base  string
	token string
	m     *sessions.Manager
}

func (a agent) do(method, path, body string, out any) int {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			a.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

func (a agent) header() http.Header { return http.Header{"Authorization": {"Bearer " + a.token}} }

func newRealAPI(t *testing.T) agent {
	t.Helper()
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := rt.UseDataDir(ctx, dir, dir); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range m.List() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, _ = m.Stop(ctx, s.ID)
			cancel()
		}
		m.Close()
	})
	tokens := auth.NewTokens(d)
	_, secret, err := tokens.Create(ctx, "e2e", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	mux := http.NewServeMux()
	New(m, web, files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), log, "").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return agent{t: t, base: srv.URL, token: secret, m: m}
}

// allowLocalNetwork lets the profile's pages reach the test sites, on the
// Docker host; agents can't, so it goes past the API.
func (a agent) allowLocalNetwork(t *testing.T, name string) {
	t.Helper()
	p, err := a.m.Profiles().Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	p.LocalNetwork = true
	if _, err := a.m.Profiles().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// TestAgentKeepsItsLoginThroughTheAPI is the done-when through the API an
// agent uses: create a profile, start it, log in over the CDP proxy, stop
// it, start it again and find the login still there.
func TestAgentKeepsItsLoginThroughTheAPI(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("e%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })

	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "api", "screen": {"width": 1280, "height": 800}}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || s.State != "running" {
		t.Fatalf("start: %d %+v", code, s)
	}
	cdpHTTP := strings.Replace(s.CDPURL, "ws://", "http://", 1)
	p := browsertest.PageVia(t, cdpHTTP+"/json/list", a.header())
	token := site.Login(t, p)
	p.Close()

	// The screenshot comes from the X display, at the size asked for.
	resp, err := http.DefaultClient.Do(func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, a.base+"/api/sessions/"+s.ID+"/screenshot?width=640", nil)
		req.Header = a.header()
		return req
	}())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(resp.Body)
	_ = resp.Body.Close()
	if err != nil || img.Bounds().Dx() != 640 || img.Bounds().Dy() != 400 {
		t.Fatalf("screenshot: %v, %v", err, img.Bounds())
	}

	var stopped sessionJSON
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", &stopped); code != http.StatusOK || stopped.State != "stopped" {
		t.Fatalf("stop: %d %+v", code, stopped)
	}
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	if got := site.Account(t, browsertest.PageVia(t, cdpHTTP+"/json/list", a.header())); got != token {
		t.Fatalf("after stop and start the site saw session %q, want %q", got, token)
	}
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("final stop: %d", code)
	}

	// In Visual mode nothing may attach over CDP, but the screen and input work.
	var vis sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": "visual"}`, name), &vis); code != http.StatusCreated || vis.CDPURL != "" {
		t.Fatalf("visual start: %d %+v", code, vis)
	}
	s = vis
	if code := a.do(http.MethodGet, "/cdp/"+name+"/json/version", "", nil); code != http.StatusConflict {
		t.Fatalf("CDP in Visual mode: %d", code)
	}
	if code := a.do(http.MethodPost, "/api/sessions/"+s.ID+"/click", `{"x": 640, "y": 400}`, nil); code != http.StatusNoContent {
		t.Fatalf("click: %d", code)
	}
	// A computer action can answer with the screen; a zoom crops it at full size.
	for body, want := range map[string][2]int{
		`{"action": "scroll", "x": 640, "y": 400, "dy": 2, "screenshot": true, "width": 640}`: {640, 400},
		`{"action": "key", "keys": "Escape", "screenshot": true}`:                             {1280, 800},
		`{"action": "zoom", "x": 1200, "y": 100, "w": 200, "h": 50}`:                          {80, 50},
	} {
		req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+s.ID+"/computer", strings.NewReader(body))
		req.Header = a.header()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || img.Bounds().Dx() != want[0] || img.Bounds().Dy() != want[1] {
			t.Fatalf("%s: %d %v", body, resp.StatusCode, err)
		}
	}
}

// TestDownloadsAndUploads: a download lands in the profile's files and is
// served by the API, in API mode and in Visual mode (where nothing
// attaches over CDP); an uploaded file can be picked by a page.
func TestDownloadsAndUploads(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewSite(t, browsertest.SiteHost(t))
	name := fmt.Sprintf("f%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "api", "screen": {"width": 1280, "height": 800}}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	cdpHTTP := strings.Replace(s.CDPURL, "ws://", "http://", 1)
	p := browsertest.PageVia(t, cdpHTTP+"/json/list", a.header())

	// Download, driven over CDP.
	_, _ = p.Try("Page.navigate", map[string]any{"url": site.URL + "/download"})
	waitForFile(t, a, name, "report.csv", "a,b\n1,2\n")

	// Upload, then pick it in a file input the way an agent would.
	req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+s.ID+"/files?name=hello.txt", strings.NewReader("hello from the agent"))
	req.Header = a.header()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var up struct{ Path string }
	_ = json.NewDecoder(resp.Body).Decode(&up)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || up.Path != "/files/uploads/hello.txt" {
		t.Fatalf("upload: %d %+v", resp.StatusCode, up)
	}
	p.Navigate(site.URL + "/upload")
	doc := p.Call("DOM.getDocument", nil)
	var root struct{ Root struct{ NodeID int } }
	_ = json.Unmarshal(doc, &root)
	q := p.Call("DOM.querySelector", map[string]any{"nodeId": root.Root.NodeID, "selector": "#file"})
	var node struct{ NodeID int }
	_ = json.Unmarshal(q, &node)
	p.Call("DOM.setFileInputFiles", map[string]any{"nodeId": node.NodeID, "files": []string{up.Path}})
	deadline := time.Now().Add(10 * time.Second)
	for p.Eval("document.getElementById('out').textContent") != "hello.txt:hello from the agent" {
		if time.Now().After(deadline) {
			t.Fatalf("the page read %v from the uploaded file", p.Eval("document.getElementById('out').textContent"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.Close()
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}

	// Visual mode: no CDP, so open the download by typing in the address bar.
	if code := a.do(http.MethodDelete, "/api/profiles/"+name+"/files/report.csv", "", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	var vis sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "mode": "visual"}`, name), &vis); code != http.StatusCreated {
		t.Fatalf("visual start: %d", code)
	}
	// The address bar sits about 63 px from the top of a 1280x800 window.
	if code := a.do(http.MethodPost, "/api/sessions/"+vis.ID+"/click", `{"x": 500, "y": 63}`, nil); code != http.StatusNoContent {
		t.Fatalf("click: %d", code)
	}
	if code := a.do(http.MethodPost, "/api/sessions/"+vis.ID+"/type", fmt.Sprintf(`{"text": %q}`, site.URL+"/download\n"), nil); code != http.StatusNoContent {
		t.Fatalf("type: %d", code)
	}
	waitForFile(t, a, name, "report.csv", "a,b\n1,2\n")
}

func waitForFile(t *testing.T, a agent, profile, name, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, a.base+"/api/profiles/"+profile+"/files/"+name, nil)
		req.Header = a.header()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			if string(body) != want {
				t.Fatalf("%s holds %q, want %q", name, body, want)
			}
			return
		}
		if time.Now().After(deadline) {
			var list any
			a.do(http.MethodGet, "/api/profiles/"+profile+"/files", "", &list)
			t.Fatalf("%s never appeared; files: %v", name, list)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestProxiedBrowserMatchesItsExit: with a proxy, everything goes through
// it by name, the time zone and languages match where it exits, WebRTC
// shows no address, and if the proxy goes away nothing goes out directly.
func TestProxiedBrowserMatchesItsExit(t *testing.T) {
	for kind, start := range map[string]func(*testing.T, string, string) *browsertest.Proxy{
		"http":   browsertest.NewConnectProxy,
		"socks5": browsertest.NewSOCKS5Proxy,
	} {
		t.Run(kind, func(t *testing.T) {
			a := newRealAPI(t)
			host := browsertest.SiteHost(t) // how the container reaches the proxy
			// The site has a name only the proxy can resolve: if pages load,
			// names went through the proxy unresolved.
			site := browsertest.NewSite(t, "browserpier.test")
			up := start(t, "pier", "s3cret:pw")
			up.Alias("browserpier.test", "127.0.0.1")
			a.m.SetGeoURL(site.URL + "/geo")

			name := fmt.Sprintf("p%d", time.Now().UnixNano()%1e9)
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
			p := profiles.New(name)
			p.DefaultMode, p.Width, p.Height = profiles.ModeAPI, 1280, 800
			p.Proxy = kind + "://pier:" + url.QueryEscape("s3cret:pw") + "@" + net.JoinHostPort(host, up.Port())
			if _, err := a.m.Profiles().Create(context.Background(), p); err != nil {
				t.Fatal(err)
			}

			var s sessionJSON
			if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
				t.Fatalf("start: %d %+v", code, s)
			}
			pg := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
			pg.Navigate(site.URL + "/account")

			if tz := pg.Eval("Intl.DateTimeFormat().resolvedOptions().timeZone"); tz != "Europe/Berlin" {
				t.Errorf("time zone %v, want Europe/Berlin", tz)
			}
			if lang := pg.Eval("navigator.language"); lang != "de-DE" {
				t.Errorf("navigator.language %v, want de-DE", lang)
			}
			if al := site.AcceptLanguage(); !strings.HasPrefix(al, "de-DE,de") {
				t.Errorf("Accept-Language %q, want de-DE first", al)
			}
			sitePort := strings.TrimPrefix(site.URL[strings.LastIndex(site.URL, ":"):], ":")
			found := false
			for _, target := range up.Targets() {
				if target == net.JoinHostPort("browserpier.test", sitePort) {
					found = true
				}
			}
			if !found {
				t.Errorf("the proxy never carried the site; it saw %v", up.Targets())
			}

			var prof profileJSON
			a.do(http.MethodGet, "/api/profiles", "", nil)
			var list struct{ Profiles []profileJSON }
			a.do(http.MethodGet, "/api/profiles", "", &list)
			for _, pr := range list.Profiles {
				if pr.Name == name {
					prof = pr
				}
			}
			if prof.Location == nil || prof.Location.Country != "DE" || prof.Location.TimeZone != "Europe/Berlin" {
				t.Errorf("recorded location %+v", prof.Location)
			}
			if strings.Contains(prof.Proxy, "s3cret") {
				t.Error("the API shows the proxy password")
			}

			// WebRTC gives away no address.
			cands := pg.Eval(`new Promise(resolve => {
				const pc = new RTCPeerConnection({iceServers: [{urls: 'stun:stun.l.google.com:19302'}]});
				const found = [];
				pc.onicecandidate = e => { if (e.candidate) found.push(e.candidate.candidate); else resolve(found); };
				pc.createDataChannel('x');
				pc.createOffer().then(o => pc.setLocalDescription(o));
				setTimeout(() => resolve(found), 5000);
			})`)
			for _, c := range cands.([]any) {
				f := strings.Fields(fmt.Sprint(c))
				if len(f) > 4 && net.ParseIP(f[4]) != nil {
					t.Errorf("WebRTC candidate with an address: %v", c)
				}
			}

			// The proxy goes away: the page fails; nothing reaches the site directly.
			up.Close()
			before := site.Requests()
			_, _ = pg.Try("Page.navigate", map[string]any{"url": site.URL + "/account?after"})
			time.Sleep(3 * time.Second)
			if site.Requests() != before {
				t.Error("with the proxy down, the browser reached the site anyway")
			}
		})
	}
}
