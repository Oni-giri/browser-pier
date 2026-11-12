package browsertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Hit is something the continuous detector (Watch) saw that a page could
// take for automation.
type Hit struct {
	// Kind names the check, e.g. "console-stack" or "main-world-call".
	Kind string `json:"kind"`
	// Where is "main" (the page's main thread) or "worker".
	Where  string `json:"where"`
	Detail string `json:"detail"`
}

func (h Hit) String() string { return h.Kind + "@" + h.Where + " " + h.Detail }

// Watch serves a page that keeps checking for CDP automation while it is
// open, on its main thread and in a worker, and reports what it finds.
// It is the continuous detector PROJ-21 and PROJ-57 measure their CDP use
// against; a client doing what Playwright does is its positive control
// (see WatchPositiveControl).
//
// Every 200 ms, on the main thread and in a dedicated worker, it checks:
//   - console-stack: a getter on an Error's stack that only a client
//     with the Runtime domain enabled used to read, when the inspector
//     serialised console arguments (the classic Runtime.enable leak,
//     closed in recent Chromium; getters on message and name aren't
//     usable, as Chromium's own console reads them with nothing attached);
//   - console-proxy: a Proxy logged to the console that is inspected;
//   - console-slow: console.debug of a large object costing much more
//     than it did when the page loaded, as when the inspector serialises
//     it (timing; reported with the numbers);
//   - webdriver and automation globals (cdc_…, __playwright…,
//     __puppeteer…), and bindings Playwright adds to the main world.
//
// On the main thread it also hooks the DOM methods automation calls
// (querySelector, getBoundingClientRect, focus, scrollIntoView,
// elementFromPoint…): the page itself never calls them, so any call
// comes from code in the page's own world (main-world-call). Scripts in
// isolated worlds have their own copies and never reach the hooks.
//
// And it looks at input: a mouse event whose screen position equals its
// client position although the window has a toolbar
// (input-screen-equals-client), a click with no mouse movement before it
// (click-without-move), a key event without a code (key-without-code),
// and text that arrives with no key pressed (input-without-key).
type Watch struct {
	URL string

	mu    sync.Mutex
	hits  []Hit
	ready chan struct{}
	once  sync.Once
	polls int
}

// watchPage is the detector. The page is plain HTML with a form, so tools
// have something to read and act on.
const watchPage = `<!doctype html><title>Watch</title>
<style>body{font:16px sans-serif;margin:20px}input,button{display:block;margin:10px 0;font-size:18px;padding:6px}</style>
<h1>Account</h1>
<p>Some text to read on this page.</p>
<label for="email">Email</label><input id="email" name="email">
<label for="pw">Password</label><input id="pw" name="password" type="password">
<button id="go" type="button">Continue</button>
<p id="out"></p>
<script>
(() => {
  const report = (kind, where, detail) => {
    fetch('/hit', {method: 'POST', body: JSON.stringify({kind, where, detail: String(detail || '')})}).catch(() => {});
  };
  const probes = ` + "`" + `
    const once = new Set();
    function probe(where, report) {
      const fire = (k, d) => { if (!once.has(k)) { once.add(k); report(k, where, d); } };
      const e = new Error('x');
      let got = false;
      Object.defineProperty(e, 'stack', {configurable: true, get() { got = true; return ''; }});
      console.debug(e);
      if (got) fire('console-stack', '');
      let trapped = false;
      const p = new Proxy({}, {ownKeys(t) { trapped = true; return Reflect.ownKeys(t); },
        getOwnPropertyDescriptor(t, k) { trapped = true; return Reflect.getOwnPropertyDescriptor(t, k); },
        get(t, k) { trapped = true; return Reflect.get(t, k); }});
      console.debug(p);
      if (trapped) fire('console-proxy', '');
      const big = {}; for (let i = 0; i < 200; i++) big['k' + i] = {a: [1, 2, 3], s: 'v' + i};
      const t0 = performance.now();
      for (let i = 0; i < 50; i++) console.debug(big);
      const cost = performance.now() - t0;
      if (probe.base === undefined) probe.base = Math.max(cost, 0.5);
      if (cost > probe.base * 5 && cost > 5) fire('console-slow', cost.toFixed(1) + ' ms vs ' + probe.base.toFixed(1) + ' ms at load');
      if (typeof navigator !== 'undefined' && navigator.webdriver) fire('webdriver', '');
    }
  ` + "`" + `;
  // The main thread.
  const main = new Function(probes + ' return probe;')();
  const tick = () => {
    main('main', report);
    const globals = Object.getOwnPropertyNames(window).filter(k => /^(cdc_|\$cdc_|__playwright|__pw|__puppeteer|__selenium|__webdriver|domAutomation)/.test(k));
    if (globals.length) report('automation-globals', 'main', globals.join(','));
    fetch('/poll', {method: 'POST'}).catch(() => {});
  };
  setInterval(tick, 200);
  // A worker, which a client attaching to workers (auto-attach) would reach.
  const src = probes + ` + "`" + `
    setInterval(() => probe('worker', (k, w, d) => postMessage({k, w, d})), 200);
  ` + "`" + `;
  const worker = new Worker(URL.createObjectURL(new Blob([src], {type: 'text/javascript'})));
  worker.onmessage = e => report(e.data.k, e.data.w, e.data.d);
  // The page's own use of the DOM, before the hooks below.
  const out = document.getElementById('out');
  document.getElementById('go').addEventListener('click', () => { out.textContent = 'Clicked'; });
  // Hooks on what automation calls in the page's own world.
  const hook = (proto, name) => {
    const orig = proto[name];
    if (typeof orig !== 'function') return;
    Object.defineProperty(proto, name, {configurable: true, writable: true, value: function (...args) {
      report('main-world-call', 'main', name);
      return orig.apply(this, args);
    }});
  };
  for (const [p, n] of [[Document.prototype, 'querySelector'], [Document.prototype, 'querySelectorAll'],
      [Element.prototype, 'querySelector'], [Element.prototype, 'querySelectorAll'],
      [Element.prototype, 'getBoundingClientRect'], [Element.prototype, 'getClientRects'],
      [HTMLElement.prototype, 'focus'], [Element.prototype, 'scrollIntoView'],
      [Element.prototype, 'scrollIntoViewIfNeeded'], [Document.prototype, 'elementFromPoint'],
      [Document.prototype, 'getElementById'], [Document.prototype, 'evaluate'],
      [Node.prototype, 'getRootNode'], [EventTarget.prototype, 'dispatchEvent']]) hook(p, n);
  // Input.
  let lastMove = 0, keyAt = 0;
  addEventListener('mousemove', () => { lastMove = performance.now(); }, true);
  for (const type of ['mousedown', 'mouseup', 'click']) {
    addEventListener(type, e => {
      if (!e.isTrusted) report('untrusted-event', 'main', type);
      const toolbar = outerHeight - innerHeight;
      if (e.screenX === e.clientX && e.screenY === e.clientY && toolbar > 0) report('input-screen-equals-client', 'main', type + ' at ' + e.clientX + ',' + e.clientY);
      if (type === 'click' && performance.now() - lastMove > 2000) report('click-without-move', 'main', '');
    }, true);
  }
  addEventListener('keydown', e => {
    keyAt = performance.now();
    if (!e.isTrusted) report('untrusted-event', 'main', 'keydown');
    if (e.code === '' && e.key.length === 1) report('key-without-code', 'main', '');
  }, true);
  addEventListener('beforeinput', e => {
    if (e.inputType === 'insertText' && performance.now() - keyAt > 1000) report('input-without-key', 'main', e.inputType);
  }, true);
})();
</script>`

// NewWatch serves the detector on every interface; host is how the
// browser container reaches this machine.
func NewWatch(t *testing.T, host string) *Watch {
	t.Helper()
	w := &Watch{ready: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(rw, watchPage)
	})
	mux.HandleFunc("POST /hit", func(rw http.ResponseWriter, r *http.Request) {
		var h Hit
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&h); err == nil {
			w.mu.Lock()
			w.hits = append(w.hits, h)
			w.mu.Unlock()
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /poll", func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.Lock()
		w.polls++
		w.mu.Unlock()
		w.once.Do(func() { close(w.ready) })
		rw.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	w.URL = fmt.Sprintf("http://%s:%d/", host, ln.Addr().(*net.TCPAddr).Port)
	return w
}

// Ready waits until a page has started polling.
func (w *Watch) Ready(t *testing.T) {
	t.Helper()
	select {
	case <-w.ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the detector page never started")
	}
}

// Polls counts the main thread's checks so far, across pages.
func (w *Watch) Polls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.polls
}

// Settle waits for a few more checks, so what was done has been looked at.
func (w *Watch) Settle() {
	start := w.Polls()
	deadline := time.Now().Add(10 * time.Second)
	for w.Polls() < start+3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// Hits returns what the detector saw since the last Reset.
func (w *Watch) Hits() []Hit {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Hit(nil), w.hits...)
}

// Kinds returns the distinct hits, as "kind@where", sorted by first sight.
func (w *Watch) Kinds() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range w.Hits() {
		k := h.Kind + "@" + h.Where
		if h.Kind == "main-world-call" {
			k += ":" + h.Detail
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Reset forgets the hits so far.
func (w *Watch) Reset() {
	w.mu.Lock()
	w.hits = nil
	w.mu.Unlock()
}

// HasKind reports whether any hit is of a kind starting with prefix.
func (w *Watch) HasKind(prefix string) bool {
	for _, h := range w.Hits() {
		if strings.HasPrefix(h.Kind, prefix) {
			return true
		}
	}
	return false
}

// WatchPositiveControl does to a page what Playwright does when it
// connects and acts, over a CDP connection to the page (see Page and
// PageVia): it enables Runtime and Page, adds a binding and an init
// script, runs code in the page's own world, and clicks and types with
// CDP input. The detector must see it, or its silence means nothing.
func WatchPositiveControl(t *testing.T, p *CDP) {
	t.Helper()
	p.Call("Runtime.enable", nil)
	p.Call("Page.enable", nil)
	_, _ = p.Try("Runtime.addBinding", map[string]any{"name": "__playwright__binding__"})
	_, _ = p.Try("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "void 0"})
	p.Eval(`(() => { const r = document.querySelector('#go').getBoundingClientRect(); return [r.x, r.y]; })()`)
	p.Eval(`document.querySelector('#email').focus()`)
	_, _ = p.Try("Input.insertText", map[string]any{"text": "a"})
	box, _ := p.Eval(`(() => { const r = document.querySelector('#go').getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2]; })()`).([]any)
	if len(box) == 2 {
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			_, _ = p.Try("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": box[0], "y": box[1], "button": "left", "clickCount": 1})
		}
	}
	// Give the console checks time to run while Runtime is enabled.
	time.Sleep(time.Second)
}
