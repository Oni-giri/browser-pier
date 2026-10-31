//go:build docker

package api

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestPagesCantReachTheLocalNetwork is PROJ-26's check: a page can't open
// the Docker host through the bridge gateway, the cloud metadata endpoint,
// another browser container or the container's own loopback, while public
// sites load. With the profile's opt-in the host is reachable; metadata
// and loopback still aren't.
func TestPagesCantReachTheLocalNetwork(t *testing.T) {
	a := newRealAPI(t)
	site := browsertest.NewSite(t, siteHost(t))
	_, sitePort, _ := net.SplitHostPort(strings.TrimPrefix(site.URL, "http://"))
	name := fmt.Sprintf("n%d", time.Now().UnixNano()%1e9)
	other := name + "o"
	for _, n := range []string{name, other} {
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+n).Run() })
		if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "api"}`, n), nil); code != http.StatusCreated {
			t.Fatalf("create %s: %d", n, code)
		}
	}
	var s, o sessionJSON
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, other), &o); code != http.StatusCreated {
		t.Fatalf("start the other: %d", code)
	}
	sock := a.m.Layout().ControlSocket(name)
	gateway := defaultGateway(t, "browserpier-"+name)
	t.Logf("gateway %s, the other container at %q", gateway, containerIP(t, "browserpier-"+other))

	p := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	targets := []string{
		"http://" + net.JoinHostPort(gateway, sitePort) + "/login",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fd00:ec2::254]/latest/meta-data/",
		"http://localhost:9222/json/version",
		"http://127.0.0.1:9222/json/version",
	}
	if ip := containerIP(t, "browserpier-"+other); ip != "" {
		// Nothing listens there on the network; the counter shows the
		// forwarder refused it rather than the container not answering.
		targets = append(targets, "http://"+net.JoinHostPort(ip, "9222")+"/json/version")
	}
	before := site.Requests()
	for _, u := range targets {
		refusedTo(t, p, sock, u)
	}
	if site.Requests() != before {
		t.Error("a page reached the test site on the Docker host")
	}

	// Public sites still load.
	p.Navigate("https://example.com/")
	if title := p.Eval("document.title"); title != "Example Domain" {
		t.Errorf("example.com: title %v", title)
	}
	p.Close()

	// With the opt-in the host is reachable; the metadata endpoint and
	// loopback still aren't.
	if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	a.allowLocalNetwork(t, name)
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	p = browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
	defer p.Close()
	p.Navigate(site.URL + "/login")
	if site.Requests() == before {
		t.Error("with the local network allowed, the test site wasn't reached")
	}
	refusedTo(t, p, sock, "http://169.254.169.254/latest/meta-data/")
	refusedTo(t, p, sock, "http://localhost:9222/json/version")
}

// refusedTo navigates to u and checks it failed because the forwarder
// refused it: Chromium could fail on its own, and prove nothing.
func refusedTo(t *testing.T, p *browsertest.CDP, sock, u string) {
	t.Helper()
	before := refusedCount(t, sock)
	var r struct{ ErrorText string }
	_ = json.Unmarshal(p.Call("Page.navigate", map[string]any{"url": u}), &r)
	if r.ErrorText == "" {
		t.Errorf("%s loaded", u)
	}
	if n := refusedCount(t, sock); n <= before {
		t.Errorf("%s: failed with %q, but not refused by the forwarder", u, r.ErrorText)
	}
}

func refusedCount(t *testing.T, sock string) int64 {
	t.Helper()
	resp, err := browsertest.ControlClient(sock).Get("http://control/proxy/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var v map[string]int64
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v["refused"]
}

// defaultGateway is the container's default route: the Docker host, on
// the bridge.
func defaultGateway(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "cat", "/proc/net/route").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			b, err := hex.DecodeString(f[2])
			if err == nil && len(b) == 4 {
				ip := make(net.IP, 4)
				binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(b))
				return ip.String()
			}
		}
	}
	t.Fatalf("no default route in %s:\n%s", container, out)
	return ""
}

// containerIP is the container's address on its network, or "" when it
// has none of its own (rootless Podman's slirp4netns).
func containerIP(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container).Output()
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
