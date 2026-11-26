package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
)

// lockedBuffer is a log destination the manager's goroutines share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// proxyFileCatcher serves the fake's control socket and keeps the proxy
// file as it was when the browser came up, as the entrypoint reads it.
func proxyFileCatcher(path string) (http.Handler, func() string) {
	var mu sync.Mutex
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		if b, err := os.ReadFile(path); err == nil {
			seen = string(b)
		}
		mu.Unlock()
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux, func() string {
		mu.Lock()
		defer mu.Unlock()
		s := seen
		seen = ""
		return s
	}
}

// TestStickyProxyIsFilledInAtStart: run/proxy.json holds the template
// filled in with the profile's own token, the same one at every start
// until a new exit; the stored proxy stays the template; and neither
// the log nor a failed session says the token.
func TestStickyProxyIsFilledInAtStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A manager whose log is kept, at every level.
	var logs lockedBuffer
	d, err := db.Open(ctx, filepath.Join(f.dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(d, f.rt, f.layout, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); _ = d.Close() })

	const template = "http://pier-{sticky}:pw-{sticky}@proxy.example:8080"
	p, _ := m.Profiles().Get(ctx, "google")
	p.Proxy = template
	if _, err := m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	h, seen := proxyFileCatcher(filepath.Join(f.dir, "profiles", "google", "run", proxyFile))
	f.rt.Control = h

	startStop := func() string {
		t.Helper()
		s := f.start(m)
		got := seen()
		if _, err := m.Stop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		var file struct{ URL string }
		if err := json.Unmarshal([]byte(got), &file); err != nil {
			t.Fatalf("proxy file %q: %v", got, err)
		}
		return file.URL
	}
	first := startStop()
	tok, _ := m.Profiles().StickyToken(ctx, "google")
	if want := "http://pier-" + tok + ":pw-" + tok + "@proxy.example:8080"; first != want || strings.ContainsAny(first, "{}") {
		t.Fatalf("proxy file holds %q, want %q", first, want)
	}
	if !regexp.MustCompile(`^[a-z0-9]{8}$`).MatchString(tok) {
		t.Fatalf("token %q", tok)
	}
	if got, _ := m.Profiles().Get(ctx, "google"); got.Proxy != template {
		t.Fatalf("the stored proxy became %q", got.Proxy)
	}
	if again := startStop(); again != first {
		t.Fatalf("the second start used %q, the first %q", again, first)
	}

	if err := m.Profiles().NewExit(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	after := startStop()
	newTok, _ := m.Profiles().StickyToken(ctx, "google")
	if after == first || after != "http://pier-"+newTok+":pw-"+newTok+"@proxy.example:8080" {
		t.Fatalf("after a new exit the proxy file holds %q (before: %q)", after, first)
	}

	// A start that fails: its error and last log don't hold the token.
	f.rt.StartErrs = []error{errors.New("no luck")}
	s, _, startErr := m.Start(ctx, "google", "")
	if !errors.Is(startErr, ErrStartFailed) {
		t.Fatalf("Start = %+v, %v; want it to fail", s, startErr)
	}
	stored, err := getSession(ctx, d, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{s.Error, s.LastLog, stored.Error, stored.LastLog, startErr.Error(), logs.String()} {
		for _, tk := range []string{tok, newTok} {
			if strings.Contains(text, tk) {
				t.Errorf("the token %q shows in %q", tk, text)
			}
		}
		if strings.Contains(text, "pw-") {
			t.Errorf("the password shows in %q", text)
		}
	}
	if logs.String() == "" {
		t.Fatal("nothing was logged: the check above proves nothing")
	}
}

// TestPlainProxyFileIsUnchanged: without the placeholder, run/proxy.json
// is byte for byte what it always was, and no token is asked for.
func TestPlainProxyFileIsUnchanged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, _ := f.m.Profiles().Get(ctx, "google")
	p.Proxy = "socks5://us%7Ber:p%40ss&x@proxy.example:1080"
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	h, seen := proxyFileCatcher(filepath.Join(f.dir, "profiles", "google", "run", proxyFile))
	f.rt.Control = h
	s := f.start(f.m)
	want, _ := json.Marshal(map[string]string{"url": p.Proxy})
	if got := seen(); got != string(want) {
		t.Fatalf("proxy file %q, want %q", got, want)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := f.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
}
