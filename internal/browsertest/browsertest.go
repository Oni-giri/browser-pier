// Package browsertest drives a browser container in tests: a CDP client
// over the control socket and a small site with a password login.
package browsertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ControlClient talks HTTP to a browser container's control socket; use
// "http://control/..." URLs.
func ControlClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// CDP is a connection to the browser's first page target.
type CDP struct {
	t    *testing.T
	conn *websocket.Conn
	mu   sync.Mutex
	id   int
}

// Page connects to the browser's first page target through the control
// socket's CDP relay.
func Page(t *testing.T, control *http.Client) *CDP {
	t.Helper()
	resp, err := control.Get("http://control/cdp/json/list")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var targets []struct{ Type, WebSocketDebuggerURL string }
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		t.Fatal(err)
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
		conn, _, err := websocket.Dial(ctx, "ws://control/cdp"+path, &websocket.DialOptions{HTTPClient: control})
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadLimit(64 << 20)
		c := &CDP{t: t, conn: conn}
		t.Cleanup(c.Close)
		return c
	}
	t.Fatalf("no page target in %+v", targets)
	return nil
}

// PageVia connects to the first page target listed at listURL (a
// /json/list endpoint, such as BrowserPier's /cdp/{profile}/json/list),
// following the WebSocket URL it gives, with header on both requests.
func PageVia(t *testing.T, listURL string, header http.Header) *CDP {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, listURL, nil)
	req.Header = header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %d %s", listURL, resp.StatusCode, b)
	}
	var targets []struct{ Type, WebSocketDebuggerURL string }
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		if tg.Type != "page" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		//nolint:bodyclose // after an upgrade the connection owns the body
		conn, _, err := websocket.Dial(ctx, tg.WebSocketDebuggerURL, &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadLimit(64 << 20)
		c := &CDP{t: t, conn: conn}
		t.Cleanup(c.Close)
		return c
	}
	t.Fatalf("no page target in %+v", targets)
	return nil
}

func (c *CDP) Close() { _ = c.conn.Close(websocket.StatusNormalClosure, "") }

func (c *CDP) Call(method string, params any) json.RawMessage {
	c.t.Helper()
	r, err := c.Try(method, params)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return r
}

// Try is Call that returns CDP errors, which are normal while a page navigates.
func (c *CDP) Try(method string, params any) (json.RawMessage, error) {
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

// Eval runs a JavaScript expression in the page over CDP and returns its value.
// It returns nil if the page is between documents.
func (c *CDP) Eval(expr string) any {
	c.t.Helper()
	raw, err := c.Try("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return nil
	}
	var r struct {
		Result struct{ Value any }
	}
	_ = json.Unmarshal(raw, &r)
	return r.Result.Value
}

func (c *CDP) Navigate(url string) {
	c.t.Helper()
	c.Call("Page.navigate", map[string]any{"url": url})
	deadline := time.Now().Add(30 * time.Second)
	for c.Eval("location.href") != url || c.Eval("document.readyState") != "complete" {
		if time.Now().After(deadline) {
			c.t.Fatalf("%s did not load; at %v", url, c.Eval("location.href"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Site is a test site with a password login that sets a persistent cookie,
// or a session-only one after SessionCookie.
type Site struct {
	URL string
	mu  sync.Mutex
	// sessionOnly makes logins set a cookie with no Expires or Max-Age.
	sessionOnly bool
	// issued is the session token from the last login, seen the cookie on the last /account visit.
	issued, seen string
	// requests counts every request; acceptLanguage is the last page
	// request's Accept-Language.
	requests       int
	acceptLanguage string
	// iconServed is set once the browser fetched /icon.png.
	iconServed bool
	// rejected is what the last refused login sent.
	rejected string
	// released lets /later's page go on (ReleaseLater); arrived is set
	// once it has.
	released, arrived bool
	// shown is every pageshow the /hello, /heavy and /back pages
	// reported; served counts the requests for each of them.
	shown  []Shown
	served map[string]int
}

// Shown is what a page saw in a pageshow event (PROJ-160): whether it came
// from the back/forward cache, its navigation's type, and Chromium's
// notRestoredReasons for it as JSON ("null" when Chromium gives none, and
// "absent" when the navigation entry has no such property at all). Load
// is random to each document: a page loaded again, rather than restored,
// reports a new one, even when its HTML came from the HTTP cache.
type Shown struct {
	Page      string
	Persisted bool
	Type      string
	Reasons   string
	Load      string
}

// Shows lists the pageshow events page reported, oldest first.
func (s *Site) Shows(page string) []Shown {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Shown
	for _, e := range s.shown {
		if e.Page == page {
			out = append(out, e)
		}
	}
	return out
}

// Served counts the requests for path, one of the pages that report
// pageshow (/hello, /heavy and those under /back/).
func (s *Site) Served(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served[path]
}

// WaitShown waits until page has reported n pageshow events, and returns
// the last.
func (s *Site) WaitShown(t *testing.T, page string, n int, timeout time.Duration) Shown {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if e := s.Shows(page); len(e) >= n {
			return e[len(e)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s reported %d pageshow events in %v, want %d", page, len(s.Shows(page)), timeout, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// reportShown is a script that tells the site of each pageshow, as Shown.
// It holds no connection open and registers no unload handler, so it
// leaves its page eligible for the back/forward cache.
const reportShown = `<script>
const load = Math.random().toString(36).slice(2);
addEventListener('pageshow', e => {
  const n = performance.getEntriesByType('navigation')[0] || {};
  fetch('/shown?' + new URLSearchParams({page: PAGE, persisted: e.persisted, type: n.type || '',
    reasons: 'notRestoredReasons' in n ? JSON.stringify(n.notRestoredReasons) : 'absent', load}));
});
</script>`

// MaxHeavyMB is the most /heavy holds, whatever its ?mb= asks: a mistyped
// measurement setting can't take the shared host's memory.
const MaxHeavyMB = 1024

// heavyPage holds mb MB in typed arrays, filled so the memory is
// committed, and a 20,000-paragraph DOM: a page heavy enough that keeping
// it in the back/forward cache shows in the container's memory.
var heavyPage = `<title>Heavy</title><h1>Heavy</h1><div id="d"></div><script>
const mb = Math.min(Number(new URLSearchParams(location.search).get('mb')) || 200, ` + strconv.Itoa(MaxHeavyMB) + `);
window.held = [];
for (let i = 0; i < mb; i++) { const a = new Float64Array(131072); a.fill(i + Math.random()); held.push(a); }
const f = document.createDocumentFragment();
for (let i = 0; i < 20000; i++) { const p = document.createElement('p'); p.textContent = 'Row ' + i + ' ' + 'lorem ipsum '.repeat(4); f.append(p); }
document.getElementById('d').append(f);
</script>`

// ReleaseLater sends the /later page, wherever it is, on to
// /account-later, which its tab can be told to match without being the
// account page.
func (s *Site) ReleaseLater() {
	s.mu.Lock()
	s.released = true
	s.mu.Unlock()
}

// LaterArrived reports whether the /later page got to /account-later.
func (s *Site) LaterArrived() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arrived
}

// SessionCookie makes later logins set a session-only cookie, with no
// Expires or Max-Age, as sites do when "remember me" is left unticked.
func (s *Site) SessionCookie() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionOnly = true
}

// IconServed reports whether the browser has fetched the /icon page's
// favicon.
func (s *Site) IconServed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.iconServed
}

// Icon is the favicon of the site's /icon page: a 32×32 PNG in one colour.
func Icon() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0x2f, 0x6b, 0xff, 0xff
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// Requests counts the requests the site has served.
func (s *Site) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// AcceptLanguage is the Accept-Language of the last page request.
func (s *Site) AcceptLanguage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptLanguage
}

// SiteHost is how a browser container reaches a test site on this machine:
// BROWSERPIER_TEST_SITE_HOST, or else the gateway of Docker's default
// bridge. Under rootless Podman 5, host.docker.internal is link-local,
// which the egress filter always refuses; CONTRIBUTING.md has the fix.
func SiteHost(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("BROWSERPIER_TEST_SITE_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("can't find the bridge gateway (%v); set BROWSERPIER_TEST_SITE_HOST", err)
	}
	return strings.TrimSpace(string(out))
}

// NewSite serves the site on every interface; host is how the browser
// container reaches this machine, from SiteHost.
func NewSite(t *testing.T, host string) *Site {
	t.Helper()
	s := &Site{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		// Each field is a 200 px band across the page in a colour of its
		// own, so a test can see on the screen where to click, whatever the
		// height of the browser's toolbar above.
		_, _ = io.WriteString(w, `<style>body{margin:0}input{display:block;box-sizing:border-box;width:100%;height:200px;margin:0;border:0}
[name=user]{background:#2f6bff}[name=password]{background:#16a34a}</style>
<form method="post" action="/login"><input name="user"><input name="password" type="password"><button>Sign in</button></form>`)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("user") != "pier" || r.FormValue("password") != "hunter2" {
			s.mu.Lock()
			s.rejected = fmt.Sprintf("user %q, password %q", r.FormValue("user"), r.FormValue("password"))
			s.mu.Unlock()
			http.Error(w, "wrong password", http.StatusUnauthorized)
			return
		}
		token := fmt.Sprintf("tok-%d", time.Now().UnixNano())
		s.mu.Lock()
		s.issued = token
		sessionOnly := s.sessionOnly
		s.mu.Unlock()
		//nolint:gosec // plain-HTTP test site; a Secure cookie wouldn't be sent back
		c := &http.Cookie{Name: "session", Value: token, Path: "/", MaxAge: 86400, HttpOnly: true}
		if sessionOnly {
			c.MaxAge = 0
		}
		http.SetCookie(w, c)
		_, _ = io.WriteString(w, "<p>Signed in</p>")
	})
	// A page that navigates by itself once the test says so (ReleaseLater),
	// as a background tab or popup might.
	mux.HandleFunc("GET /later", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<title>Later</title><p>Waiting</p><script>
setInterval(async () => { if ((await fetch('/later/go')).status === 200) location = '/account-later' }, 250)</script>`)
	})
	mux.HandleFunc("GET /later/go", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		released := s.released
		s.mu.Unlock()
		if !released {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /account-later", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.arrived = true
		s.mu.Unlock()
		_, _ = io.WriteString(w, "<title>Moved</title><p>Moved by itself</p>")
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
	// A page with a favicon of its own, so others keep asking for the
	// missing /favicon.ico as before.
	mux.HandleFunc("GET /icon", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<link rel="icon" href="/icon.png" type="image/png"><h1>A page with an icon</h1>`)
	})
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(Icon())
		s.mu.Lock()
		s.iconServed = true
		s.mu.Unlock()
	})
	// Pages that report each pageshow to /shown (PROJ-160): two light
	// ones for Back, a plain one to leave to, and a heavy one (?mb=N).
	// /back/unload has an unload handler, which keeps the back/forward
	// cache from restoring it; /back/no-store is served with
	// Cache-Control: no-store, which Chromium 156 restores all the same.
	for path, body := range map[string]string{
		"/hello":         "<title>Hello</title><h1>Hello</h1>",
		"/back/a":        "<title>A</title><h1>Page A</h1>",
		"/back/b":        "<title>B</title><h1>Page B</h1>",
		"/back/unload":   "<title>Unload</title><h1>A page with an unload handler</h1><script>addEventListener('unload', () => {})</script>",
		"/back/no-store": "<title>No store</title><h1>A page served with no-store</h1>",
		"/heavy":         heavyPage,
	} {
		script := strings.Replace(reportShown, "PAGE", fmt.Sprintf("%q", path), 1)
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			s.mu.Lock()
			if s.served == nil {
				s.served = map[string]int{}
			}
			s.served[path]++
			s.mu.Unlock()
			if path == "/back/no-store" {
				w.Header().Set("Cache-Control", "no-store")
			}
			_, _ = io.WriteString(w, body+script)
		})
	}
	mux.HandleFunc("GET /shown", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		s.shown = append(s.shown, Shown{Page: q.Get("page"), Persisted: q.Get("persisted") == "true", Type: q.Get("type"), Reasons: q.Get("reasons"), Load: q.Get("load")})
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /geo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ip":"198.51.100.23","country_iso":"DE","time_zone":"Europe/Berlin"}`)
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
		w.Header().Set("Content-Type", "text/csv")
		_, _ = io.WriteString(w, "a,b\n1,2\n")
	})
	mux.HandleFunc("GET /upload", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<input type="file" id="file"><pre id="out"></pre><script>
document.getElementById('file').addEventListener('change', async e => {
  const f = e.target.files[0];
  document.getElementById('out').textContent = f.name + ':' + await f.text();
});</script>`)
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("site: %s %s", r.Method, r.URL.Path)
		s.mu.Lock()
		s.requests++
		if r.URL.Path != "/geo" {
			s.acceptLanguage = r.Header.Get("Accept-Language")
		}
		s.mu.Unlock()
		// Content sniffing doesn't take a page starting with <form> as HTML.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: logged, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.URL = fmt.Sprintf("http://%s:%d", host, ln.Addr().(*net.TCPAddr).Port)
	return s
}

func (s *Site) state() (issued, seen string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued, s.seen
}

// Issued is the session token the last good login was given; Seen is the
// one /account was last visited with ("" for none).
func (s *Site) Issued() string { issued, _ := s.state(); return issued }
func (s *Site) Seen() string   { _, seen := s.state(); return seen }

// Rejected is what the last refused login sent ("" for none), to tell a
// form never submitted from one filled in wrong.
func (s *Site) Rejected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}

// Login signs in through the page's form and returns once the cookie is set.
func (s *Site) Login(t *testing.T, p *CDP) string {
	t.Helper()
	before, _ := s.state()
	p.Navigate(s.URL + "/login")
	p.Eval(`document.querySelector('[name=user]').value = 'pier';
		document.querySelector('[name=password]').value = 'hunter2';
		setTimeout(() => document.forms[0].submit(), 0)`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if issued, _ := s.state(); issued != before {
			// The response carrying the cookie must have reached the browser.
			for p.Eval("document.body && document.body.innerText") != "Signed in" {
				if time.Now().After(deadline) {
					t.Fatal("no signed-in page after login")
				}
				time.Sleep(50 * time.Millisecond)
			}
			return issued
		}
		if time.Now().After(deadline) {
			t.Fatalf("login form was not submitted; page at %v says %q", p.Eval("location.href"), p.Eval("document.body && document.body.innerText"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Account visits /account and returns the session cookie the site received.
func (s *Site) Account(t *testing.T, p *CDP) string {
	t.Helper()
	p.Navigate(s.URL + "/account")
	_, seen := s.state()
	return seen
}

// OtherAddress is another name for the test site's host, as a different
// site: what host resolves to in the browser's container when it's a name
// (host.docker.internal under rootless Podman), or else, since an address
// has no name to resolve (getent would fail on Docker's bridge gateway,
// CI's default), another IPv4 address of this machine that the container
// reaches the site at (the test site listens on every interface). Not
// every one does: rootless Podman's network namespace holds a copy of the
// host's LAN address, so the container can't reach the host there.
// container is the browser container's name.
func OtherAddress(t *testing.T, container, host, port string) string {
	t.Helper()
	if net.ParseIP(host) == nil {
		out, err := exec.Command("docker", "exec", container, "getent", "hosts", host).Output() //nolint:gosec // a test's own container and host
		if err != nil || len(strings.Fields(string(out))) == 0 {
			t.Fatalf("can't resolve %s in the container: %v %q", host, err, out)
		}
		return strings.Fields(string(out))[0]
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.To4() == nil || !n.IP.IsGlobalUnicast() || n.IP.Equal(net.ParseIP(host)) || n.Contains(net.ParseIP(host)) {
			continue
		}
		probe := exec.Command("docker", "exec", container, "timeout", "3", "bash", "-c", "exec 3<>/dev/tcp/"+n.IP.String()+"/"+port) //nolint:gosec // a test's own container
		if probe.Run() != nil {
			t.Logf("the container can't reach %s; trying another address", n.IP)
			continue
		}
		return n.IP.String()
	}
	t.Fatalf("no address of this machine but %s that the browser's container reaches; set BROWSERPIER_TEST_SITE_HOST to a name", host)
	return ""
}
