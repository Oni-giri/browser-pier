package blocklist

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

// listServer serves one list, with an ETag, answering 304 to a matching
// If-None-Match, or fails while down.
type listServer struct {
	*httptest.Server
	mu          sync.Mutex
	body, etag  string
	down        bool
	requests    int
	conditional int
}

func newListServer(t *testing.T, body string) *listServer {
	s := &listServer{body: body, etag: `"v1"`}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests++
		if s.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			s.conditional++
			if r.Header.Get("If-None-Match") == s.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("ETag", s.etag)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *listServer) set(f func(s *listServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newLists(t *testing.T, dir string, urls ...string) (*Lists, *clock, *logs) {
	t.Helper()
	lg := &logs{}
	l, err := New(dir, urls, slog.New(slog.NewTextHandler(lg, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	l.now = c.now
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	return l, c, lg
}

type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestConditionalRefresh(t *testing.T) {
	srv := newListServer(t, "0.0.0.0 ads.example.com\n")
	dir := t.TempDir()
	l, clk, _ := newLists(t, dir, srv.URL)
	ctx := context.Background()

	l.Refresh(ctx, false)
	if !l.Set().Matches("x.ads.example.com") {
		t.Fatal("the downloaded list isn't in the set")
	}
	// Not due yet: nothing is asked.
	clk.t = clk.t.Add(time.Hour)
	l.Refresh(ctx, false)
	if srv.requests != 1 {
		t.Fatalf("%d requests before the list was due", srv.requests)
	}
	// Due: a conditional request; a 304 keeps the set.
	clk.t = clk.t.Add(RefreshEvery)
	l.Refresh(ctx, false)
	if srv.requests != 2 || srv.conditional != 1 {
		t.Fatalf("requests %d, conditional %d", srv.requests, srv.conditional)
	}
	if !l.Set().Matches("ads.example.com") {
		t.Fatal("a 304 lost the set")
	}
	// The 304 counts as fresh: not asked again for RefreshEvery.
	clk.t = clk.t.Add(2 * time.Hour)
	l.Refresh(ctx, false)
	if srv.requests != 2 {
		t.Fatalf("asked again %d times after a 304", srv.requests-2)
	}
	// A new version replaces the set.
	srv.set(func(s *listServer) { s.body, s.etag = "||tracker.example.org^\n", `"v2"` })
	clk.t = clk.t.Add(RefreshEvery)
	l.Refresh(ctx, false)
	if l.Set().Matches("ads.example.com") || !l.Set().Matches("tracker.example.org") {
		t.Fatalf("set after a new version: %v", l.Set().Names())
	}
	// A restart reads the list from disk, without the network.
	srv.set(func(s *listServer) { s.down = true })
	l2, _, _ := newLists(t, dir, srv.URL)
	if !l2.Set().Matches("tracker.example.org") {
		t.Fatal("a restart lost the downloaded list")
	}
}

func TestFailedDownloadKeepsTheLastSet(t *testing.T) {
	srv := newListServer(t, "0.0.0.0 ads.example.com\n")
	l, clk, lg := newLists(t, t.TempDir(), srv.URL)
	ctx := context.Background()
	l.Refresh(ctx, false)

	for name, broken := range map[string]func(s *listServer){
		"server error": func(s *listServer) { s.down = true },
		"no names":     func(s *listServer) { s.down, s.body, s.etag = false, "<html>an error page</html>\n", `"v3"` },
		"too big":      func(s *listServer) { s.body, s.etag = strings.Repeat("# x\n", maxListBytes/4+1), `"v4"` },
	} {
		srv.set(broken)
		clk.t = clk.t.Add(RefreshEvery)
		l.Refresh(ctx, false)
		if !l.Set().Matches("ads.example.com") {
			t.Fatalf("%s: lost the last good set", name)
		}
	}
	if !strings.Contains(lg.String(), "keeping the last one") {
		t.Errorf("no warning logged: %s", lg)
	}
	// A failed list is tried again an hour later, not at every check.
	srv.set(func(s *listServer) { s.down = true })
	clk.t = clk.t.Add(RefreshEvery)
	l.Refresh(ctx, false)
	n := srv.requests
	clk.t = clk.t.Add(10 * time.Minute)
	l.Refresh(ctx, false)
	if srv.requests != n {
		t.Error("a failed list was tried again within the hour")
	}
	clk.t = clk.t.Add(time.Hour)
	l.Refresh(ctx, false)
	if srv.requests != n+1 {
		t.Error("a failed list wasn't tried again after an hour")
	}
}

func TestFirstStartWithoutNetwork(t *testing.T) {
	srv := newListServer(t, "")
	srv.set(func(s *listServer) { s.down = true })
	l, _, lg := newLists(t, t.TempDir(), srv.URL, "http://127.0.0.1:1/nothing-listens-here")
	l.Refresh(context.Background(), false)
	if l.Set().Len() != 0 {
		t.Fatal("a set out of nothing")
	}
	if f := l.File(); f != nil {
		t.Fatalf("a file for browsers without any list: %q", f)
	}
	_ = l.File()
	if n := strings.Count(lg.String(), "browsers start without blocking"); n != 1 {
		t.Errorf("the missing-list warning was logged %d times, want once: %s", n, lg)
	}
}

func TestSeveralListsCompileInOrder(t *testing.T) {
	a := newListServer(t, "0.0.0.0 ads.example.com\n0.0.0.0 shared.example.com\n")
	b := newListServer(t, "||tracker.example.org^\n||shared.example.com^\n")
	l, _, _ := newLists(t, t.TempDir(), a.URL, b.URL)
	l.Refresh(context.Background(), false)
	if got := strings.Join(l.Set().Names(), " "); got != "ads.example.com shared.example.com tracker.example.org" {
		t.Errorf("set %q", got)
	}
	st := l.Stats()
	if st.Names != 4 || st.Lines != 4 {
		t.Errorf("stats %+v", st)
	}
	// The file a browser gets reads back to the same set.
	s, err := domainset.Read(bytes.NewReader(l.File()))
	if err != nil || s.Len() != 3 {
		t.Errorf("file: %v, %d names", err, s.Len())
	}
}

func TestParseURLs(t *testing.T) {
	if got := ParseURLs("", false); len(got) != len(DefaultURLs) {
		t.Errorf("unset: %q", got)
	}
	if got := ParseURLs("none", true); got != nil {
		t.Errorf("none: %q", got)
	}
	if got := ParseURLs(" https://a.example/x, https://b.example/y ", true); len(got) != 2 || got[1] != "https://b.example/y" {
		t.Errorf("two: %q", got)
	}
	if _, err := New(t.TempDir(), []string{"file:///etc/hosts"}, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("a file: URL was taken")
	}
}
