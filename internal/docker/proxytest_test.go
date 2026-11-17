package docker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

func TestProxyTestConfig(t *testing.T) {
	const (
		image   = "ghcr.io/oni-giri/browser-pier-chromium:1.0.0"
		seccomp = `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`
		secret  = "s3cret-pw-91"
	)
	spec := ProxyTestSpec{Dir: "/srv/bp/proxytest", GeoURL: "https://ifconfig.co/json"}
	cfg, host := proxyTestConfig(image, seccomp, "apparmor=browserpier", "1000:1000", "keep-id", "inst1", spec)
	_, browser := browserConfig(image, seccomp, "apparmor=browserpier", BrowserSpec{User: "1000:1000"})

	if cfg.Image != image || cfg.User != "1000:1000" {
		t.Fatalf("image %q, user %q: the browser image, as the browsers' user", cfg.Image, cfg.User)
	}
	if len(cfg.ExposedPorts) != 0 || len(host.PortBindings) != 0 || host.PublishAllPorts {
		t.Fatal("the proxy test must publish no ports")
	}
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" || len(host.CapAdd) != 0 {
		t.Fatalf("caps drop %v add %v; want ALL dropped and nothing back", host.CapDrop, host.CapAdd)
	}
	if strings.Join(host.SecurityOpt, " ") != strings.Join(browser.SecurityOpt, " ") ||
		!strings.Contains(strings.Join(host.SecurityOpt, " "), "no-new-privileges:true") {
		t.Fatalf("security opts %v; want a browser's, %v", host.SecurityOpt, browser.SecurityOpt)
	}
	if host.NetworkMode != browser.NetworkMode {
		t.Fatalf("network %q; want a browser's, %q", host.NetworkMode, browser.NetworkMode)
	}
	if host.UsernsMode != "keep-id" || host.Privileged || host.ShmSize != 0 {
		t.Fatalf("userns %q privileged %t shm %d", host.UsernsMode, host.Privileged, host.ShmSize)
	}
	if host.Init == nil || *host.Init {
		t.Fatal("the image's entrypoint must be PID 1")
	}
	if len(host.Mounts) != 1 || len(host.Binds) != 0 || host.Mounts[0] != (mount.Mount{Type: mount.TypeBind, Source: spec.Dir, Target: "/run/browserpier"}) {
		t.Fatalf("mounts %+v binds %v; want the test's folder alone", host.Mounts, host.Binds)
	}
	if host.Memory != ProxyTestMemory || host.MemorySwap != ProxyTestMemory || host.PidsLimit == nil || *host.PidsLimit != ProxyTestPids {
		t.Fatalf("memory %d swap %d pids %v", host.Memory, host.MemorySwap, host.PidsLimit)
	}
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != "proxy-check" {
		t.Fatalf("cmd %v", cfg.Cmd)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "BROWSERPIER_GEOIP_URL="+spec.GeoURL {
		t.Fatalf("env %v; want the lookup endpoint only", cfg.Env)
	}
	if cfg.StopTimeout != nil {
		t.Fatal("no stop timeout: the check has no Chromium to close")
	}
	if len(cfg.Labels) != 2 || cfg.Labels[LabelProxyTest] != "true" || cfg.Labels[LabelInstance] != "inst1" || cfg.Labels[LabelManaged] != "" {
		t.Fatalf("labels %v; never a browser's", cfg.Labels)
	}
	if name := ProxyTestName("inst1"); name == "browserpier_check_inst1" || strings.HasPrefix(name, "browserpier-") {
		t.Fatalf("name %s could be a browser's or the sandbox check's", name)
	}
	b, _ := json.Marshal([]any{cfg, host})
	if strings.Contains(string(b), secret) {
		t.Fatal("the definition holds the proxy's password")
	}
}

func TestParseProxyResult(t *testing.T) {
	ok := `{"ok":true,"ip":"198.51.100.23","country":"DE","time_zone":"Europe/Berlin","locale":"de-DE","accept_languages":["de-DE","de","en-US","en"],"ms":312}`
	r, err := ParseProxyResult([]byte(ok+"\n"), 0)
	if err != nil || !r.OK || r.IP.String() != "198.51.100.23" || r.Country != "DE" || r.TimeZone != "Europe/Berlin" ||
		r.Locale != "de-DE" || strings.Join(r.Accept, ",") != "de-DE,de,en-US,en" || r.Lookup.Milliseconds() != 312 {
		t.Fatalf("ok: %+v, %v", r, err)
	}
	for _, reason := range proxyReasons {
		r, err := ParseProxyResult([]byte(`{"ok":false,"reason":"`+reason+`"}`), 1)
		if err != nil || r.OK || r.Reason != reason {
			t.Fatalf("%s: %+v, %v", reason, r, err)
		}
	}
	with := func(old, repl string) string { return strings.Replace(ok, old, repl, 1) }
	bad := map[string]struct {
		out  string
		code int
	}{
		"unknown field":         {with(`"ms":312`, `"ms":312,"city":"Berlin"`), 0},
		"unknown field, failed": {`{"ok":false,"reason":"auth","detail":"407 Proxy Authentication Required"}`, 1},
		"IP":                    {with("198.51.100.23", "198.51.100"), 0},
		"IP with a zone":        {with("198.51.100.23", "fe80::1%<b>x</b>"), 0},
		"country de":            {with(`"DE"`, `"de"`), 0},
		"country DEU":           {with(`"DE"`, `"DEU"`), 0},
		"time zone":             {with("Europe/Berlin", "Mars/Olympus"), 0},
		"time zone Local":       {with("Europe/Berlin", "Local"), 0},
		"ms negative":           {with(`"ms":312`, `"ms":-1`), 0},
		"ms zero":               {with(`"ms":312`, `"ms":0`), 0},
		"ms over the timeout":   {with(`"ms":312`, `"ms":30001`), 0},
		"unknown reason":        {`{"ok":false,"reason":"proxy said no"}`, 1},
		"two lines":             {ok + "\n" + ok, 0},
		"1 MB":                  {strings.Repeat(" ", 1<<20) + ok, 0},
		"HTML locale":           {with(`"locale":"de-DE"`, `"locale":"<b>de</b>"`), 0},
		"HTML language":         {with(`"en-US"`, `"<script>"`), 0},
		"HTML country":          {with(`"DE"`, `"<b>"`), 0},
		"HTML time zone":        {with("Europe/Berlin", "<b>Berlin</b>"), 0},
		"HTML IP":               {with("198.51.100.23", "<b>1.2.3.4</b>"), 0},
		"success exiting 1":     {ok, 1},
		"failure exiting 0":     {`{"ok":false,"reason":"auth"}`, 0},
		"failure with an IP":    {`{"ok":false,"reason":"auth","ip":"198.51.100.23"}`, 1},
		"no ok":                 {`{"reason":"auth"}`, 1},
		"not JSON":              {`sandbox-check: ok`, 0},
		"nothing":               {``, 1},
		"trailing JSON":         {ok + ` {"ok":true}`, 0},
	}
	for name, c := range bad {
		if r, err := ParseProxyResult([]byte(c.out), c.code); err == nil {
			t.Errorf("%s: accepted as %+v", name, r)
		}
	}
}

// proxyDaemon answers the calls TestProxy makes before the container
// runs, the way the Docker API does.
type proxyDaemon struct {
	mu       sync.Mutex
	labels   map[string]string // the image's
	leftover map[string]string // a container by the test's name, with these labels; nil for none
	removed  []string
	creates  int
}

func (d *proxyDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	path := apiVersion.ReplaceAllString(r.URL.Path, "")
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	name := ProxyTestName("inst1")
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.44")
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodGet && path == "/images/img/json":
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:1", "Config": map[string]any{"Labels": d.labels}})
	case r.Method == http.MethodGet && path == "/containers/"+name+"/json":
		if d.leftover == nil {
			fail(http.StatusNotFound, "no such container")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "old", "Name": "/" + name, "Config": map[string]any{"Labels": d.leftover}})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
		d.removed = append(d.removed, strings.TrimPrefix(path, "/containers/"))
		if strings.HasSuffix(path, "/"+name) {
			d.leftover = nil
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/create":
		d.creates++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "t1"})
	case r.Method == http.MethodPost && path == "/containers/t1/start":
		fail(http.StatusInternalServerError, "no starting in this test")
	default:
		fail(http.StatusNotImplemented, r.Method+" "+path)
	}
}

func proxyClient(t *testing.T, d *proxyDaemon) *Client {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	api, err := client.New(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	return &Client{api: api, image: "img", instance: "inst1", user: "1000:1000", seccompOpt: "seccomp={}", log: slog.New(slog.DiscardHandler)}
}

var spec = ProxyTestSpec{Dir: "/srv/bp/proxytest", GeoURL: "https://ifconfig.co/json"}

// TestProxyTestLeftovers: a leftover test container is removed before a
// test; a container by that name that isn't one is neither removed nor
// replaced.
func TestProxyTestLeftovers(t *testing.T) {
	d := &proxyDaemon{labels: map[string]string{LabelProxyCheck: "1"}, leftover: map[string]string{LabelProxyTest: "true", LabelInstance: "inst1"}}
	c := proxyClient(t, d)
	if _, err := c.TestProxy(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "no starting") {
		t.Fatalf("err = %v; want the fake's start failure", err)
	}
	name := ProxyTestName("inst1")
	if len(d.removed) != 2 || d.removed[0] != name || d.removed[1] != "t1" || d.creates != 1 {
		t.Fatalf("removed %v, %d creates; want the leftover gone first, then the test's own after it failed", d.removed, d.creates)
	}

	for _, labels := range []map[string]string{{}, {LabelProxyTest: "true", LabelInstance: "another"}, {LabelManaged: "true"}} {
		d := &proxyDaemon{labels: map[string]string{LabelProxyCheck: "1"}, leftover: labels}
		c := proxyClient(t, d)
		if _, err := c.TestProxy(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "isn't BrowserPier's proxy test") {
			t.Fatalf("labels %v: err = %v", labels, err)
		}
		if len(d.removed) != 0 || d.creates != 0 {
			t.Fatalf("labels %v: removed %v, %d creates; someone else's container must be left alone", labels, d.removed, d.creates)
		}
		if err := c.RemoveProxyTest(context.Background()); err == nil || len(d.removed) != 0 {
			t.Fatalf("labels %v: start-up removed someone else's container (%v)", labels, err)
		}
	}

	// At start-up.
	d = &proxyDaemon{leftover: map[string]string{LabelProxyTest: "true", LabelInstance: "inst1"}}
	if err := proxyClient(t, d).RemoveProxyTest(context.Background()); err != nil || len(d.removed) != 1 || d.removed[0] != name {
		t.Fatalf("start-up: %v, removed %v", err, d.removed)
	}
}

// TestProxyTestNeedsTheCheck: an image older than the check makes no
// container.
func TestProxyTestNeedsTheCheck(t *testing.T) {
	d := &proxyDaemon{labels: map[string]string{LabelSandboxCheck: "1"}}
	c := proxyClient(t, d)
	if _, err := c.TestProxy(context.Background(), spec); !errors.Is(err, ErrNoProxyCheck) {
		t.Fatalf("err = %v", err)
	}
	if d.creates != 0 || len(d.removed) != 0 {
		t.Fatalf("%d creates, removed %v", d.creates, d.removed)
	}
}

func TestProxyTestOneAtATime(t *testing.T) {
	d := &proxyDaemon{labels: map[string]string{LabelProxyCheck: "1"}}
	c := proxyClient(t, d)
	c.proxyTestMu.Lock()
	_, err := c.TestProxy(context.Background(), spec)
	c.proxyTestMu.Unlock()
	if !errors.Is(err, ErrProxyTestBusy) || d.creates != 0 {
		t.Fatalf("err = %v, %d creates", err, d.creates)
	}
}
