package main

import (
	"bufio"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// socksGet does what Chromium does: a SOCKS5 CONNECT to the forwarder by
// host name, then an HTTP request over it.
func socksGet(t *testing.T, fwdAddr, host string, port int) (string, error) {
	t.Helper()
	c, err := net.Dial("tcp", fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte{0x05, 0x01, 0x00})
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		return "", err
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port)) //nolint:gosec // test port
	_, _ = c.Write(req)
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		return "", err
	}
	if reply[1] != 0x00 {
		return "", io.ErrUnexpectedEOF
	}
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func startForwarder(t *testing.T, upstream string, policy egressPolicy) *forwarder {
	t.Helper()
	var u *url.URL
	if upstream != "" {
		u, _ = url.Parse(upstream)
	}
	f, err := newForwarder(u, policy, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve(ln)
	fwdAddrs[f] = ln.Addr().String()
	return f
}

var fwdAddrs = map[*forwarder]string{}

func TestForwarderThroughUpstreams(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }))
	defer origin.Close()
	port, _ := strconv.Atoi(strings.Split(origin.Listener.Addr().String(), ":")[1])

	for name, mk := range map[string]func(*testing.T, string, string) *browsertest.Proxy{
		"http":   browsertest.NewConnectProxy,
		"socks5": browsertest.NewSOCKS5Proxy,
	} {
		t.Run(name, func(t *testing.T) {
			up := mk(t, "user", "p@ss:word")
			up.Alias("origin.test", "127.0.0.1")
			f := startForwarder(t, name+"://user:"+url.QueryEscape("p@ss:word")+"@"+up.Addr, egressPolicy{})
			body, err := socksGet(t, fwdAddrs[f], "origin.test", port)
			if err != nil || body != "hello" {
				t.Fatalf("through the forwarder: %q, %v", body, err)
			}
			// The upstream got the name, not an address: no local DNS.
			if got := up.Targets(); len(got) != 1 || got[0] != "origin.test:"+strconv.Itoa(port) {
				t.Fatalf("upstream saw %v", got)
			}
			deadline := time.Now().Add(5 * time.Second)
			for f.up.Load() == 0 || f.down.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("traffic not counted: %d up, %d down", f.up.Load(), f.down.Load())
				}
				time.Sleep(10 * time.Millisecond)
			}

			bad := startForwarder(t, name+"://user:wrong@"+up.Addr, egressPolicy{})
			if _, err := socksGet(t, fwdAddrs[bad], "origin.test", port); err == nil {
				t.Fatal("wrong upstream credentials still got through")
			}
			if up.Denied() == 0 {
				t.Fatal("the upstream never saw the wrong credentials")
			}
		})
	}
}

func TestReadProxyFileDeletesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	if u, err := readProxyFile(path); err != nil || u != nil {
		t.Fatalf("no file: %v, %v", u, err)
	}
	if err := os.WriteFile(path, []byte(`{"url": "socks5://u:secret@proxy.example:1080"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := readProxyFile(path)
	if err != nil || u.Host != "proxy.example:1080" {
		t.Fatalf("readProxyFile = %v, %v", u, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the credentials file is still there")
	}
	for _, bad := range []string{"ftp://x:1", "http://noport"} {
		u, _ := url.Parse(bad)
		if _, err := newForwarder(u, egressPolicy{}, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("newForwarder(%s) accepted", bad)
		}
	}
}

func TestChromiumAlwaysGoesThroughTheForwarder(t *testing.T) {
	args := strings.Join(chromiumArgs(config{Width: 1280, Height: 800, ProfileDir: "/p"}, false), " ")
	for _, want := range []string{
		"--proxy-server=socks5://127.0.0.1:1080",
		"--proxy-bypass-list=<-loopback>",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s: %s", want, args)
		}
	}
}
