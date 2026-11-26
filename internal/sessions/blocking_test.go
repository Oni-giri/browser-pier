package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBlocker stands in for the forwarder's controls on the control socket.
type fakeBlocker struct {
	mu     sync.Mutex
	off    int
	exempt []string
	full   bool // answer exemptions as past the cap
}

func (b *fakeBlocker) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /proxy/blocking/off", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		b.off++
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /proxy/exempt", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Host string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.full {
			http.Error(w, "the session exempts no more hosts from the ad and tracker list", http.StatusConflict)
			return
		}
		b.exempt = append(b.exempt, in.Host)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /proxy/stats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"blocked": 7, "refused": 1}`))
	})
	return mux
}

func TestBlockListHandover(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list := []byte("# browserpier domain set 1\nads.example.com\n")
	var have []byte
	f.m.SetBlocklist(func() []byte { return have })
	fb := &fakeBlocker{}
	f.rt.Control = fb.mux()
	path := filepath.Join(f.dir, "profiles", "google", "run", blockFile)
	env := func(s Session) string {
		c, _ := f.rt.Container(s.ContainerID)
		return strings.Join(c.Spec.Env, "\n")
	}
	stop := func(s Session) {
		t.Helper()
		if _, err := f.m.Stop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
	}

	// No list downloaded yet: the browser starts without one, and the
	// session says it blocks nothing, and why (PROJ-119 review).
	s := f.start(f.m)
	if s.BlockAds || !s.AdListMissing || !strings.Contains(env(s), "BROWSERPIER_BLOCK_ADS=false") {
		t.Fatalf("without a list: block_ads %v, list missing %v, env %s", s.BlockAds, s.AdListMissing, env(s))
	}
	if f.stored(s.ID).BlockAds {
		t.Fatal("stored as blocking without a list")
	}
	// An agent attaching with AllowAds: off by its choice now.
	if s2, _, err := f.m.StartWith(ctx, "google", StartOptions{AllowAds: true}); err != nil || s2.BlockAds || s2.AdListMissing || fb.off != 0 {
		t.Fatalf("attach with AllowAds without a list: %+v, %v, off called %d times", s2, err, fb.off)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a list file without a list")
	}
	stop(s)

	// With one: the file waits in the control folder, read-only, and stays.
	have = list
	s = f.start(f.m)
	if !s.BlockAds || s.AdListMissing || !strings.Contains(env(s), "BROWSERPIER_BLOCK_ADS=true") {
		t.Fatalf("block_ads %v, env %s", s.BlockAds, env(s))
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o444 {
		t.Fatalf("list file: %v, %v", st, err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(list) {
		t.Fatalf("list file holds %q", b)
	}
	if n, ok := f.m.Blocked(ctx, "google"); !ok || n != 7 {
		t.Errorf("Blocked = %d, %v", n, ok)
	}
	if err := f.m.ExemptHost(ctx, s.ID, "ads.example.com"); err != nil {
		t.Fatal(err)
	}
	// An address's host, in its ASCII form; one with no host is passed over.
	if err := f.m.ExemptURL(ctx, s.ID, "https://Bücher.example:8443/x?q=1"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ExemptURL(ctx, s.ID, "about:blank"); err != nil || !slices.Equal(fb.exempt, []string{"ads.example.com", "xn--bcher-kva.example"}) {
		t.Fatalf("exempt: %v, %v", fb.exempt, err)
	}
	fb.exempt = fb.exempt[:1]

	// An agent turns it off for the running session: the browser is told,
	// the session says so, the profile doesn't change.
	s2, attached, err := f.m.StartWith(ctx, "google", StartOptions{AllowAds: true})
	if err != nil || !attached || s2.ID != s.ID || s2.BlockAds {
		t.Fatalf("attach with AllowAds: %+v, %v, %v", s2, attached, err)
	}
	if fb.off != 1 || f.stored(s.ID).BlockAds {
		t.Fatalf("off called %d times; stored %+v", fb.off, f.stored(s.ID))
	}
	if p, _ := f.m.Profiles().Get(ctx, "google"); !p.BlockAds {
		t.Fatal("turning it off for a session changed the profile")
	}
	// It can't be turned back on, and an exemption is no longer needed.
	if s3, _, _ := f.m.Start(ctx, "google", ""); s3.BlockAds {
		t.Fatal("blocking came back on")
	}
	if err := f.m.ExemptHost(ctx, s.ID, "other.example.com"); err != nil || !slices.Equal(fb.exempt, []string{"ads.example.com"}) {
		t.Fatalf("exempt: %v, %v", fb.exempt, err)
	}
	stop(s)

	// A new session started with AllowAds: no file, nothing blocked.
	s, _, err = f.m.StartWith(ctx, "google", StartOptions{AllowAds: true})
	if err != nil || s.BlockAds || !strings.Contains(env(s), "BROWSERPIER_BLOCK_ADS=false") {
		t.Fatalf("new session with AllowAds: %+v, %v, env %s", s, err, env(s))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the last list was left for a session that doesn't block")
	}
	stop(s)

	// A profile with blocking off never gets one.
	p, _ := f.m.Profiles().Get(ctx, "google")
	p.BlockAds = false
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	s = f.start(f.m)
	if s.BlockAds || !strings.Contains(env(s), "BROWSERPIER_BLOCK_ADS=false") {
		t.Fatalf("profile off: %+v, env %s", s, env(s))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a list for a profile that doesn't block")
	}
}

// ExemptHost never asks the browser to exempt a public suffix, which
// would let every site under it through, and logs a warning, once, when
// the browser's exemptions are full.
func TestExemptHostRefusesPublicSuffixesAndWarnsWhenFull(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f.m.SetBlocklist(func() []byte { return []byte("# browserpier domain set 1\nads.example.com\n") })
	fb := &fakeBlocker{}
	f.rt.Control = fb.mux()
	var logs bytes.Buffer
	f.m.log = slog.New(slog.NewTextHandler(&logs, nil))
	s := f.start(f.m)
	for _, host := range []string{"co.uk", "github.io", "localhost", "1.2.3.4"} {
		if err := f.m.ExemptHost(ctx, s.ID, host); err == nil {
			t.Errorf("%s exempted", host)
		}
	}
	if err := f.m.ExemptHost(ctx, s.ID, "Ads.Example.COM."); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	got := slices.Clone(fb.exempt)
	fb.full = true
	fb.mu.Unlock()
	if !slices.Equal(got, []string{"ads.example.com"}) {
		t.Fatalf("the browser was asked to exempt %q", got)
	}
	for range 3 {
		if err := f.m.ExemptHost(ctx, s.ID, "more.example.com"); err == nil {
			t.Error("an exemption past the cap succeeded")
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "exempts no more hosts") {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
}
