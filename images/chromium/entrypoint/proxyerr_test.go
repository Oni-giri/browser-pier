package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log the forwarder's goroutines write to.
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

// cutAfterTunnel is an HTTP proxy that opens every tunnel and then hangs
// up, so a lookup fails after the tunnel opened, on a connection to the
// proxy's own address.
func cutAfterTunnel(t *testing.T) string {
	t.Helper()
	return statusProxy(t, "200 Connection established")
}

// TestProxyErrorsHoldNoProxyText (PROJ-205): a failure reaching or going
// through the proxy is told in BrowserPier's words, in a start's error,
// its fallback warning and the forwarder's log alike, which reach agents
// through a session's error and last_log: never the proxy's host, port,
// address, credentials or reason phrase.
func TestProxyErrorsHoldNoProxyText(t *testing.T) {
	was := lookupTimeout
	lookupTimeout = 2 * time.Second
	t.Cleanup(func() { lookupTimeout = was })
	const user, pass, banner = "acct-9f3k2", "pw-7q2x", "HiddenBanner-acct"
	creds := user + ":" + pass + "@"

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	// Its certificate is for example.com and 127.0.0.1, not localhost.
	badTLS := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(badTLS.Close)
	_, tlsPort, _ := net.SplitHostPort(badTLS.Listener.Addr().String())

	cases := []struct{ name, proxy, kind string }{
		{"unresolvable", "http://" + creds + "hidden-proxy.invalid:31877", proxyUnreachable},
		{"refused", "socks5://" + creds + closedAddr, proxyUnreachable},
		{"bad TLS", "https://" + creds + "localhost:" + tlsPort, proxyTLS},
		{"407 with a banner", "http://" + creds + statusProxy(t, "407 "+banner), proxyAuth},
		{"403 with a banner", "http://" + creds + statusProxy(t, "403 "+banner), proxyRefused},
		{"cut after the tunnel", "http://" + creds + cutAfterTunnel(t), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.proxy)
			if err != nil {
				t.Fatal(err)
			}
			secret := []string{user, pass, banner, u.Host, u.Port(), "hidden-proxy", "localhost", "127.0.0.1"}
			if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
				secret = append(secret, h)
			}
			clean := func(where, text string) {
				t.Helper()
				for _, s := range secret {
					if s != "" && strings.Contains(text, s) {
						t.Errorf("%s holds %q: %s", where, s, text)
					}
				}
			}

			log := &lockedBuffer{}
			f, err := newForwarder(u, egressPolicy{}, slog.New(slog.NewTextHandler(log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go f.serve(ln)

			// A start's lookup: with nothing stored it fails the start,
			// with a stored zone it warns.
			lookup := func() (string, string, string, error) {
				return lookupGeo(context.Background(), lookupClient(f), "http://geo.test:8080/geo")
			}
			cfg := config{GeoURL: "http://geo.test:8080/geo"}
			_, err = decideGeo(cfg, true, lookup)
			if err == nil {
				t.Fatal("the start went ahead")
			}
			clean("the start's error", err.Error())
			if c.kind != "" && checkReason(err, false) != c.kind {
				t.Errorf("kind %q, want %q (%v)", checkReason(err, false), c.kind, err)
			}
			cfg.StoredCountry, cfg.StoredTZ = "DE", "Europe/Berlin"
			g, err := decideGeo(cfg, true, lookup)
			if err != nil || g.Warning == "" {
				t.Fatalf("with a stored zone: %+v, %v", g, err)
			}
			clean("the warning", g.Warning)

			// A page's connection, as Chromium makes it.
			if c.kind != "" {
				if _, err := socksGet(t, ln.Addr().String(), "geo.test", 80); err == nil {
					t.Fatal("the page got through")
				}
				deadline := time.Now().Add(5 * time.Second)
				for !strings.Contains(log.String(), "upstream refused") && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				out := log.String()
				if !strings.Contains(out, "kind="+c.kind) {
					t.Errorf("the forwarder's log: %s", out)
				}
				clean("the forwarder's log", out)
			}
			t.Logf("error: %v; warning: %s", err, g.Warning)
		})
	}
}
