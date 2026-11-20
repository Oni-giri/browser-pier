//go:build docker

package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// TestRealBrowserClipboardBox is PROJ-194 on a real browser in Visual
// mode, through the live view's own messages and no CDP: the clipboard
// box pastes into a page's field, Ctrl+V and Ctrl+C go to the browser as
// keys, a copy in the page reads back through the box, and the clipboard
// is empty once the person hands back. Nothing is attached to the page
// meanwhile.
func TestRealBrowserClipboardBox(t *testing.T) {
	m, name := startBrowserIn(t, profiles.ModeVisual, func(p *profiles.Profile) {
		p.LocalNetwork = true // the test page is on the Docker host
	})
	s, ok := m.Live(name)
	if !ok {
		t.Fatal("no live session")
	}
	page := newFieldPage(t)
	sock := m.Layout().ControlSocket(name)
	v := &viewer{h: NewHandler(nil, m, nil, slog.New(slog.DiscardHandler)), session: s.ID, sock: sock}
	replies := make(chan map[string]any, 10)
	send := func(msg any) {
		t.Helper()
		b, _ := json.Marshal(msg)
		v.message(b, func(r string) {
			var got map[string]any
			_ = json.Unmarshal([]byte(r), &got)
			replies <- got
		})
	}
	next := func(want string) map[string]any {
		t.Helper()
		select {
		case r := <-replies:
			if r["type"] != want {
				t.Fatalf("got %v, want a %s", r, want)
			}
			return r
		case <-time.After(15 * time.Second):
			t.Fatalf("no %s", want)
			return nil
		}
	}
	input := func(evs ...map[string]any) {
		t.Helper()
		send(map[string]any{"type": "input", "events": evs})
		time.Sleep(300 * time.Millisecond)
	}
	ctrl := func(k string) {
		input(map[string]any{"t": "keydown", "k": "Control_L"}, map[string]any{"t": "keydown", "k": k},
			map[string]any{"t": "keyup", "k": k}, map[string]any{"t": "keyup", "k": "Control_L"})
	}
	id := 0
	set := func(text string, paste bool) {
		t.Helper()
		id++
		send(map[string]any{"type": "clipboard-set", "id": id, "text": text, "paste": paste})
		if r := next("clipboard-set"); r["id"] != float64(id) {
			t.Fatalf("set answered %v", r)
		}
	}
	read := func() string {
		t.Helper()
		id++
		send(map[string]any{"type": "clipboard", "id": id})
		r := next("clipboard")
		return fmt.Sprint(r["text"])
	}

	send(map[string]any{"type": "take"})
	if r := next("control"); r["on"] != true {
		t.Fatalf("take: %v", r)
	}
	// The window manager focuses on click: click the page first. Then a
	// page's address is pasted into the address bar from the box.
	input(map[string]any{"t": "move", "x": 640, "y": 500}, map[string]any{"t": "down", "b": 1}, map[string]any{"t": "up", "b": 1})
	open := func(url string) {
		ctrl("l")
		set(url, true)
		input(map[string]any{"t": "keydown", "k": "Return"}, map[string]any{"t": "keyup", "k": "Return"})
	}
	// While here: the live view's script parses in this Chromium.
	open(page.url + "parse")
	page.await(t, "live.js to parse", func(f fieldState) bool { return f.parsed != "" })
	if f := page.state(); f.parsed != "ok" {
		t.Errorf("live.js doesn't parse: %s", f.parsed)
	}
	open(page.url)
	page.await(t, "the page to load", func(f fieldState) bool { return f.loaded })
	// Its field fills the page: a click focuses it.
	input(map[string]any{"t": "move", "x": 640, "y": 400}, map[string]any{"t": "down", "b": 1}, map[string]any{"t": "up", "b": 1})

	// Paste into page: the text arrives exactly, accents included, as a
	// paste.
	const text = "Rīga ūdens 42, naïve café ünïcode ĀČĒ ģķļ šž"
	set(text, true)
	page.await(t, "the paste", func(f fieldState) bool { return f.value == text && f.pastes == 1 })
	if got := read(); got != text {
		t.Errorf("the clipboard holds %q, want what was pasted", got)
	}

	// Send to browser, then Ctrl+V as keys, as the person presses them.
	set("from the box", false)
	ctrl("a")
	ctrl("v")
	page.await(t, "Ctrl+V's paste", func(f fieldState) bool { return f.value == "from the box" && f.pastes == 2 })

	// Ctrl+C in the page: Chromium takes the clipboard (the box's xclip
	// lets go), and the box reads what it copied.
	ctrl("a")
	set(text, true) // the field holds text again
	page.await(t, "the field to hold the text again", func(f fieldState) bool { return f.value == text })
	set("not this", false)
	ctrl("a")
	ctrl("c")
	time.Sleep(300 * time.Millisecond)
	if got := read(); got != text {
		t.Errorf("after Ctrl+C the box reads %q, want %q", got, text)
	}

	// In Visual mode nothing is attached to the page, the clipboard box
	// included.
	for _, tg := range browserTargets(t, sock) {
		if tg.Type == "page" && tg.Attached {
			t.Errorf("a CDP client is attached to %s", tg.URL)
		}
	}

	// What is selected in the page is PRIMARY, Chromium's, which a
	// middle-click pastes: here, the person's own middle-click.
	if got, ok := xselection(t, name, "primary", "UTF8_STRING"); !ok || got != text {
		t.Fatalf("with the field's text selected, PRIMARY holds %q (%t)", got, ok)
	}
	before := page.state().pastes
	input(map[string]any{"t": "move", "x": 640, "y": 400}, map[string]any{"t": "down", "b": 2}, map[string]any{"t": "up", "b": 2})
	page.await(t, "the middle-click's paste", func(f fieldState) bool { return f.pastes == before+1 && f.value != text })
	ctrl("a") // selected again: PRIMARY holds it all

	// Handing back empties both selections, Chromium's own included.
	send(map[string]any{"type": "release"})
	if r := next("control"); r["on"] != false {
		t.Fatalf("release: %v", r)
	}
	if got, err := v.do(http.MethodGet, "/input/clipboard", nil, http.StatusOK, controlWait); err != nil || len(got) != 0 {
		t.Errorf("after handing back the clipboard holds %q (%v)", got, err)
	}
	for _, sel := range []string{"clipboard", "primary"} {
		if got, ok := xselection(t, name, sel, "TARGETS"); ok {
			t.Errorf("after handing back %s has an owner, offering %q", sel, got)
		}
	}
	// An agent's middle-click into the field pastes nothing.
	after := page.state()
	body, _ := json.Marshal(map[string]any{"x": 640, "y": 400, "button": "middle", "pace": "instant"})
	if _, err := v.do(http.MethodPost, "/input/click", body, http.StatusNoContent, controlWait); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	// (Chromium still fires a paste event, with nothing to paste.)
	if f := page.state(); f.value != after.value {
		t.Errorf("an agent's middle-click after the hand-back pasted: %q, was %q", f.value, after.value)
	}
	// Without control, the box is refused.
	send(map[string]any{"type": "clipboard", "id": 99})
	if r := next("clipboard-error"); r["id"] != float64(99) {
		t.Errorf("a read without control: %v", r)
	}
}

// fieldPage is a test site with one text field filling the page, which
// reports its value and its paste events.
type fieldPage struct {
	url string
	mu  sync.Mutex
	f   fieldState
}

type fieldState struct {
	parsed string
	loaded bool
	value  string
	pastes int
}

func newFieldPage(t *testing.T) *fieldPage {
	t.Helper()
	p := &fieldPage{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>field</title>
<textarea id="f" autofocus style="position:fixed;inset:0;width:100%;height:100%;font-size:32px"></textarea>
<script>
const f = document.getElementById("f");
const tell = (what, body) => fetch("/" + what, {method: "POST", body});
f.addEventListener("input", () => tell("value", f.value));
f.addEventListener("paste", () => tell("paste", ""));
tell("loaded", "");
</script>`)
	})
	mux.HandleFunc("GET /live.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(liveJS)
	})
	mux.HandleFunc("GET /parse", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><title>parse</title><script>
fetch("/live.js").then(r => r.text()).then(src => {
  let res = "ok";
  try { new Function(src); } catch (e) { res = String(e); }
  return fetch("/parsed", {method: "POST", body: res});
});
</script>`)
	})
	mux.HandleFunc("POST /{what}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		switch r.PathValue("what") {
		case "parsed":
			p.f.parsed = string(b)
		case "loaded":
			p.f.loaded = true
		case "value":
			p.f.value = string(b)
		case "paste":
			p.f.pastes++
		}
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	p.url = fmt.Sprintf("http://%s:%d/", browsertest.SiteHost(t), ln.Addr().(*net.TCPAddr).Port)
	return p
}

func (p *fieldPage) state() fieldState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.f
}

// await waits, for 15 s at most, until the page's state is as ok says.
func (p *fieldPage) await(t *testing.T, what string, ok func(fieldState) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok(p.state()) {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s: %+v", what, p.state())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// xselection reads an X selection in the profile's browser container as
// target, with xclip, as the test's own look: ok is false when xclip
// finds nothing (no owner, or none with that target).
func xselection(t *testing.T, profile, selection, target string) (string, bool) {
	t.Helper()
	out, err := exec.Command("docker", "exec", "-e", "DISPLAY=:99", "browserpier-"+profile,
		"xclip", "-selection", selection, "-out", "-target", target).Output()
	return string(out), err == nil
}

type browserTarget struct {
	Type, URL string
	Attached  bool
}

// browserTargets lists the browser's targets over a browser-level CDP
// connection on the control socket, which attaches to no page.
func browserTargets(t *testing.T, sock string) []browserTarget {
	t.Helper()
	control := browsertest.ControlClient(sock)
	resp, err := control.Get("http://control/cdp/json/version")
	if err != nil {
		t.Fatal(err)
	}
	var ver struct{ WebSocketDebuggerURL string }
	err = json.NewDecoder(resp.Body).Decode(&ver)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := ver.WebSocketDebuggerURL[strings.Index(ver.WebSocketDebuggerURL, "/devtools/"):]
	//nolint:bodyclose // after an upgrade the connection owns the body
	conn, _, err := websocket.Dial(ctx, "ws://control/cdp"+path, &websocket.DialOptions{HTTPClient: control})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Target.getTargets"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			ID     int
			Result struct{ TargetInfos []browserTarget }
		}
		if json.Unmarshal(raw, &r) == nil && r.ID == 1 {
			return r.Result.TargetInfos
		}
	}
}
