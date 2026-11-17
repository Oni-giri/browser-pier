//go:build docker

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const proxyTestPass = "s3cret:pw"

// proxyTestContainers lists the proxy test containers on the host, by
// their label: running or not, any instance.
func proxyTestContainers(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label="+docker.LabelProxyTest+"=true", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

// stallProxy passes connections on to target once released: a proxy that
// is slow to answer, so a test can look at the container meanwhile.
func stallProxy(t *testing.T, target string) (port string, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // reached from the test's container
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gate := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				select {
				case <-gate:
				case <-time.After(time.Minute):
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, func() { once.Do(func() { close(gate) }) }
}

// inspected is what docker inspect says of a container, as far as the
// test needs.
type inspected struct {
	Config struct {
		Env    []string
		Cmd    []string
		Labels map[string]string
	}
	HostConfig struct {
		PortBindings    map[string]any
		PublishAllPorts bool
		NetworkMode     string
		Memory          int64
	}
	Mounts []struct {
		Source, Destination string
	}
	NetworkSettings struct {
		Ports map[string]any
	}
}

func inspectWhenThere(t *testing.T, name string) inspected {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, err := exec.Command("docker", "inspect", name).Output()
		var v []inspected
		if err == nil && json.Unmarshal(out, &v) == nil && len(v) == 1 {
			return v[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy test's container never appeared: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func checkNothingLeft(t *testing.T, a agent) {
	t.Helper()
	if left := proxyTestContainers(t); len(left) != 0 {
		t.Fatalf("containers left behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(a.m.Layout().DataDir, "proxytest")); !os.IsNotExist(err) {
		t.Fatalf("the test's folder is still there: %v", err)
	}
}

// TestProxyTestInAContainer runs the Test button's check for real: from
// a browser-image container on the browsers' network, through http and
// socks5 proxies, to the test site's /geo.
func TestProxyTestInAContainer(t *testing.T) {
	for kind, start := range map[string]func(*testing.T, string, string) *browsertest.Proxy{
		"http":   browsertest.NewConnectProxy,
		"socks5": browsertest.NewSOCKS5Proxy,
	} {
		t.Run(kind, func(t *testing.T) {
			a := newRealAPI(t)
			checkNothingLeft(t, a)
			ctx := context.Background()
			host := browsertest.SiteHost(t)
			site := browsertest.NewSite(t, "browserpier.test")
			up := start(t, "pier", proxyTestPass)
			up.Alias("browserpier.test", "127.0.0.1")
			a.m.SetGeoURL(site.URL + "/geo")
			sitePort := site.URL[strings.LastIndex(site.URL, ":")+1:]
			proxyAt := func(port, pass string) string {
				return kind + "://pier:" + url.QueryEscape(pass) + "@" + net.JoinHostPort(host, port)
			}

			// Right credentials, looked at while it runs.
			port, release := stallProxy(t, up.Addr)
			type outcome struct {
				res docker.ProxyResult
				err error
			}
			done := make(chan outcome, 1)
			began := time.Now()
			go func() {
				res, err := a.m.TestProxy(ctx, sessions.ProxyTest{Proxy: proxyAt(port, proxyTestPass), For: "settings"})
				done <- outcome{res, err}
			}()
			names := func() []string {
				deadline := time.Now().Add(20 * time.Second)
				for {
					if n := proxyTestContainers(t); len(n) > 0 || time.Now().After(deadline) {
						return n
					}
					time.Sleep(100 * time.Millisecond)
				}
			}()
			if len(names) != 1 || !strings.HasPrefix(names[0], "browserpier_proxytest_") {
				t.Fatalf("test containers %v", names)
			}
			c := inspectWhenThere(t, names[0])
			raw, _ := json.Marshal([]any{c.Config.Env, c.Config.Cmd, c.Config.Labels})
			for _, secret := range []string{"s3cret", url.QueryEscape(proxyTestPass), "pier:", net.JoinHostPort(host, port)} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("the container's env, command or labels hold %q: %s", secret, raw)
				}
			}
			if len(c.HostConfig.PortBindings) != 0 || c.HostConfig.PublishAllPorts || len(c.NetworkSettings.Ports) != 0 {
				t.Fatalf("the test's container publishes ports: %+v %+v", c.HostConfig, c.NetworkSettings)
			}
			if len(c.Mounts) != 1 || c.Mounts[0].Destination != "/run/browserpier" ||
				filepath.Clean(c.Mounts[0].Source) != filepath.Join(a.m.Layout().HostDataDir, "proxytest") {
				t.Fatalf("mounts %+v; want the test's folder alone", c.Mounts)
			}
			if c.HostConfig.Memory != docker.ProxyTestMemory || c.Config.Labels[docker.LabelManaged] != "" {
				t.Fatalf("memory %d, labels %v", c.HostConfig.Memory, c.Config.Labels)
			}
			release()
			o := <-done
			t.Logf("create to remove: %s", time.Since(began))
			if o.err != nil {
				t.Fatal(o.err)
			}
			if r := o.res; !r.OK || r.IP.String() != "198.51.100.23" || r.Country != "DE" || r.TimeZone != "Europe/Berlin" || r.Locale != "de-DE" || r.Lookup <= 0 {
				t.Fatalf("result %+v", r)
			}
			if got := up.Targets(); len(got) != 1 || got[0] != "browserpier.test:"+sitePort {
				t.Fatalf("the proxy carried %v; want one tunnel, to the lookup host", got)
			}
			checkNothingLeft(t, a)

			// Each way it fails, and nothing left after any.
			fails := []struct {
				name, proxy, geo, want string
			}{
				{"wrong password", proxyAt(up.Port(), "wrong"), "/geo", docker.ProxyAuth},
				{"lookup answers 404", proxyAt(up.Port(), proxyTestPass), "/no-such-path", docker.ProxyLookup},
			}
			for _, f := range fails {
				a.m.SetGeoURL(site.URL + f.geo)
				res, err := a.m.TestProxy(ctx, sessions.ProxyTest{Proxy: f.proxy, For: "settings"})
				if err != nil || res.OK || res.Reason != f.want {
					t.Fatalf("%s: %+v, %v; want %s", f.name, res, err, f.want)
				}
				checkNothingLeft(t, a)
			}
			a.m.SetGeoURL(site.URL + "/geo")
			up.Close()
			res, err := a.m.TestProxy(ctx, sessions.ProxyTest{Proxy: proxyAt(up.Port(), proxyTestPass), For: "settings"})
			if err != nil || res.OK || (res.Reason != docker.ProxyUnreachable && res.Reason != docker.ProxyRefused) {
				t.Fatalf("proxy stopped: %+v, %v", res, err)
			}
			t.Logf("proxy stopped: %s", res.Reason)
			checkNothingLeft(t, a)
		})
	}
}

// TestProxyTestCutShort: a test cancelled half way, and a second test
// while one runs, leave nothing behind and never make two containers.
func TestProxyTestCutShort(t *testing.T) {
	a := newRealAPI(t)
	host := browsertest.SiteHost(t)
	site := browsertest.NewSite(t, "browserpier.test")
	up := browsertest.NewConnectProxy(t, "pier", proxyTestPass)
	up.Alias("browserpier.test", "127.0.0.1")
	a.m.SetGeoURL(site.URL + "/geo")
	port, release := stallProxy(t, up.Addr)
	t.Cleanup(release)
	proxy := "http://pier:" + url.QueryEscape(proxyTestPass) + "@" + net.JoinHostPort(host, port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.m.TestProxy(ctx, sessions.ProxyTest{Proxy: proxy, For: "settings"})
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for len(proxyTestContainers(t)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the test's container never appeared")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A second test meanwhile is turned away, and makes no container.
	if _, err := a.m.TestProxy(context.Background(), sessions.ProxyTest{Proxy: proxy, For: "settings"}); !errors.Is(err, sessions.ErrProxyTestBusy) {
		t.Fatalf("second test: %v", err)
	}
	if n := proxyTestContainers(t); len(n) != 1 {
		t.Fatalf("containers %v", n)
	}
	time.Sleep(time.Second) // well inside the check: its handshake waits on the stalled proxy
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled test reported no error")
	}
	checkNothingLeft(t, a)
}

// TestProxyTestBesideItsBrowser: testing a running profile's proxy leaves
// its browser alone, and its login and stored location too: the AGENTS.md
// run, with tests before the first start and while the browser runs.
func TestProxyTestBesideItsBrowser(t *testing.T) {
	a := newRealAPI(t)
	host := browsertest.SiteHost(t)
	site := browsertest.NewSite(t, "browserpier.test")
	up := browsertest.NewSOCKS5Proxy(t, "pier", proxyTestPass)
	up.Alias("browserpier.test", "127.0.0.1")
	a.m.SetGeoURL(site.URL + "/geo")
	name := fmt.Sprintf("p%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.DefaultMode, p.Width, p.Height = profiles.ModeAPI, 1280, 800
	p.Proxy = "socks5://pier:" + url.QueryEscape(proxyTestPass) + "@" + net.JoinHostPort(host, up.Port())
	if _, err := a.m.Profiles().Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	location := func() profiles.Location {
		got, err := a.m.Profiles().Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return got.Location
	}
	// Before the first start: nothing stored, and the test stores nothing.
	if res, err := a.m.TestProxy(ctx, sessions.ProxyTest{Proxy: p.Proxy, Profile: name, For: name}); err != nil || !res.OK {
		t.Fatalf("TestProxy = %+v, %v", res, err)
	}
	if l := location(); l != (profiles.Location{}) {
		t.Fatalf("the test stored a location: %+v", l)
	}

	var s sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("start: %d %+v", code, s)
	}
	control := browsertest.ControlClient(a.m.Layout().ControlSocket(name))
	pg := browsertest.Page(t, control)
	login := site.Login(t, pg)
	pg.Close()
	stored := location()
	if stored.Country != "DE" {
		t.Fatalf("the start stored %+v", stored)
	}
	res, err := a.m.TestProxy(ctx, sessions.ProxyTest{Proxy: p.Proxy, Profile: name, For: name})
	if err != nil || !res.OK || res.Country != "DE" {
		t.Fatalf("TestProxy = %+v, %v", res, err)
	}
	live, ok := a.m.Live(name)
	if !ok || live.ID != s.ID || live.State != sessions.Running {
		t.Fatalf("the session after the test: %+v, %t; want %s running", live, ok, s.ID)
	}
	req, _ := http.NewRequest(http.MethodGet, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/version", nil)
	req.Header = a.header()
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CDP after the test: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	checkNothingLeft(t, a)
	if l := location(); l != stored {
		t.Fatalf("the location went from %+v to %+v", stored, l)
	}

	// Stop, start again: still signed in.
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := a.m.Stop(stopCtx, s.ID); err != nil {
		t.Fatal(err)
	}
	var again sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &again); code != http.StatusCreated || again.ID == s.ID {
		t.Fatalf("second start: %d %+v", code, again)
	}
	if got := site.Account(t, browsertest.Page(t, control)); got != login {
		t.Fatalf("after stop and start the site saw session %q, want %q", got, login)
	}
	// The new start checked again (CheckedAt), and found the same place.
	if l := location(); l.Country != stored.Country || l.TimeZone != stored.TimeZone {
		t.Fatalf("the location went from %+v to %+v", stored, l)
	}
}
