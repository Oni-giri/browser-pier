//go:build docker

package chromium

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// What PROJ-191 checks: an address typed into the address bar, as Visual
// mode's `key ctrl+l` and `type` and a person in the live view type one,
// mustn't reach the search engine's suggest service on its way, nor an
// address PROJ-164's open action types into the same address bar.
//
// The rig makes the suggest service one the test can read. A test policy
// file, mounted next to the image's own, makes the default search engine
// suggest.test, and every connection the browser makes goes through an
// isolated upstream proxy, which routes suggest.test and site.test (the
// address being opened) to a local server and refuses, and records,
// everything else. So nothing reaches the internet, and a host the browser
// tried shows up even though nothing reached it.

// seen is one request the local server answered.
type seen struct {
	at   time.Time
	host string // without the port
	uri  string // the request URI, decoded
}

type suggestRig struct {
	b  *browser
	up *browsertest.Proxy
	mu sync.Mutex
	// reqs is every request to suggest.test or site.test, in order.
	reqs []seen
}

// searchPolicy makes suggest.test the default search engine, with a
// suggest URL.
const searchPolicy = `{
  "DefaultSearchProviderEnabled": true,
  "DefaultSearchProviderName": "Local test",
  "DefaultSearchProviderKeyword": "local",
  "DefaultSearchProviderSearchURL": "http://suggest.test/search?q={searchTerms}",
  "DefaultSearchProviderSuggestURL": "http://suggest.test/suggest?q={searchTerms}"
}`

// newSuggestRig starts a browser behind the isolated proxy. With
// localEngine, suggest.test is its search engine; without, it keeps the
// image's own (the Debian package's DuckDuckGo), which the proxy refuses.
func newSuggestRig(t *testing.T, image string, localEngine bool) *suggestRig {
	t.Helper()
	r := &suggestRig{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.Host)
		if err != nil {
			host = req.Host
		}
		uri, err := url.QueryUnescape(req.URL.RequestURI())
		if err != nil {
			uri = req.URL.RequestURI()
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, seen{time.Now(), host, uri})
		r.mu.Unlock()
		switch {
		case host == "suggest.test" && req.URL.Path == "/suggest":
			q := req.URL.Query().Get("q")
			w.Header().Set("Content-Type", "application/x-suggestions+json")
			_ = json.NewEncoder(w).Encode([]any{q, []string{q + " one", q + " two"}})
		case host == "suggest.test":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, `<!doctype html><title>Results</title><p>Search results`)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, `<!doctype html><title>Opened</title><p>The address opened`)
		}
	})
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(handler) // its certificate fails; what matters is what is sent before
	t.Cleanup(tlsSrv.Close)

	r.up = browsertest.NewSOCKS5Proxy(t, "pier", "pw")
	r.up.Isolate()
	r.up.Route("suggest.test:80", plain.Listener.Addr().String())
	r.up.Route("site.test:80", plain.Listener.Addr().String())
	r.up.Route("site.test:443", tlsSrv.Listener.Addr().String())

	opt := browserOptions{proxy: "socks5://pier:pw@" + net.JoinHostPort(browsertest.SiteHost(t), r.up.Port())}
	// A proxied browser with no location lookup needs them set.
	opt.args = []string{"-e", "BROWSERPIER_TZ_OVERRIDE=Europe/Paris", "-e", "BROWSERPIER_LOCALE_OVERRIDE=fr-FR"}
	if localEngine {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the container's user reads it
			t.Fatal(err)
		}
		file := filepath.Join(dir, "search.json")
		if err := os.WriteFile(file, []byte(searchPolicy), 0o644); err != nil { //nolint:gosec // the container's user reads it
			t.Fatal(err)
		}
		opt.args = append(opt.args, "-v", file+":/etc/chromium/policies/managed/zz-test-search.json:ro")
	}
	r.b = startBrowserWith(t, image, newVolume(t), opt)

	// A new profile's first tab opens start.duckduckgo.com (PROJ-127),
	// which the proxy refuses, and an address typed while that settles
	// can be lost. Open one until it opens, so no check passes on typing
	// that never reached the address bar. Each try has its own path, so a
	// late answer to one doesn't count for the next.
	for i := 0; ; i++ {
		tp := r.open(t, fmt.Sprintf("http://site.test/ready/%d", i), "instant")
		if r.opened(tp) {
			break
		}
		if i == 4 {
			t.Fatalf("no address typed into the address bar opened in 5 tries\n%s", r.b.logs())
		}
	}
	return r
}

// opened reports whether site.test got, after Return, exactly the path
// and query typed: not just any request to it, which an earlier
// address's late answer, or one that lost part of what was typed, could
// make.
func (r *suggestRig) opened(tp typed) bool {
	want := strings.TrimPrefix(strings.TrimPrefix(tp.text, "http://"), "site.test")
	for _, s := range r.since("site.test", tp.enter) {
		if s.uri == want {
			return true
		}
	}
	return false
}

// uris lists reqs' request URIs, for a message.
func uris(reqs []seen) []string {
	out := make([]string, 0, len(reqs))
	for _, s := range reqs {
		out = append(out, s.uri)
	}
	return out
}

// since lists the requests to host from t on.
func (r *suggestRig) since(host string, from time.Time) []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []seen
	for _, s := range r.reqs {
		if s.host == host && !s.at.Before(from) {
			out = append(out, s)
		}
	}
	return out
}

// typed is what one address typed into the address bar caused.
type typed struct {
	text     string
	start    time.Time // the first key
	enter    time.Time // Return
	suggests []seen    // requests to suggest.test's /suggest
	early    []seen    // requests to site.test before Return
	targets  []string  // hosts the proxy reached: only site.test and suggest.test can be
	refused  []string  // hosts the proxy refused, so they got nothing
}

// open focuses the address bar, types text at pace, waits for suggest
// requests (Chromium sends them after a short pause), presses Return,
// and waits for what follows.
func (r *suggestRig) open(t *testing.T, text, pace string) typed {
	t.Helper()
	nTargets, nRefused := len(r.up.Targets()), len(r.up.Refused())
	r.b.input(t, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	time.Sleep(300 * time.Millisecond)
	tp := typed{text: text, start: time.Now()}
	r.b.inputWithin(t, "/input/type", fmt.Sprintf(`{"text": %q, "pace": %q}`, text, pace), 90*time.Second)
	time.Sleep(1500 * time.Millisecond)
	tp.enter = time.Now()
	r.b.input(t, "/input/key", `{"keys": "Return", "pace": "instant"}`)
	time.Sleep(2 * time.Second)
	for _, s := range r.since("suggest.test", tp.start) {
		if strings.HasPrefix(s.uri, "/suggest") {
			tp.suggests = append(tp.suggests, s)
		}
	}
	for _, s := range r.since("site.test", tp.start) {
		if s.at.Before(tp.enter) {
			tp.early = append(tp.early, s)
		}
	}
	tp.targets, tp.refused = r.up.Targets()[nTargets:], r.up.Refused()[nRefused:]
	return tp
}

// openAction opens text with the open action (PROJ-164), which clicks
// the address bar, types the address and presses Delete and Return in one
// call; Return is somewhere in it, so only requests before the call count
// as early, and opened looks from its start.
func (r *suggestRig) openAction(t *testing.T, text, pace string) typed {
	t.Helper()
	nTargets, nRefused := len(r.up.Targets()), len(r.up.Refused())
	tp := typed{text: text, start: time.Now()}
	tp.enter = tp.start
	r.b.inputWithin(t, "/input/open", fmt.Sprintf(`{"url": %q, "pace": %q}`, text, pace), 30*time.Second)
	time.Sleep(2 * time.Second)
	for _, s := range r.since("suggest.test", tp.start) {
		if strings.HasPrefix(s.uri, "/suggest") {
			tp.suggests = append(tp.suggests, s)
		}
	}
	tp.targets, tp.refused = r.up.Targets()[nTargets:], r.up.Refused()[nRefused:]
	return tp
}

// leaked is the longest start of secret that any of reqs carried.
func leaked(reqs []seen, secret string) string {
	for n := len(secret); n > 0; n-- {
		for _, s := range reqs {
			if strings.Contains(s.uri, secret[:n]) {
				return secret[:n]
			}
		}
	}
	return ""
}

// otherHosts lists the hosts in targets that aren't in allowed.
func otherHosts(targets []string, allowed ...string) []string {
	var out []string
	for _, tg := range targets {
		host, _, _ := net.SplitHostPort(tg)
		if !slices.Contains(allowed, host) && !slices.Contains(out, tg) {
			out = append(out, tg)
		}
	}
	return out
}

// TestTypedAddressReachesNoSuggestService is PROJ-191's acceptance test:
// an address with a token, typed into the address bar at either pace and
// opened, sends no request carrying the token to anything but its own
// host. Without the image's SearchSuggestEnabled policy, Chromium sends an
// http address's path and an address without a scheme to the suggest
// service as they are typed, token included (TestMeasureSuggest).
//
// First with a search engine the test reads: a plain word, which Chromium
// would send, gets no suggest request either, so the policy is what keeps
// it quiet; its Return lands on that engine's search page, so the engine
// really is in use. The isolated proxy reached nothing but site.test and
// suggest.test, and every other host it was asked for (Chromium's own
// background requests) was refused before it got a byte. Then with the
// image's own engine, DuckDuckGo: typing an address doesn't ask for its
// suggest host, and the address still opens.
func TestTypedAddressReachesNoSuggestService(t *testing.T) {
	image := testImage(t)
	forms := []string{
		"http://site.test/reset?token=%s",
		"http://site.test/reset/%s",
		"site.test/login/%s",
	}
	r := newSuggestRig(t, image, true)
	for _, pace := range []string{"instant", "human"} {
		tp := r.open(t, "pierword", pace)
		if len(tp.suggests) != 0 {
			t.Errorf("%s: a plain word sent %d suggest requests, the first %q; is SearchSuggestEnabled off?", pace, len(tp.suggests), tp.suggests[0].uri)
		}
		searched := false
		for _, s := range r.since("suggest.test", tp.enter) {
			searched = searched || strings.HasPrefix(s.uri, "/search?q=pierword")
		}
		if !searched {
			t.Fatalf("%s: Return on a plain word didn't search suggest.test, so the test search engine isn't in effect; the browser asked for %v", pace, tp.refused)
		}
		for i, form := range forms {
			secret := fmt.Sprintf("Zq%dx%dWv", i, time.Now().UnixNano()%1e6)
			text := fmt.Sprintf(form, secret)
			tp := r.open(t, text, pace)
			if len(tp.suggests) != 0 {
				t.Errorf("%s %s: %d suggest requests, carrying %q", pace, text, len(tp.suggests), leaked(tp.suggests, secret))
			}
			if got := leaked(r.since("suggest.test", tp.start), secret); got != "" {
				t.Errorf("%s %s: the search engine got %q of the secret", pace, text, got)
			}
			if other := otherHosts(tp.targets, "site.test"); len(other) != 0 {
				t.Errorf("%s %s: the browser reached %v as well as the address's own host", pace, text, other)
			}
			if len(tp.refused) != 0 {
				t.Logf("%s %s: refused, so nothing reached them: %v", pace, text, tp.refused)
			}
			if !r.opened(tp) {
				t.Errorf("%s %s: the address never opened as typed; site.test got %v after Return", pace, text, uris(r.since("site.test", tp.enter)))
			}
		}
		// The open action types into the same address bar. It takes only
		// http and https addresses.
		for i, form := range forms[:2] {
			secret := fmt.Sprintf("Zq%dx%dOp", i, time.Now().UnixNano()%1e6)
			text := fmt.Sprintf(form, secret)
			tp := r.openAction(t, text, pace)
			if len(tp.suggests) != 0 {
				t.Errorf("open %s %s: %d suggest requests, carrying %q", pace, text, len(tp.suggests), leaked(tp.suggests, secret))
			}
			if got := leaked(r.since("suggest.test", tp.start), secret); got != "" {
				t.Errorf("open %s %s: the search engine got %q of the secret", pace, text, got)
			}
			if other := otherHosts(tp.targets, "site.test"); len(other) != 0 {
				t.Errorf("open %s %s: the browser reached %v as well as the address's own host", pace, text, other)
			}
			if !r.opened(tp) {
				t.Errorf("open %s %s: the address never opened as typed; site.test got %v", pace, text, uris(r.since("site.test", tp.start)))
			}
		}
	}
	docker(t, "rm", "-f", r.b.name)

	d := newSuggestRig(t, image, false)
	for _, pace := range []string{"instant", "human"} {
		text := fmt.Sprintf("http://site.test/reset/Zq8x%dWv", time.Now().UnixNano()%1e6)
		tp := d.open(t, text, pace)
		for _, tg := range tp.refused {
			// DuckDuckGo's suggest service is duckduckgo.com/ac/. A new
			// profile's first tab also asks for start.duckduckgo.com
			// (PROJ-127), which isn't typing.
			if host, _, _ := net.SplitHostPort(tg); host == "duckduckgo.com" {
				t.Errorf("%s %s: the browser asked for %s while it was typed (all refused: %v)", pace, text, tg, tp.refused)
				break
			}
		}
		if !d.opened(tp) {
			t.Errorf("%s %s: the address never opened as typed, so the typing may not have reached the address bar; site.test got %v after Return", pace, text, uris(d.since("site.test", tp.enter)))
		}
	}

	// The policy shows on chrome://policy, to a person; a page can't read
	// it. That's Chromium's, with or without this policy: the check
	// documents ANTI-BOT.md's claim rather than guarding the change.
	pg := d.b.page()
	defer pg.Close()
	got := pg.Eval(`fetch('chrome://policy/').then(() => 'read', e => 'refused: ' + e.name)`)
	if got != "refused: TypeError" {
		t.Errorf("a page fetching chrome://policy got %v", got)
	}
}
