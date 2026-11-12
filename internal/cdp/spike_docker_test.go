//go:build docker

// PROJ-21's phase 0: what the page tools' CDP use can and can't do on the
// pinned Chromium, measured with nothing but the tools' own allow-list.
// Run it with BROWSERPIER_SPIKE=1 (it takes a few minutes); the results
// are in images/chromium/ANTI-BOT.md.
package cdp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// spikeList is the design's allow-list (PROJ-21 §1), nothing more.
var spikeList = cdp.NewAllowList("spike",
	"Target.getTargets", "Target.attachToTarget", "Target.detachFromTarget", "Target.activateTarget", "Target.createTarget", "Target.closeTarget",
	"Page.getFrameTree", "Page.createIsolatedWorld", "Page.navigate", "Page.navigateToHistoryEntry", "Page.getNavigationHistory", "Page.reload", "Page.handleJavaScriptDialog",
	"Runtime.evaluate", "Runtime.callFunctionOn", "Runtime.releaseObject",
	"DOM.resolveNode", "DOM.describeNode", "DOM.getContentQuads", "DOM.scrollIntoViewIfNeeded", "DOM.focus",
	"Accessibility.getFullAXTree", "Accessibility.getPartialAXTree", "DOMSnapshot.captureSnapshot",
	"Input.dispatchMouseEvent", "Input.dispatchKeyEvent", "Input.insertText")

type spike struct {
	t       *testing.T
	sock    string
	name    string
	control *http.Client
	site    string
	out     strings.Builder
}

func (s *spike) logf(format string, args ...any) {
	s.t.Helper()
	line := fmt.Sprintf(format, args...)
	s.t.Log(line)
	s.out.WriteString(line + "\n")
}

func startSpike(t *testing.T) *spike {
	t.Helper()
	if os.Getenv("BROWSERPIER_SPIKE") == "" {
		t.Skip("set BROWSERPIER_SPIKE=1 to run PROJ-21's spike")
	}
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set")
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
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, slog.New(slog.DiscardHandler))
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
	name := fmt.Sprintf("s%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.LocalNetwork = true
	p.DefaultMode = profiles.ModeAPI
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Start(ctx, name, profiles.ModeAPI); err != nil {
		t.Fatal(err)
	}
	sock := m.Layout().ControlSocket(name)
	s := &spike{t: t, sock: sock, name: name, control: browsertest.ControlClient(sock)}
	s.site = spikeSite(t, siteHost(t))
	t.Cleanup(func() {
		if f := os.Getenv("BROWSERPIER_SPIKE_OUT"); f != "" {
			_ = os.WriteFile(f, []byte(s.out.String()), 0o600)
		}
	})
	return s
}

func siteHost(t *testing.T) string {
	if h := os.Getenv("BROWSERPIER_TEST_SITE_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		t.Fatalf("set BROWSERPIER_TEST_SITE_HOST: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func spikeSite(t *testing.T, host string) string {
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, body)
		}
	}
	mux.HandleFunc("GET /blank", page(`<!doctype html><title>Blank</title><p>blank</p>`))
	mux.HandleFunc("GET /form", page(`<!doctype html><title>Form</title>
<label>Email <input id="email" name="email"></label>
<label>Password <input id="pw" name="password" type="password"></label>
<button id="go">Sign in</button>
<script>document.addEventListener('keydown', e => { document.body.dataset.keys = (document.body.dataset.keys || '') + e.key; }, true);</script>`))
	mux.HandleFunc("GET /alert", page(`<!doctype html><title>Alert</title><p>An alert opens in 1.5 s.</p>
<script>setTimeout(() => alert('Hello from the page'), 1500)</script>`))
	mux.HandleFunc("GET /popup", page(`<!doctype html><title>Opener</title>
<style>button{position:fixed;inset:0;width:100%;height:100%;font-size:40px}</style>
<button onclick="window.open(location.origin + '/blank?popup', '_blank', 'popup,width=500,height=400')">Open a popup</button>`))
	mux.HandleFunc("GET /newtab", page(`<!doctype html><title>Opener</title>
<style>a{position:fixed;inset:0;display:block;font-size:40px}</style>
<a href="/blank?tab" target="_blank">Open a tab</a>`))
	mux.HandleFunc("GET /big", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Big</title><nav>")
		for i := range n {
			_, _ = fmt.Fprintf(w, `<div><a href="#i%d">Link %d</a> <button>Button %d</button><p>Paragraph %d with some words to read, lorem ipsum dolor sit amet.</p></div>`, i, i, i, i)
		}
		_, _ = io.WriteString(w, "</nav>")
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return fmt.Sprintf("http://%s:%d", host, ln.Addr().(*net.TCPAddr).Port)
}

func (s *spike) dial() *cdp.Conn {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cdp.Dial(ctx, s.sock, spikeList, cdp.Options{MaxMessage: 256 << 20})
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Attached bool   `json:"attached"`
	OpenerID string `json:"openerId"`
}

func targets(t *testing.T, c *cdp.Conn) []targetInfo {
	t.Helper()
	var r struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := c.Call(ctx(5), "", "Target.getTargets", nil, &r); err != nil {
		t.Fatal(err)
	}
	var pages []targetInfo
	for _, ti := range r.TargetInfos {
		if ti.Type == "page" {
			pages = append(pages, ti)
		}
	}
	return pages
}

func ctx(seconds int) context.Context {
	c, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	_ = cancel // the timer ends it
	return c
}

// tab is a page with a session and its current isolated world.
type tab struct {
	t *testing.T
	*cdp.Target
}

func (tb tab) frame() (id, loader, url string) {
	tb.t.Helper()
	var r struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := tb.Call(ctx(5), "Page.getFrameTree", nil, &r); err != nil {
		tb.t.Fatal(err)
	}
	return r.FrameTree.Frame.ID, r.FrameTree.Frame.LoaderID, r.FrameTree.Frame.URL
}

func (tb tab) world() int64 {
	tb.t.Helper()
	f, _, _ := tb.frame()
	w, err := tb.IsolatedWorld(ctx(5), f)
	if err != nil {
		tb.t.Fatal(err)
	}
	return w
}

func (tb tab) eval(expr string) any {
	tb.t.Helper()
	var v any
	if err := tb.Eval(ctx(5), tb.world(), expr, &v); err != nil {
		tb.t.Fatalf("%s: %v", expr, err)
	}
	return v
}

func (tb tab) navigate(url string) {
	tb.t.Helper()
	if err := tb.Call(ctx(10), "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		tb.t.Fatal(err)
	}
	tb.waitLoad(url)
}

func (tb tab) waitLoad(url string) {
	tb.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, _, at := tb.frame()
		if at == url {
			var state string
			f, _, _ := tb.frame()
			if w, err := tb.IsolatedWorld(ctx(5), f); err == nil && tb.Eval(ctx(5), w, "document.readyState", &state) == nil && state == "complete" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.t.Fatalf("%s didn't load", url)
}

// node finds an element's backend node ID through the isolated world.
func (tb tab) node(selector string) int64 {
	tb.t.Helper()
	var r struct{ Result cdp.RemoteObject }
	if err := tb.Call(ctx(5), "Runtime.evaluate", map[string]any{"expression": "document.querySelector(" + strconv.Quote(selector) + ")", "contextId": tb.world()}, &r); err != nil || r.Result.ObjectID == "" {
		tb.t.Fatalf("%s: %v", selector, err)
	}
	var d struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := tb.Call(ctx(5), "DOM.describeNode", map[string]any{"objectId": r.Result.ObjectID}, &d); err != nil {
		tb.t.Fatal(err)
	}
	return d.Node.BackendNodeID
}

func attach(t *testing.T, c *cdp.Conn, id string) tab {
	t.Helper()
	tg, err := c.Attach(ctx(5), id)
	if err != nil {
		t.Fatal(err)
	}
	return tab{t, tg}
}

func (s *spike) firstTab(c *cdp.Conn) tab {
	s.t.Helper()
	pages := targets(s.t, c)
	if len(pages) == 0 {
		s.t.Fatal("no page")
	}
	return attach(s.t, c, pages[0].TargetID)
}

func try(s *spike, what string, f func() error) error {
	start := time.Now()
	err := f()
	res := "works"
	if err != nil {
		res = "FAILS: " + err.Error()
	}
	s.logf("  %-36s %s (%s)", what, res, time.Since(start).Round(time.Millisecond))
	return err
}

func TestSpike(t *testing.T) {
	s := startSpike(t)
	s.logf("# PROJ-21 spike, Chromium in %s", os.Getenv("BROWSERPIER_TEST_IMAGE"))
	only := os.Getenv("BROWSERPIER_SPIKE_ONLY")
	for _, part := range []struct {
		name string
		run  func()
	}{
		{"methods", s.methodsWithoutEnable}, {"typing", s.typingAfterNavigate}, {"focus", s.firstKey},
		{"dialogs", s.dialogs}, {"tabs", s.foregroundTab}, {"large", s.largePages},
		{"worlds", s.isolatedWorlds}, {"detector", s.detector},
	} {
		if only == "" || strings.Contains(","+only+",", ","+part.name+",") {
			part.run()
		}
	}
}

func (s *spike) methodsWithoutEnable() {
	s.logf("## Methods without any *.enable")
	c := s.dial()
	defer func() { _ = c.Close() }()
	tb := s.firstTab(c)
	tb.navigate(s.site + "/form")
	world := tb.world()
	email := tb.node("#email")
	_ = try(s, "Accessibility.getFullAXTree", func() error {
		var r struct {
			Nodes []struct {
				Ignored          bool  `json:"ignored"`
				BackendDOMNodeID int64 `json:"backendDOMNodeId"`
				Name             *struct {
					Value any `json:"value"`
				} `json:"name"`
			} `json:"nodes"`
		}
		if err := tb.Call(ctx(10), "Accessibility.getFullAXTree", nil, &r); err != nil {
			return err
		}
		for _, n := range r.Nodes {
			if n.BackendDOMNodeID == email && n.Name != nil {
				s.logf("    the email field's AX name: %v", n.Name.Value)
			}
		}
		s.logf("    %d AX nodes", len(r.Nodes))
		return nil
	})
	_ = try(s, "DOMSnapshot.captureSnapshot", func() error {
		var r struct {
			Documents []struct {
				Nodes struct {
					BackendNodeID []int64 `json:"backendNodeId"`
				} `json:"nodes"`
				Layout struct {
					NodeIndex []int `json:"nodeIndex"`
				} `json:"layout"`
			} `json:"documents"`
			Strings []string `json:"strings"`
		}
		if err := tb.Call(ctx(10), "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": []string{"display", "visibility", "opacity"}, "includeDOMRects": true}, &r); err != nil {
			return err
		}
		s.logf("    %d documents, %d nodes, %d laid out", len(r.Documents), len(r.Documents[0].Nodes.BackendNodeID), len(r.Documents[0].Layout.NodeIndex))
		return nil
	})
	_ = try(s, "Accessibility.getPartialAXTree (one node)", func() error {
		var r struct {
			Nodes []struct {
				Role struct{ Value any } `json:"role"`
				Name struct{ Value any } `json:"name"`
			} `json:"nodes"`
		}
		err := tb.Call(ctx(5), "Accessibility.getPartialAXTree", map[string]any{"backendNodeId": email, "fetchRelatives": false}, &r)
		if err == nil && len(r.Nodes) > 0 {
			s.logf("    %d nodes; role %v, name %v", len(r.Nodes), r.Nodes[0].Role.Value, r.Nodes[0].Name.Value)
		}
		return err
	})
	_ = try(s, "DOM.describeNode (backendNodeId)", func() error {
		return tb.Call(ctx(5), "DOM.describeNode", map[string]any{"backendNodeId": email}, nil)
	})
	var obj string
	_ = try(s, "DOM.resolveNode (isolated world)", func() error {
		var r struct{ Object cdp.RemoteObject }
		err := tb.Call(ctx(5), "DOM.resolveNode", map[string]any{"backendNodeId": email, "executionContextId": world}, &r)
		obj = r.Object.ObjectID
		return err
	})
	_ = try(s, "Runtime.callFunctionOn (that object)", func() error {
		var r struct{ Result cdp.RemoteObject }
		err := tb.Call(ctx(5), "Runtime.callFunctionOn", map[string]any{"objectId": obj, "functionDeclaration": "function() { return this.id; }", "returnByValue": true}, &r)
		if err == nil && string(r.Result.Value) != `"email"` {
			err = fmt.Errorf("got %s", r.Result.Value)
		}
		return err
	})
	_ = try(s, "Runtime.releaseObject", func() error {
		return tb.Call(ctx(5), "Runtime.releaseObject", map[string]any{"objectId": obj}, nil)
	})
	_ = try(s, "DOM.getContentQuads", func() error {
		var r struct {
			Quads [][]float64 `json:"quads"`
		}
		err := tb.Call(ctx(5), "DOM.getContentQuads", map[string]any{"backendNodeId": email}, &r)
		if err == nil {
			s.logf("    quads: %v", r.Quads)
		}
		return err
	})
	_ = try(s, "DOM.scrollIntoViewIfNeeded", func() error {
		return tb.Call(ctx(5), "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": email}, nil)
	})
	_ = try(s, "DOM.focus", func() error {
		if err := tb.Call(ctx(5), "DOM.focus", map[string]any{"backendNodeId": email}, nil); err != nil {
			return err
		}
		if id := tb.eval("document.activeElement && document.activeElement.id"); id != "email" {
			return fmt.Errorf("activeElement is %v", id)
		}
		return nil
	})
	_ = try(s, "Page.createIsolatedWorld", func() error {
		_, err := tb.IsolatedWorld(ctx(5), func() string { f, _, _ := tb.frame(); return f }())
		return err
	})
	_ = try(s, "Page.getNavigationHistory", func() error {
		return tb.Call(ctx(5), "Page.getNavigationHistory", nil, nil)
	})
	_ = try(s, "Page.handleJavaScriptDialog (none open)", func() error {
		return tb.Call(ctx(5), "Page.handleJavaScriptDialog", map[string]any{"accept": true}, nil)
	})
	// Whether anything stays attached once the connection closes.
	_ = c.Close()
	c2 := s.dial()
	defer func() { _ = c2.Close() }()
	for _, p := range targets(s.t, c2) {
		s.logf("  after closing: %s attached=%t", p.URL, p.Attached)
	}
}

func (s *spike) typingAfterNavigate() {
	s.logf("## Typing right after Page.navigate (focus in the address bar)")
	c := s.dial()
	defer func() { _ = c.Close() }()
	tb := s.firstTab(c)
	// insertText first, on a fresh navigation.
	tb.navigate(s.site + "/form?1")
	s.logf("  document.hasFocus() after navigate: %v", tb.eval("document.hasFocus()"))
	pw := tb.node("#pw")
	if err := tb.Call(ctx(5), "DOM.focus", map[string]any{"backendNodeId": pw}, nil); err != nil {
		s.t.Fatal(err)
	}
	s.logf("  after DOM.focus: hasFocus %v, activeElement %v", tb.eval("document.hasFocus()"), tb.eval("document.activeElement.id"))
	if err := tb.Call(ctx(5), "Input.insertText", map[string]any{"text": "hunter2"}, nil); err != nil {
		s.t.Fatal(err)
	}
	s.logf("  Input.insertText into #pw: value length %v (want 7)", tb.eval("document.querySelector('#pw').value.length"))
	// Then key events, on another.
	tb.navigate(s.site + "/form?2")
	email := tb.node("#email")
	_ = tb.Call(ctx(5), "DOM.focus", map[string]any{"backendNodeId": email}, nil)
	for _, ch := range "abc" {
		k := string(ch)
		_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": k, "code": "Key" + strings.ToUpper(k), "text": k, "windowsVirtualKeyCode": int(ch) - 32}, nil)
		_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k, "code": "Key" + strings.ToUpper(k), "windowsVirtualKeyCode": int(ch) - 32}, nil)
	}
	s.logf("  Input.dispatchKeyEvent a, b, c into #email: value %q, page saw keys %q", tb.eval("document.querySelector('#email').value"), tb.eval("document.body.dataset.keys || ''"))
	// selectAll then insertText: fill's replace.
	_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65, "modifiers": 2, "commands": []string{"selectAll"}}, nil)
	_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65, "modifiers": 2}, nil)
	_ = tb.Call(ctx(5), "Input.insertText", map[string]any{"text": "ana@example.com"}, nil)
	s.logf("  selectAll command + insertText: value %q (want ana@example.com)", tb.eval("document.querySelector('#email').value"))
	// Keys with nothing focused by us.
	tb.navigate(s.site + "/form?3")
	_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "k", "code": "KeyK", "text": "k", "windowsVirtualKeyCode": 75}, nil)
	_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "k", "code": "KeyK", "windowsVirtualKeyCode": 75}, nil)
	s.logf("  a key with no DOM.focus first: the page saw %q", tb.eval("document.body.dataset.keys || ''"))
}

// firstKey looks at the first key event after a navigation in detail.
func (s *spike) firstKey() {
	s.logf("## The first key event after a navigation")
	c := s.dial()
	defer func() { _ = c.Close() }()
	key := func(tb tab, typ, k string) {
		p := map[string]any{"type": typ, "key": k, "code": "Key" + strings.ToUpper(k), "windowsVirtualKeyCode": int(k[0]) - 32}
		if typ == "keyDown" {
			p["text"] = k
		}
		_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", p, nil)
	}
	typeABC := func(tb tab) string {
		for _, k := range []string{"a", "b", "c"} {
			key(tb, "keyDown", k)
			key(tb, "keyUp", k)
		}
		v, _ := tb.eval("document.querySelector('#email').value").(string)
		return v
	}
	focus := func(tb tab) {
		_ = tb.Call(ctx(5), "DOM.focus", map[string]any{"backendNodeId": tb.node("#email")}, nil)
	}
	for i := range 3 {
		var r struct {
			TargetID string `json:"targetId"`
		}
		_ = c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": s.site + "/form?new" + strconv.Itoa(i)}, &r)
		tb := attach(s.t, c, r.TargetID)
		tb.waitLoad(s.site + "/form?new" + strconv.Itoa(i))
		focus(tb)
		s.logf("  new tab %d: hasFocus %v; typed abc: %q", i, tb.eval("document.hasFocus()"), typeABC(tb))
		tb.navigate(s.site + "/form?again" + strconv.Itoa(i))
		focus(tb)
		s.logf("  same tab, navigated again: hasFocus %v; typed abc: %q", tb.eval("document.hasFocus()"), typeABC(tb))
		_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": r.TargetID}, nil)
	}
	// As a keyboard sends them: rawKeyDown, char, keyUp.
	typeRaw := func(tb tab) string {
		for _, k := range []string{"a", "b", "c"} {
			code, vk := "Key"+strings.ToUpper(k), int(k[0])-32
			_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": k, "code": code, "windowsVirtualKeyCode": vk}, nil)
			_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "char", "key": k, "code": code, "text": k, "unmodifiedText": k, "windowsVirtualKeyCode": vk}, nil)
			_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k, "code": code, "windowsVirtualKeyCode": vk}, nil)
		}
		v, _ := tb.eval("document.querySelector('#email').value").(string)
		return v
	}
	for i := range 3 {
		var r struct {
			TargetID string `json:"targetId"`
		}
		_ = c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": s.site + "/form?raw" + strconv.Itoa(i)}, &r)
		tb := attach(s.t, c, r.TargetID)
		tb.waitLoad(s.site + "/form?raw" + strconv.Itoa(i))
		focus(tb)
		s.logf("  rawKeyDown+char+keyUp, new tab %d: typed abc: %q", i, typeRaw(tb))
		tb.navigate(s.site + "/form?rawagain" + strconv.Itoa(i))
		focus(tb)
		s.logf("  rawKeyDown+char+keyUp, same tab navigated again: typed abc: %q; the page saw keys %q", typeRaw(tb), tb.eval("document.body.dataset.keys"))
		_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": r.TargetID}, nil)
	}
	// With a modifier press first, then waiting for focus.
	var r struct {
		TargetID string `json:"targetId"`
	}
	_ = c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": s.site + "/form?shift"}, &r)
	tb := attach(s.t, c, r.TargetID)
	tb.waitLoad(s.site + "/form?shift")
	focus(tb)
	start := time.Now()
	_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16}, nil)
	_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16}, nil)
	for tb.eval("document.hasFocus()") != true && time.Since(start) < 2*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	s.logf("  new tab, a Shift press first: hasFocus after %s; typed abc: %q; the page saw keys %q", time.Since(start).Round(time.Millisecond), typeABC(tb), tb.eval("document.body.dataset.keys"))
	_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": r.TargetID}, nil)
	// A mouse click on the field instead of DOM.focus.
	_ = c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": s.site + "/form?click"}, &r)
	tb = attach(s.t, c, r.TargetID)
	tb.waitLoad(s.site + "/form?click")
	s.click(tb, "#email")
	s.logf("  new tab, a click on the field: hasFocus %v; typed abc: %q", tb.eval("document.hasFocus()"), typeABC(tb))
	_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": r.TargetID}, nil)
	// insertText alone, then a key.
	_ = c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": s.site + "/form?insert"}, &r)
	tb = attach(s.t, c, r.TargetID)
	tb.waitLoad(s.site + "/form?insert")
	focus(tb)
	_ = tb.Call(ctx(5), "Input.insertText", map[string]any{"text": "xy"}, nil)
	s.logf("  new tab, insertText xy: hasFocus %v; value %q; then typed abc: %q", tb.eval("document.hasFocus()"), tb.eval("document.querySelector('#email').value"), typeABC(tb))
	_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": r.TargetID}, nil)
}

func (s *spike) dialogs() {
	s.logf("## Dialogs")
	c := s.dial()
	defer func() { _ = c.Close() }()
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": s.site + "/alert"}, &created); err != nil {
		s.t.Fatal(err)
	}
	defer func() {
		_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": created.TargetID}, nil)
	}()
	tb := attach(s.t, c, created.TargetID)
	tb.waitLoad(s.site + "/alert")
	world := tb.world()
	frame, _, _ := tb.frame()
	time.Sleep(2500 * time.Millisecond) // the alert is open now
	for _, ti := range targets(s.t, c) {
		if ti.TargetID == created.TargetID {
			s.logf("  Target.getTargets with the alert open: %+v", ti)
		}
	}
	resp, err := s.control.Get("http://control/cdp/json/list")
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		s.logf("  /json/list mentions the dialog: %t", strings.Contains(string(b), "Hello from the page"))
	}
	probe := func(what string, f func(context.Context) error) {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		err := f(c)
		s.logf("  with the alert open, %-30s %v (%s)", what, errOK(err), time.Since(start).Round(time.Millisecond))
	}
	probe("Page.getFrameTree", func(c context.Context) error { return tb.Call(c, "Page.getFrameTree", nil, nil) })
	probe("Page.getNavigationHistory", func(c context.Context) error { return tb.Call(c, "Page.getNavigationHistory", nil, nil) })
	probe("Runtime.evaluate", func(c context.Context) error { var v any; return tb.Eval(c, world, "1+1", &v) })
	probe("Page.createIsolatedWorld", func(c context.Context) error { _, err := tb.IsolatedWorld(c, frame); return err })
	probe("Accessibility.getFullAXTree", func(c context.Context) error { return tb.Call(c, "Accessibility.getFullAXTree", nil, nil) })
	probe("DOMSnapshot.captureSnapshot", func(c context.Context) error {
		return tb.Call(c, "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": []string{}}, nil)
	})
	probe("Page.handleJavaScriptDialog", func(c context.Context) error {
		return tb.Call(c, "Page.handleJavaScriptDialog", map[string]any{"accept": true}, nil)
	})
	probe("Runtime.evaluate again", func(c context.Context) error { var v any; return tb.Eval(c, world, "1+1", &v) })
}

func errOK(err error) string {
	if err == nil {
		return "answers"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "NO ANSWER (timed out)"
	}
	return "error: " + err.Error()
}

func (s *spike) visibility(c *cdp.Conn) (map[string]string, time.Duration) {
	start := time.Now()
	out := map[string]string{}
	for _, p := range targets(s.t, c) {
		tb := attach(s.t, c, p.TargetID)
		v, _ := tb.eval("document.visibilityState + (document.hasFocus() ? ' focused' : '')").(string)
		out[p.TargetID] = v
		tb.Detach()
	}
	return out, time.Since(start)
}

func (s *spike) foregroundTab() {
	s.logf("## The foreground tab")
	c := s.dial()
	defer func() { _ = c.Close() }()
	first := targets(s.t, c)[0].TargetID
	var made []string
	for i := range 19 {
		var r struct {
			TargetID string `json:"targetId"`
		}
		if err := c.Call(ctx(5), "", "Target.createTarget", map[string]any{"url": fmt.Sprintf("%s/blank?%d", s.site, i)}, &r); err != nil {
			s.t.Fatal(err)
		}
		made = append(made, r.TargetID)
	}
	defer func() {
		for _, id := range made {
			_ = c.Call(ctx(5), "", "Target.closeTarget", map[string]any{"targetId": id}, nil)
		}
	}()
	time.Sleep(2 * time.Second)
	count := func(v map[string]string) (visible []string) {
		for id, st := range v {
			if strings.HasPrefix(st, "visible") {
				visible = append(visible, id+" "+st)
			}
		}
		sort.Strings(visible)
		return visible
	}
	v, d := s.visibility(c)
	s.logf("  20 tabs, the last created in front: visible %v; reading all took %s", count(v), d.Round(time.Millisecond))
	s.logf("  (the last created is %s)", made[len(made)-1])
	if err := c.Call(ctx(5), "", "Target.activateTarget", map[string]any{"targetId": made[4]}, nil); err != nil {
		s.t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	v, d = s.visibility(c)
	s.logf("  after Target.activateTarget(%s): visible %v (%s)", made[4], count(v), d.Round(time.Millisecond))
	_ = c.Call(ctx(5), "", "Target.activateTarget", map[string]any{"targetId": first}, nil)
	time.Sleep(500 * time.Millisecond)
	v, _ = s.visibility(c)
	s.logf("  after activating the first tab (%s): visible %v", first, count(v))

	// A popup window, opened by a click (a user gesture).
	tb := attach(s.t, c, first)
	tb.navigate(s.site + "/popup")
	s.click(tb, "button")
	time.Sleep(2 * time.Second)
	for _, p := range targets(s.t, c) {
		if strings.HasSuffix(p.URL, "/blank?popup") {
			s.logf("  popup target: %s opener=%s", p.URL, p.OpenerID)
			made = append(made, p.TargetID)
		}
	}
	v, _ = s.visibility(c)
	s.logf("  with a popup window open: visible %v", count(v))
	// A target=_blank link.
	tb.navigate(s.site + "/newtab")
	s.click(tb, "a")
	time.Sleep(2 * time.Second)
	for _, p := range targets(s.t, c) {
		if strings.HasSuffix(p.URL, "/blank?tab") {
			s.logf("  target=_blank tab: %s opener=%s", p.URL, p.OpenerID)
			made = append(made, p.TargetID)
		}
	}
	v, _ = s.visibility(c)
	s.logf("  after a target=_blank click: visible %v", count(v))
}

// click clicks the centre of the first element matching selector.
func (s *spike) click(tb tab, selector string) {
	n := tb.node(selector)
	var q struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := tb.Call(ctx(5), "DOM.getContentQuads", map[string]any{"backendNodeId": n}, &q); err != nil || len(q.Quads) == 0 {
		s.t.Fatalf("quads: %v", err)
	}
	x := (q.Quads[0][0] + q.Quads[0][4]) / 2
	y := (q.Quads[0][1] + q.Quads[0][5]) / 2
	_ = tb.Call(ctx(5), "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}, nil)
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		if err := tb.Call(ctx(5), "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1}, nil); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *spike) largePages() {
	s.logf("## Large pages")
	c := s.dial()
	defer func() { _ = c.Close() }()
	tb := s.firstTab(c)
	measure := func(url string) {
		start := time.Now()
		if err := tb.Call(ctx(30), "Page.navigate", map[string]any{"url": url}, nil); err != nil {
			s.logf("  %s: %v", url, err)
			return
		}
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			f, _, at := tb.frame()
			var st string
			if w, err := tb.IsolatedWorld(ctx(5), f); err == nil && strings.HasPrefix(at, url[:20]) && tb.Eval(ctx(5), w, "document.readyState", &st) == nil && st == "complete" {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		load := time.Since(start)
		var nodes float64
		_ = tb.Eval(ctx(5), tb.world(), "document.getElementsByTagName('*').length", &nodes)
		var ax, snap json.RawMessage
		t0 := time.Now()
		axErr := tb.Call(ctx(60), "Accessibility.getFullAXTree", nil, &ax)
		axT := time.Since(t0)
		t0 = time.Now()
		snapErr := tb.Call(ctx(60), "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": []string{"display", "visibility", "opacity"}, "includeDOMRects": true}, &snap)
		snapT := time.Since(t0)
		s.logf("  %-60s %6.0f elements (load %s): AX tree %s in %s %v; DOM snapshot %s in %s %v", url, nodes, load.Round(time.Millisecond),
			mb(len(ax)), axT.Round(time.Millisecond), errOr(axErr), mb(len(snap)), snapT.Round(time.Millisecond), errOr(snapErr))
	}
	for _, n := range []int{100, 1000, 5000, 20000, 50000} {
		measure(fmt.Sprintf("%s/big?n=%d", s.site, n))
	}
	for _, u := range []string{"https://en.wikipedia.org/wiki/List_of_countries_and_dependencies_by_population", "https://html.spec.whatwg.org/"} {
		measure(u)
	}
}

func mb(n int) string { return fmt.Sprintf("%.2f MB", float64(n)/1e6) }

func errOr(err error) string {
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return ""
}

// rss sums the renderers' resident memory, in MB.
func (s *spike) rss() string {
	out, err := exec.Command("docker", "exec", "browserpier-"+s.name, "sh", "-c",
		`for p in /proc/[0-9]*; do if tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q -- '--type=renderer'; then grep VmRSS $p/status; fi; done`).CombinedOutput()
	if err != nil {
		return "? (" + err.Error() + ": " + string(out) + ")"
	}
	total := 0
	var each []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			kb, _ := strconv.Atoi(f[1])
			total += kb
			each = append(each, strconv.Itoa(kb/1024))
		}
	}
	return fmt.Sprintf("%d MB over %d renderers (%s)", total/1024, len(each), strings.Join(each, ", "))
}

func (s *spike) isolatedWorlds() {
	s.logf("## Isolated worlds: 1,000 calls on one document")
	c := s.dial()
	tb := s.firstTab(c)
	tb.navigate(s.site + "/big?n=2000")
	_ = c.Close()
	before := s.rss()
	worlds := map[int64]bool{}
	start := time.Now()
	for range 1000 {
		c := s.dial()
		tb := s.firstTab(c)
		w := tb.world()
		worlds[w] = true
		var v float64
		if err := tb.Eval(ctx(5), w, "document.body.childElementCount", &v); err != nil {
			s.t.Fatal(err)
		}
		_ = c.Close()
	}
	per := time.Since(start) / 1000
	s.logf("  renderer memory before: %s", before)
	s.logf("  after 1,000 calls (each: dial, attach, getFrameTree, createIsolatedWorld, evaluate, close): %s", s.rss())
	s.logf("  distinct execution contexts: %d; %s per call", len(worlds), per.Round(time.Millisecond/10))
}

func (s *spike) detector() {
	s.logf("## Continuous detector")
	w := browsertest.NewWatch(s.t, siteHost(s.t))
	c := s.dial()
	tb := s.firstTab(c)
	tb.navigate(w.URL)
	_ = c.Close()
	w.Ready(s.t)
	time.Sleep(2 * time.Second)
	w.Settle()
	s.logf("  nothing attached: %v", w.Kinds())
	w.Reset()

	runs := map[string]func(tab){
		"read (frame tree, AX tree, DOM snapshot, isolated world)": func(tb tab) {
			_ = tb.Call(ctx(10), "Accessibility.getFullAXTree", nil, nil)
			_ = tb.Call(ctx(10), "Accessibility.getPartialAXTree", map[string]any{"backendNodeId": tb.node("#go"), "fetchRelatives": false}, nil)
			_ = tb.Call(ctx(10), "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": []string{"display", "visibility", "opacity"}, "includeDOMRects": true}, nil)
			tb.eval("document.readyState + document.visibilityState")
		},
		"click (scroll, quads, hit-test, mouse events)": func(tb tab) {
			n := tb.node("#go")
			_ = tb.Call(ctx(5), "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": n}, nil)
			var r struct{ Object cdp.RemoteObject }
			_ = tb.Call(ctx(5), "DOM.resolveNode", map[string]any{"backendNodeId": n, "executionContextId": tb.world()}, &r)
			_ = tb.Call(ctx(5), "Runtime.callFunctionOn", map[string]any{"objectId": r.Object.ObjectID, "returnByValue": true,
				"functionDeclaration": "function() { const b = this.getBoundingClientRect(); const e = this.getRootNode().elementFromPoint(b.x + b.width / 2, b.y + b.height / 2); return e === this || this.contains(e); }"}, nil)
			s.click(tb, "#go")
		},
		"fill (focus, selectAll, insertText)": func(tb tab) {
			n := tb.node("#email")
			_ = tb.Call(ctx(5), "DOM.focus", map[string]any{"backendNodeId": n}, nil)
			tb.eval("document.activeElement.id")
			_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65, "modifiers": 2, "commands": []string{"selectAll"}}, nil)
			_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65, "modifiers": 2}, nil)
			_ = tb.Call(ctx(5), "Input.insertText", map[string]any{"text": "ana@example.com"}, nil)
		},
		"type (focus, key events)": func(tb tab) {
			n := tb.node("#pw")
			_ = tb.Call(ctx(5), "DOM.focus", map[string]any{"backendNodeId": n}, nil)
			for _, ch := range "pw" {
				k := string(ch)
				_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": k, "code": "Key" + strings.ToUpper(k), "text": k, "windowsVirtualKeyCode": int(ch) - 32}, nil)
				_ = tb.Call(ctx(5), "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k, "code": "Key" + strings.ToUpper(k), "windowsVirtualKeyCode": int(ch) - 32}, nil)
			}
		},
		"tabs (targets, visibility of each)": func(tb tab) {
			for _, p := range targets(s.t, tb.c()) {
				t2 := attach(s.t, tb.c(), p.TargetID)
				t2.eval("document.visibilityState")
				t2.Detach()
			}
		},
	}
	names := make([]string, 0, len(runs))
	for n := range runs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		w.Reset()
		for range 20 {
			c := s.dial()
			runs[name](s.firstTab(c))
			_ = c.Close()
		}
		w.Settle()
		s.logf("  20 × %s: %v", name, w.Kinds())
	}
	w.Reset()
	p := browsertest.Page(s.t, s.control)
	p.Call("Runtime.enable", nil)
	time.Sleep(2 * time.Second)
	w.Settle()
	s.logf("  Runtime.enable alone: %v", w.Kinds())
	browsertest.WatchPositiveControl(s.t, p)
	w.Settle()
	p.Close()
	s.logf("  positive control (Runtime.enable, Page.enable, binding, main-world script, CDP input): %v", w.Kinds())
}

func (tb tab) c() *cdp.Conn { return tb.Conn() }
