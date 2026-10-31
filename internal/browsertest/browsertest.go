// Package browsertest drives a browser container in tests: a CDP client
// over the control socket and a small site with a password login.
package browsertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

// Site is a test site with a password login that sets a persistent cookie.
type Site struct {
	URL string
	mu  sync.Mutex
	// issued is the session token from the last login, seen the cookie on the last /account visit.
	issued, seen string
}

// NewSite serves the site on every interface; host is how the browser
// container reaches this machine, such as host.docker.internal.
func NewSite(t *testing.T, host string) *Site {
	t.Helper()
	s := &Site{}
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
		//nolint:gosec // plain-HTTP test site; a Secure cookie wouldn't be sent back
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
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
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
	s.URL = fmt.Sprintf("http://%s:%d", host, ln.Addr().(*net.TCPAddr).Port)
	return s
}

func (s *Site) state() (issued, seen string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued, s.seen
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
