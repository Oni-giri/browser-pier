package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

const (
	checkUser = "chkuser-7f3a"
	checkPass = "s3cret-pw-91"
)

// geoEndpoint is a fake echoip endpoint, reached through the proxy as
// geo.test.
func geoEndpoint(t *testing.T) (port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/geo":
			_, _ = io.WriteString(w, `{"ip":"198.51.100.23","country_iso":"DE","time_zone":"Europe/Berlin"}`)
		case "/notjson":
			_, _ = io.WriteString(w, `<html>hello</html>`)
		case "/nocountry":
			_, _ = io.WriteString(w, `{"ip":"198.51.100.23","time_zone":"Europe/Berlin"}`)
		case "/slow":
			// Answers after the lookup has given up.
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			_, _ = io.WriteString(w, `{"ip":"198.51.100.23","country_iso":"DE","time_zone":"Europe/Berlin"}`)
		case "/badip":
			_, _ = io.WriteString(w, `{"ip":"<b>not an address</b>","country_iso":"DE","time_zone":"Europe/Berlin"}`)
		case "/zoneip":
			_, _ = io.WriteString(w, `{"ip":"fe80::1%eth0","country_iso":"DE","time_zone":"Europe/Berlin"}`)
		case "/local":
			_, _ = io.WriteString(w, `{"ip":"198.51.100.23","country_iso":"DE","time_zone":"Local"}`)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ = net.SplitHostPort(srv.Listener.Addr().String())
	return port
}

// runCheck runs the check as the container does, with proxyURL handed
// over in a proxy file, and returns its result and its whole output,
// stdout and stderr. It checks the file is gone and nothing was started.
func runCheck(t *testing.T, proxyURL, geoURL string) (checkResult, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "proxy.json")
	b, _ := json.Marshal(map[string]string{"url": proxyURL})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSERPIER_PROXY_FILE", file)
	t.Setenv("BROWSERPIER_GEOIP_URL", geoURL)
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stderr
	os.Stderr = stderr
	starts := procs.starts.Load()
	var out bytes.Buffer
	code := runProxyCheck(&out)
	os.Stderr = was
	errOut, _ := os.ReadFile(stderr.Name())
	if procs.starts.Load() != starts {
		t.Fatal("the check started a process: it must never start Xvfb or Chromium")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("the proxy file is still there after the check")
	}
	all := out.String() + string(errOut)
	for _, secret := range []string{proxyURL, checkUser, checkPass, url.QueryEscape(checkPass)} {
		if strings.Contains(all, secret) {
			t.Fatalf("the check's output holds %q: %s", secret, all)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var res checkResult
	dec := json.NewDecoder(strings.NewReader(lines[len(lines)-1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("last line %q: %v", lines[len(lines)-1], err)
	}
	if (code == 0) != res.OK {
		t.Fatalf("exit code %d for %+v", code, res)
	}
	return res, all
}

func TestProxyCheckFindsTheExit(t *testing.T) {
	port := geoEndpoint(t)
	for name, mk := range map[string]func(*testing.T, string, string) *browsertest.Proxy{
		"http":   browsertest.NewConnectProxy,
		"socks5": browsertest.NewSOCKS5Proxy,
	} {
		t.Run(name, func(t *testing.T) {
			up := mk(t, checkUser, checkPass)
			up.Alias("geo.test", "127.0.0.1")
			res, _ := runCheck(t, name+"://"+checkUser+":"+checkPass+"@"+up.Addr, "http://geo.test:"+port+"/geo")
			want := checkResult{OK: true, IP: "198.51.100.23", Country: "DE", TimeZone: "Europe/Berlin", Locale: "de-DE", Accept: []string{"de-DE", "de", "en-US", "en"}}
			if res.MS <= 0 || res.MS > lookupTimeout.Milliseconds() {
				t.Errorf("ms = %d", res.MS)
			}
			res.MS = 0
			if got, _ := json.Marshal(res); string(got) != string(must(json.Marshal(want))) {
				t.Fatalf("result %s, want %s", got, must(json.Marshal(want)))
			}
			if got := up.Targets(); len(got) != 1 || got[0] != "geo.test:"+port {
				t.Fatalf("the proxy was asked for %v; want the lookup host only, by name", got)
			}
		})
	}
}

// TestProxyCheckFollowsNoRedirect: the check contacts the lookup URL and
// nothing else; a redirect is a lookup with no usable answer.
func TestProxyCheckFollowsNoRedirect(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		_, _ = io.WriteString(w, `{"ip":"198.51.100.23","country_iso":"DE","time_zone":"Europe/Berlin"}`)
	}))
	t.Cleanup(other.Close)
	_, otherPort, _ := net.SplitHostPort(other.Listener.Addr().String())
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.test:"+otherPort+"/geo", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	_, port, _ := net.SplitHostPort(redirect.Listener.Addr().String())
	up := browsertest.NewConnectProxy(t, checkUser, checkPass)
	up.Alias("geo.test", "127.0.0.1")
	up.Alias("other.test", "127.0.0.1")
	res, _ := runCheck(t, "http://"+checkUser+":"+checkPass+"@"+up.Addr, "http://geo.test:"+port+"/json")
	if res.OK || res.Reason != checkLookup {
		t.Fatalf("got %+v, want reason %s", res, checkLookup)
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("the redirect's host got %d requests", n)
	}
	if got := up.Targets(); len(got) != 1 || got[0] != "geo.test:"+port {
		t.Fatalf("the proxy was asked for %v; want the lookup host only", got)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// statusProxy answers every CONNECT with status, a whole status line's
// rest (code and reason phrase).
func statusProxy(t *testing.T, status string) string {
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
				defer func() { _ = c.Close() }()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 "+status+"\r\nContent-Length: 0\r\n\r\n")
			}()
		}
	}()
	return ln.Addr().String()
}

// socksCodeProxy is a SOCKS5 proxy that takes any credentials and then
// refuses every CONNECT with code.
func socksCodeProxy(t *testing.T, code byte) string {
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
				defer func() { _ = c.Close() }()
				h := make([]byte, 2)
				_, _ = io.ReadFull(c, h)
				_, _ = io.ReadFull(c, make([]byte, h[1]))
				_, _ = c.Write([]byte{0x05, 0x02})
				v := make([]byte, 2)
				_, _ = io.ReadFull(c, v)
				_, _ = io.ReadFull(c, make([]byte, v[1]))
				l := make([]byte, 1)
				_, _ = io.ReadFull(c, l)
				_, _ = io.ReadFull(c, make([]byte, l[0]))
				_, _ = c.Write([]byte{0x01, 0x00})
				req := make([]byte, 5)
				_, _ = io.ReadFull(c, req)
				_, _ = io.ReadFull(c, make([]byte, int(req[4])+2))
				_, _ = c.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}()
		}
	}()
	return ln.Addr().String()
}

func TestProxyCheckReasons(t *testing.T) {
	was := lookupTimeout
	lookupTimeout = 500 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = was })
	port := geoEndpoint(t)
	geo := "http://geo.test:" + port
	creds := checkUser + ":" + checkPass + "@"

	httpUp := browsertest.NewConnectProxy(t, checkUser, checkPass)
	httpUp.Alias("geo.test", "127.0.0.1")
	socksUp := browsertest.NewSOCKS5Proxy(t, checkUser, checkPass)
	socksUp.Alias("geo.test", "127.0.0.1")

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	// Accepts, and never says a word.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		var held []net.Conn
		for {
			c, err := silent.Accept()
			if err != nil {
				for _, c := range held {
					_ = c.Close()
				}
				return
			}
			held = append(held, c)
		}
	}()

	badTLS := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(badTLS.Close)

	cases := []struct {
		name, proxy, geo, want string
	}{
		{"http wrong password", "http://" + checkUser + ":wrong@" + httpUp.Addr, geo + "/geo", proxyAuth},
		{"socks5 wrong password", "socks5://" + checkUser + ":wrong@" + socksUp.Addr, geo + "/geo", proxyAuth},
		{"closed port", "http://" + creds + closedAddr, geo + "/geo", proxyUnreachable},
		{"unresolvable name", "http://" + creds + "proxy.invalid:8080", geo + "/geo", proxyUnreachable},
		{"never answers", "http://" + creds + silent.Addr().String(), geo + "/geo", proxyTimeout},
		{"CONNECT 403", "http://" + creds + statusProxy(t, "403 Forbidden"), geo + "/geo", proxyRefused},
		{"CONNECT 502", "http://" + creds + statusProxy(t, "502 Bad Gateway"), geo + "/geo", proxyRefused},
		{"SOCKS5 code 5", "socks5://" + creds + socksCodeProxy(t, 5), geo + "/geo", proxyRefused},
		{"https proxy, bad certificate", "https://" + creds + badTLS.Listener.Addr().String(), geo + "/geo", proxyTLS},
		{"endpoint 500", "http://" + creds + httpUp.Addr, geo + "/500", checkLookup},
		{"endpoint not JSON", "http://" + creds + httpUp.Addr, geo + "/notjson", checkLookup},
		{"endpoint without a country", "http://" + creds + httpUp.Addr, geo + "/nocountry", checkLookup},
		{"endpoint with time zone Local", "http://" + creds + httpUp.Addr, geo + "/local", checkLookup},
		{"endpoint gives no address", "http://" + creds + httpUp.Addr, geo + "/badip", checkLookup},
		{"endpoint gives an address with a zone", "http://" + creds + httpUp.Addr, geo + "/zoneip", checkLookup},
		{"tunnel opens, endpoint too slow", "http://" + creds + httpUp.Addr, geo + "/slow", checkLookup},
		{"tunnel opens over socks5, endpoint too slow", "socks5://" + creds + socksUp.Addr, geo + "/slow", checkLookup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _ := runCheck(t, c.proxy, c.geo)
			if res.OK || res.Reason != c.want {
				t.Fatalf("got %+v, want reason %s", res, c.want)
			}
			if res.IP != "" || res.Country != "" || res.MS != 0 {
				t.Fatalf("a failure carries only its reason: %+v", res)
			}
		})
	}
}

// TestProxyCheckLookupOff: with the lookup off nothing is contacted, and
// the proxy file is gone all the same.
func TestProxyCheckLookupOff(t *testing.T) {
	up := browsertest.NewConnectProxy(t, checkUser, checkPass)
	res, _ := runCheck(t, "http://"+checkUser+":"+checkPass+"@"+up.Addr, "")
	if res.OK || res.Reason != checkLookupOff {
		t.Fatalf("got %+v", res)
	}
	if len(up.Targets()) != 0 || up.Denied() != 0 {
		t.Fatal("the proxy was contacted with the lookup off")
	}
}

// TestProxyCheckHidesTheProxysWords: a proxy's reason phrase is its own
// text, never the check's output.
func TestProxyCheckHidesTheProxysWords(t *testing.T) {
	port := geoEndpoint(t)
	long := strings.Repeat("A", 2048)
	for _, status := range []string{"407 <script>alert(1)</script>", "407 " + long} {
		res, out := runCheck(t, "http://"+checkUser+":"+checkPass+"@"+statusProxy(t, status), "http://geo.test:"+port+"/geo")
		if res.Reason != proxyAuth {
			t.Fatalf("%.20s: got %+v", status, res)
		}
		if strings.Contains(out, "script") || strings.Contains(out, "AAAA") {
			t.Fatalf("the output holds the proxy's text: %s", out)
		}
	}
}

func TestProxyCheckWithoutAProxyFile(t *testing.T) {
	t.Setenv("BROWSERPIER_PROXY_FILE", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("BROWSERPIER_GEOIP_URL", "http://geo.test/geo")
	var out bytes.Buffer
	if code := runProxyCheck(&out); code == 0 || strings.TrimSpace(out.String()) != `{"ok":false,"reason":"failed"}` {
		t.Fatalf("exit %d, %s", code, out.String())
	}
}

// TestProxyCheckModeSkipsTheBrowser: proxy-check runs before anything of
// the browser's: no mount check (which would fail here), no profile
// prepared (which would clear the lock).
func TestProxyCheckModeSkipsTheBrowser(t *testing.T) {
	if checkMode([]string{"entrypoint"}) != nil {
		t.Fatal("no argument must start the browser")
	}
	if checkMode([]string{"entrypoint", "sandbox-check"}) == nil || checkMode([]string{"entrypoint", proxyCheckArg}) == nil {
		t.Fatal("the checks aren't modes")
	}
	profile := t.TempDir()
	lock := filepath.Join(profile, "SingletonLock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSERPIER_PROFILE_DIR", profile)
	t.Setenv("BROWSERPIER_MOUNT_CHECK", "not-the-marker")
	t.Setenv("BROWSERPIER_PROXY_FILE", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("BROWSERPIER_GEOIP_URL", "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stdout
	os.Stdout = w
	code := checkMode([]string{"entrypoint", proxyCheckArg})()
	os.Stdout = was
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if code == 0 || strings.TrimSpace(string(out)) != `{"ok":false,"reason":"failed"}` {
		t.Fatalf("exit %d, %q", code, out)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("proxy-check prepared the profile")
	}
}

// TestProxyErrorKeepsItsText: the kinds don't change what a browser's
// start logs.
func TestProxyErrorKeepsItsText(t *testing.T) {
	addr := statusProxy(t, "407 Proxy Authentication Required")
	u, _ := url.Parse("http://" + checkUser + ":" + checkPass + "@" + addr)
	f := startForwarder(t, u.String(), egressPolicy{})
	_, err := f.connect(t.Context(), "geo.test:80")
	if err == nil || err.Error() != "the proxy answered 407 Proxy Authentication Required" {
		t.Fatalf("err = %v", err)
	}
	if checkReason(err, false) != proxyAuth {
		t.Fatalf("kind %s", checkReason(err, false))
	}
}
