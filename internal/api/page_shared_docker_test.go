//go:build docker

package api

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// sharedPage builds, by script, n nodes that share one long string: the
// DOM snapshot sends it once, but a per-node cost grows with its length.
const sharedPage = `<!doctype html><title>Shared</title><body><p>Start of the page.</p><script>
const q = new URLSearchParams(location.search), n = +q.get('n'), kind = q.get('kind'), size = +q.get('size');
const spaces = ' '.repeat(size) + 'needle', long = 'a'.repeat(size);
const frag = document.createDocumentFragment();
for (let i = 0; i < n; i++) {
  let e;
  if (kind === 'text' || kind === 'hidden') {
    e = document.createElement('div'); e.textContent = spaces;
    if (kind === 'hidden') e.style.display = 'none';
  } else if (kind === 'fields') {
    e = document.createElement('input'); e.name = long; e.id = 'f' + i; e.setAttribute('autocomplete', long); e.setAttribute('aria-label', 'Field');
  }
  frag.append(e);
}
document.body.append(frag);
document.title = 'Shared ' + kind;
</script>`

// TestSharedStringsOnARealPage: snapshot and find stay quick on pages
// where thousands of nodes share a megabyte string, visible or not.
func TestSharedStringsOnARealPage(t *testing.T) {
	a := newRealAPI(t)
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/long" {
			_, _ = io.WriteString(w, longPage)
			return
		}
		_, _ = io.WriteString(w, sharedPage)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	base := fmt.Sprintf("http://%s:%d/", browsertest.SiteHost(t), ln.Addr().(*net.TCPAddr).Port)
	s := a.startAPIBrowser(t, "sh")
	// Long text is searched whole (first: the heavy pages below keep
	// Chrome busy for a while after).
	a.mustTool(s.ID, "navigate", map[string]string{"url": base + "long"})
	for _, m := range []string{"MARKERONE", "MARKERTWO", "MARKERTHREE"} {
		if found := a.mustTool(s.ID, "find", map[string]any{"text": m}); !strings.Contains(found, "1 match.") {
			t.Errorf("find %s in long text: %s", m, lastLine(found))
		}
	}
	// Sized so Chrome itself can build and lay the page out in time: it
	// handles each long attribute and visible text per element. Hidden
	// text, which it doesn't lay out, takes a megabyte each. (Iframes
	// sharing a long src are left to the unit test: Chrome can't build
	// even a hundred of them in time.)
	for _, c := range []struct {
		kind    string
		n, size int
	}{{"text", 2000, 100 << 10}, {"hidden", 5000, 1 << 20}, {"fields", 2000, 100 << 10}} {
		a.mustTool(s.ID, "navigate", map[string]string{"url": fmt.Sprintf("%s?kind=%s&n=%d&size=%d", base, c.kind, c.n, c.size)})
		start := time.Now()
		code, snap := a.tool(s.ID, "snapshot", nil)
		took := time.Since(start)
		t.Logf("%s × %d: snapshot %d in %s (%d characters)", c.kind, c.n, code, took.Round(time.Millisecond), len(snap))
		if took > 15*time.Second {
			t.Errorf("%s: snapshot took %s", c.kind, took)
		}
		if code != http.StatusOK || !strings.Contains(snap, "Title: Shared "+c.kind) {
			t.Fatalf("%s: the page wasn't built: %d %s", c.kind, code, snap)
		}
		start = time.Now()
		code, found := a.tool(s.ID, "find", map[string]any{"text": "needle"})
		took = time.Since(start)
		t.Logf("%s × %d: find %d in %s: %s", c.kind, c.n, code, took.Round(time.Millisecond), lastLine(found))
		if took > 15*time.Second {
			t.Errorf("%s: find took %s", c.kind, took)
		}
		if c.kind == "fields" && !strings.Contains(snap, `textbox "Field"`) {
			t.Errorf("fields: %s", snap)
		}
		if c.kind == "text" && !strings.Contains(found, "matches.") {
			t.Errorf("find on shared text: %d %s", code, lastLine(found))
		}
	}
}

// longPage has long text with a marker at its end: a 30 KB <pre> and
// <p>, and a <div> of 3,000 spans (about 96 KB).
const longPage = `<!doctype html><title>Long</title><body><script>
const filler = 'filler words '.repeat(2400);
const pre = document.createElement('pre'); pre.textContent = filler + 'MARKERONE'; document.body.append(pre);
const p = document.createElement('p'); p.textContent = filler + 'MARKERTWO'; document.body.append(p);
const div = document.createElement('div');
for (let i = 0; i < 3000; i++) { const s = document.createElement('span'); s.textContent = i < 2999 ? 'span text number xxxxxxxx ' : 'MARKERTHREE'; div.append(s); }
document.body.append(div);
</script>`

func lastLine(s string) string { return s[strings.LastIndex(s, "\n")+1:] }
