package browsertest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// KeySite serves the pages PROJ-151 measures keyboard shortcuts on: /keys,
// a page whose keydown listener takes as long as asked, and /arrived, the
// page an address typed after the shortcut should open. Each page load
// is a run, named by its n, and the pages report what they saw to the
// site, as PageSite's pages do, so a test needn't touch them over CDP.
type KeySite struct {
	URL  string // http://host:port
	Port string

	mu   sync.Mutex
	runs map[string]*KeyRun
}

// KeyEvent is a keydown as the /keys page's own script recorded it.
type KeyEvent struct {
	Key     string  `json:"key"`
	Ctrl    bool    `json:"ctrl"`
	Alt     bool    `json:"alt"`
	Trusted bool    `json:"trusted"` // isTrusted
	T       float64 `json:"t"`       // its timeStamp, in milliseconds
	seq     int     // the report it came in
}

// KeyRun is what the pages with one n reported.
type KeyRun struct {
	// Ready: a /keys page loaded and drew twice. Focus is whether its
	// document had the focus then, and Active the id (or tag) of its
	// focused element.
	Ready  bool
	Focus  bool
	Active string
	// Keys are the keydowns the page saw, in its order; Value is its
	// field's text as last reported.
	Keys  []KeyEvent
	Value string
	// Arrived: an /arrived page with this n loaded (not as a prerender).
	Arrived bool
	seq     int
	next    string // where its /arrived page goes on to
}

// keysPage's keydown listener records each key, then busy-waits slow ms,
// as a page whose key handler is busy does. With focus=field an
// autofocused field fills the top of the page; with focus=body there is
// none; with frame=URL the page is that URL in an iframe filling it. It
// reports each key as it comes, its field's text every 50 ms, once it has
// drawn twice, and as it goes.
const keysScript = `<script>
const q = new URLSearchParams(location.search);
const n = q.get('n') || '', slow = Number(q.get('slow')) || 0;
const field = document.getElementById('f');
let batch = [], seq = 0, sent = '';
function body(extra) {
  const a = document.activeElement;
  const b = Object.assign({seq: ++seq, keys: batch, value: field ? field.value : '', focus: document.hasFocus(),
    active: a ? (a.id || a.localName) : ''}, extra || {});
  batch = []; sent = b.value;
  return JSON.stringify(b);
}
const to = '/keys/report?n=' + encodeURIComponent(n);
function send(extra) { fetch(to, {method: 'POST', body: body(extra), keepalive: true}).catch(() => {}); }
addEventListener('keydown', e => {
  const start = performance.now();
  // A beacon goes out at once: a busy page may be gone before its next report.
  batch.push({key: e.key, ctrl: e.ctrlKey, alt: e.altKey, trusted: e.isTrusted, t: e.timeStamp});
  navigator.sendBeacon(to, body());
  while (performance.now() - start < slow) {}
}, true);
setInterval(() => { if (batch.length || (field && field.value !== sent)) send(); }, 50);
addEventListener('pagehide', () => navigator.sendBeacon(to, body()));
addEventListener('load', () => requestAnimationFrame(() => requestAnimationFrame(() => send({ready: true}))));
</script>`

// arrivedPage reports that it loaded (once it is shown, not while
// Chrome prerenders it from the address bar), then goes on to the page
// the site names, if any.
const arrivedPage = `<!doctype html><title>Arrived</title><p>Arrived.</p><script>
const n = new URLSearchParams(location.search).get('n') || '', next = %s;
function go() {
  fetch('/arrived/report?n=' + encodeURIComponent(n), {method: 'POST', keepalive: true})
    .catch(() => {}).finally(() => { if (next) location.replace(next); });
}
if (document.prerendering) document.addEventListener('prerenderingchange', go, {once: true}); else go();
</script>`

// NewKeySite serves the pages on every interface; host is how the
// browser container reaches this machine, from SiteHost.
func NewKeySite(t *testing.T, host string) *KeySite {
	t.Helper()
	s := &KeySite{runs: map[string]*KeyRun{}}
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.Port = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	s.URL = "http://" + net.JoinHostPort(host, s.Port)
	return s
}

func (s *KeySite) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if ms, _ := strconv.Atoi(q.Get("wait")); ms > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Duration(min(ms, 10_000)) * time.Millisecond):
			}
		}
		content := ""
		switch {
		case q.Get("frame") != "":
			// Some text of its own, so Chrome paints the page and takes
			// clicks on it without waiting for the frame.
			content = `<p style="margin: 0; height: 24px">Around the frame</p><iframe id="frame" title="Frame" src="` + html.EscapeString(q.Get("frame")) + `"></iframe>`
		case q.Get("focus") == "field":
			content = `<input id="f" aria-label="Field" autofocus autocomplete="off">`
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// So a restored tab loads it again rather than from the cache.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, `<!doctype html><title>Keys</title>
<style>html, body { margin: 0; height: 100%; } #f { display: block; box-sizing: border-box; width: 100%; height: 60vh; font-size: 24px; }
iframe { display: block; border: 0; width: 100%; height: calc(100% - 24px); }</style>
`+content+keysScript)
	})
	mux.HandleFunc("GET /dialog", func(w http.ResponseWriter, r *http.Request) {
		// A click anywhere opens what kind asks for, as a page's own
		// button would: a pop-up window over the whole browser window, or
		// an alert.
		open := "alert('A dialog')"
		if r.URL.Query().Get("kind") == "popup" {
			open = "window.open('about:blank', 'p', 'popup,left=0,top=0,width=' + screen.availWidth + ',height=' + screen.availHeight)"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><title>Dialog</title>
<style>html, body { margin: 0; height: 100%; } button { width: 100%; height: 100%; font-size: 32px; }</style>
<button onclick="`+open+`">Open it</button>`)
	})
	mux.HandleFunc("POST /keys/report", func(w http.ResponseWriter, r *http.Request) {
		var rep struct {
			Seq    int        `json:"seq"`
			Keys   []KeyEvent `json:"keys"`
			Value  string     `json:"value"`
			Focus  bool       `json:"focus"`
			Active string     `json:"active"`
			Ready  bool       `json:"ready"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		run := s.run(r.URL.Query().Get("n"))
		for _, k := range rep.Keys {
			k.seq = rep.Seq
			i, _ := slices.BinarySearchFunc(run.Keys, k, func(a, b KeyEvent) int { return cmp.Compare(a.seq, b.seq) })
			// Beacons can arrive out of order; keep the page's order.
			for i < len(run.Keys) && run.Keys[i].seq == k.seq {
				i++
			}
			run.Keys = slices.Insert(run.Keys, i, k)
		}
		// Reports can arrive out of order; the newest says the value.
		if rep.Seq >= run.seq {
			run.seq, run.Value = rep.Seq, rep.Value
		}
		if rep.Ready {
			run.Ready, run.Focus, run.Active = true, rep.Focus, rep.Active
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /arrived", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		next, _ := json.Marshal(s.run(r.URL.Query().Get("n")).next)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, arrivedPage, strings.ReplaceAll(string(next), "<", `\u003c`))
	})
	mux.HandleFunc("POST /arrived/report", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.run(r.URL.Query().Get("n")).Arrived = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// run is n's run, with s.mu held.
func (s *KeySite) run(n string) *KeyRun {
	r := s.runs[n]
	if r == nil {
		r = &KeyRun{}
		s.runs[n] = r
	}
	return r
}

// Keys is the /keys page for run n: its key handler takes slow ms, and
// focus is "field" (an autofocused field) or "body".
func (s *KeySite) Keys(n string, slow int, focus string) string {
	return fmt.Sprintf("%s/keys?n=%s&slow=%d&focus=%s", s.URL, url.QueryEscape(n), slow, url.QueryEscape(focus))
}

// Dialog is a page a click on which opens kind: "popup", a pop-up window
// as big as the browser's, or "alert".
func (s *KeySite) Dialog(kind string) string {
	return fmt.Sprintf("%s/dialog?kind=%s", s.URL, url.QueryEscape(kind))
}

// Loading is a /keys page u that the site takes ms milliseconds (at
// most 10 s) to answer: a page still loading.
func Loading(u string, ms int) string { return fmt.Sprintf("%s&wait=%d", u, ms) }

// Framed is a /keys page for run n that is frame in an iframe.
func (s *KeySite) Framed(n, frame string) string {
	return fmt.Sprintf("%s/keys?n=%s&frame=%s", s.URL, url.QueryEscape(n), url.QueryEscape(frame))
}

// Arrived is the /arrived page for run n, which goes on to next if it
// isn't empty. next is kept by the site, so the address is short to type.
func (s *KeySite) Arrived(n, next string) string {
	s.mu.Lock()
	s.run(n).next = next
	s.mu.Unlock()
	return fmt.Sprintf("%s/arrived?n=%s", s.URL, url.QueryEscape(n))
}

// At is the site's base URL under another host name or address of this
// machine: another site, for a cross-site frame.
func (s *KeySite) At(host string) string { return "http://" + net.JoinHostPort(host, s.Port) }

// Run is what run n's pages have reported so far.
func (s *KeySite) Run(n string) KeyRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := *s.run(n)
	r.Keys = append([]KeyEvent(nil), r.Keys...)
	return r
}

// Forget drops what run n's pages have reported.
func (s *KeySite) Forget(n string) {
	s.mu.Lock()
	delete(s.runs, n)
	s.mu.Unlock()
}

// WaitRun waits up to timeout for run n to satisfy done, and returns it
// and whether it did.
func (s *KeySite) WaitRun(n string, timeout time.Duration, done func(KeyRun) bool) (KeyRun, bool) {
	deadline := time.Now().Add(timeout)
	for {
		r := s.Run(n)
		if done(r) {
			return r, true
		}
		if time.Now().After(deadline) {
			return r, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
