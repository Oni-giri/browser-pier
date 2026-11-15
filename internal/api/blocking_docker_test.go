//go:build docker

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/blocklist"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// adSite serves pages that embed an image and a script from another host,
// and records which hosts asked for what. Every name reaches it through
// the test's upstream proxy, which alone knows them (browsertest.Proxy
// aliases), so no DNS is needed.
type adSite struct {
	*httptest.Server
	port string
	mu   sync.Mutex
	hits map[string]int // by Host, without the port
}

func newAdSite(t *testing.T) *adSite {
	s := &adSite{hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		s.mu.Lock()
		s.hits[host]++
		s.mu.Unlock()
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><title>Article</title><p id="own">The page's own text.</p>
<img id="ad" src="http://ads.blocked.test:%[1]s/pixel.gif"><script src="http://ads.blocked.test:%[1]s/ad.js"></script>`, s.port)
		case "/pixel.gif":
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
		case "/ad.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(`window.adLoaded = true;`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	_, s.port, _ = net.SplitHostPort(s.Listener.Addr().String())
	return s
}

func (s *adSite) hitsOf(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[host]
}

func (s *adSite) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = map[string]int{}
}

// TestAdBlocking: with blocking on, a page's own text loads and the
// listed host it embeds gets no connection, not even to the upstream
// proxy; with "block_ads": false on the session it does. A page whose own
// host is listed still loads when the agent navigates to it, and what it
// embeds from other listed hosts stays blocked.
func TestAdBlocking(t *testing.T) {
	a := newRealAPI(t)
	site := newAdSite(t)
	up := browsertest.NewSOCKS5Proxy(t, "pier", "pw")
	for _, name := range []string{"news.example.test", "ads.blocked.test", "listed.blocked.test"} {
		up.Alias(name, "127.0.0.1")
	}
	// The list comes from a local server, as the orchestrator downloads
	// it, never from the internet.
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# test list\n0.0.0.0 ads.blocked.test\n||listed.blocked.test^\n"))
	}))
	t.Cleanup(listSrv.Close)
	lists, err := blocklist.New(t.TempDir(), []string{listSrv.URL}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := lists.Load(); err != nil {
		t.Fatal(err)
	}
	lists.Refresh(context.Background(), false)
	if lists.Set().Len() != 2 {
		t.Fatalf("the list compiled to %v", lists.Set().Names())
	}
	a.m.SetBlocklist(lists.File)

	name := fmt.Sprintf("ads%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.DefaultMode, p.Width, p.Height = profiles.ModeAPI, 1280, 800
	p.TimeZone, p.Locale = "Europe/Paris", "fr-FR" // a proxied profile with no lookup needs them
	p.Proxy = "socks5://pier:pw@" + net.JoinHostPort(browsertest.SiteHost(t), up.Port())
	if _, err := a.m.Profiles().Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !p.BlockAds {
		t.Fatal("a new profile doesn't block")
	}
	page := "http://news.example.test:" + site.port + "/"
	sawUpstream := func(host string) bool {
		for _, target := range up.Targets() {
			if strings.HasPrefix(target, host+":") {
				return true
			}
		}
		return false
	}
	// loaded waits for the page's own text and reports whether the
	// embedded script ran and the image has pixels.
	loaded := func(pg *browsertest.CDP) (script, image bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			own := pg.Eval(`document.getElementById('own') ? document.getElementById('own').textContent : ''`)
			done := pg.Eval(`document.readyState === 'complete'`)
			if own == "The page's own text." && done == true {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the page's own text never loaded: %v", own)
			}
			time.Sleep(200 * time.Millisecond)
		}
		return pg.Eval(`window.adLoaded === true`) == true, pg.Eval(`document.getElementById('ad').naturalWidth > 0`) == true
	}

	// Blocking on.
	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || !s.BlockAds {
		t.Fatalf("start: %d %+v", code, s)
	}
	pg := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	pg.Navigate(page)
	if script, image := loaded(pg); script || image {
		t.Errorf("blocking on, the listed host's script ran %v, image loaded %v", script, image)
	}
	if n := site.hitsOf("ads.blocked.test"); n != 0 {
		t.Errorf("blocking on, the listed host got %d requests", n)
	}
	if sawUpstream("ads.blocked.test") {
		t.Errorf("blocking on, the upstream proxy was asked for the listed host: %v", up.Targets())
	}
	if n, ok := a.m.Blocked(context.Background(), name); !ok || n < 1 {
		t.Errorf("blocked count %d, %v", n, ok)
	}

	// A page whose own host is listed loads when the agent opens it with
	// navigate; what it embeds from another listed host stays blocked.
	text := a.mustTool(s.ID, "navigate", map[string]string{"url": "http://listed.blocked.test:" + site.port + "/"})
	if !strings.Contains(text, "Title: Article") {
		t.Errorf("navigate to a listed host: %s", text)
	}
	if site.hitsOf("listed.blocked.test") == 0 {
		t.Error("the listed page opened directly never reached the site")
	}
	if script, image := loaded(pg); script || image {
		t.Errorf("on the listed page, the other listed host's script ran %v, image loaded %v", script, image)
	}
	if n := site.hitsOf("ads.blocked.test"); n != 0 {
		t.Errorf("on the listed page, the other listed host got %d requests", n)
	}
	pg.Close()
	if _, err := a.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}

	// The same page, with blocking off for this one session.
	site.reset()
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "block_ads": false}`, name), &s); code != http.StatusCreated || s.BlockAds {
		t.Fatalf("start without blocking: %d %+v", code, s)
	}
	pg = browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	pg.Navigate(page)
	if script, image := loaded(pg); !script || !image {
		t.Errorf("blocking off, the script ran %v, image loaded %v", script, image)
	}
	if site.hitsOf("ads.blocked.test") < 2 || !sawUpstream("ads.blocked.test") {
		t.Errorf("blocking off, the listed host got %d requests; upstream saw %v", site.hitsOf("ads.blocked.test"), up.Targets())
	}
	if got, _ := a.m.Profiles().Get(context.Background(), name); !got.BlockAds {
		t.Error("turning blocking off for a session changed the profile")
	}
	pg.Close()

	// Turned off on a running session: an attach with block_ads false.
	if _, err := a.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || !s.BlockAds {
		t.Fatalf("start: %d %+v", code, s)
	}
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q, "block_ads": false}`, name), &s); code != http.StatusOK || s.BlockAds {
		t.Fatalf("attach without blocking: %d %+v", code, s)
	}
	site.reset()
	pg = browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	defer pg.Close()
	pg.Navigate(page)
	if script, image := loaded(pg); !script || !image {
		t.Errorf("turned off while running, the script ran %v, image loaded %v", script, image)
	}
}
