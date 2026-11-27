package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
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

// resetAfterTunnel is an HTTP proxy that opens every tunnel, reads the
// start of the request sent through it, writes then (if anything) and
// resets the connection: a lookup fails after the tunnel opened, with an
// error from a connection to the proxy's own address
// ("read tcp …->127.0.0.1:port: connection reset by peer").
func resetAfterTunnel(t *testing.T, then string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					_ = c.Close()
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				if _, err := br.ReadByte(); err != nil {
					_ = c.Close()
					return
				}
				_, _ = io.WriteString(c, then)
				time.Sleep(50 * time.Millisecond)
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// silentProxy accepts connections and never says a word.
func silentProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

// TestProxyErrorsHoldNoProxyText (PROJ-205): a failure reaching or going
// through the proxy is told in BrowserPier's words, in a start's error,
// its fallback warning and the forwarder's log alike, which reach agents
// through a session's error and last_log: never the proxy's host, port,
// address, credentials or reason phrase.
func TestProxyErrorsHoldNoProxyText(t *testing.T) {
	was := lookupTimeout
	lookupTimeout = time.Second
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

	socksUp := browsertest.NewSOCKS5Proxy(t, user, "the-right-one")

	// kind "" is a failure after the tunnel opened, which the forwarder
	// doesn't see; slow is a proxy that never answers, which a page's
	// connection would wait 30 s for.
	cases := []struct {
		name, proxy, kind string
		slow              bool
	}{
		{"unresolvable", "http://" + creds + "hidden-proxy.invalid:31877", proxyUnreachable, false},
		{"refused", "socks5://" + creds + closedAddr, proxyUnreachable, false},
		{"bad TLS", "https://" + creds + "localhost:" + tlsPort, proxyTLS, false},
		{"407 with a banner", "http://" + creds + statusProxy(t, "407 "+banner), proxyAuth, false},
		{"403 with a banner", "http://" + creds + statusProxy(t, "403 "+banner), proxyRefused, false},
		{"SOCKS5 refuses the credentials", "socks5://" + creds + socksUp.Addr, proxyAuth, false},
		{"never answers", "http://" + creds + silentProxy(t), proxyTimeout, true},
		{"reset after the tunnel", "http://" + creds + resetAfterTunnel(t, ""), "", false},
		{"reset in the middle of the answer", "http://" + creds + resetAfterTunnel(t,
			"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 400\r\n\r\n{\"ip\": \"203.0.113.7\", \"coun"), "", false},
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
			lookup := startLookup(context.Background(), f, "http://geo.test:8080/geo")
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
			if c.kind != "" && !c.slow {
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
