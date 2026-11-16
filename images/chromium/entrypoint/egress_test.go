package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

func TestEgressPolicy(t *testing.T) {
	for _, c := range []struct {
		ip           string
		plain, local bool // allowed without and with the local-network opt-in
	}{
		{"93.184.215.14", true, true},
		{"2606:2800:21f:cb07:6820:80da:af6b:8b2c", true, true},
		{"8.8.8.8", true, true},
		// The Docker host and its containers, the LAN, Tailscale.
		{"172.17.0.1", false, true},
		{"172.31.255.254", false, true},
		{"10.21.0.1", false, true},
		{"192.168.1.1", false, true},
		{"100.101.102.103", false, true},
		{"fd7a:115c:a1e0::1", false, true},
		// Never: loopback, metadata and other link-local, the rest.
		{"127.0.0.1", false, false},
		{"127.8.9.10", false, false},
		{"::1", false, false},
		{"169.254.169.254", false, false},
		{"fd00:ec2::254", false, false},
		{"100.100.100.200", false, false},
		{"fe80::1", false, false},
		{"fe80::1%eth0", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"224.0.0.1", false, false},
		{"255.255.255.255", false, false},
		// The same addresses written another way.
		{"::ffff:127.0.0.1", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"::ffff:192.168.1.1", false, true},
		{"64:ff9b::a9fe:a9fe", false, false}, // NAT64 for 169.254.169.254
		{"64:ff9b::c0a8:101", false, true},   // and for 192.168.1.1
		{"64:ff9b::808:808", true, true},
		{"2002:a9fe:a9fe::1", false, false}, // 6to4 for 169.254.169.254
		{"2002:c0a8:101::1", false, true},   // and for 192.168.1.1
		{"2002:808:808::1", true, true},
		{"::127.0.0.1", false, false},                          // IPv4-compatible
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false, false}, // Teredo
		{"64:ff9b:1::a9fe:a9fe", false, false},                 // local-use NAT64
		// The host's own public address, as the admin gave it.
		{"203.0.113.10", false, true},
		{"::ffff:203.0.113.10", false, true},
		{"203.0.113.11", true, true},
	} {
		ip := netip.MustParseAddr(c.ip)
		host := []netip.Addr{netip.MustParseAddr("203.0.113.10")}
		if got := (egressPolicy{host: host}).allows(ip); got != c.plain {
			t.Errorf("%s: allowed %t, want %t", c.ip, got, c.plain)
		}
		if got := (egressPolicy{localNetwork: true, host: host}).allows(ip); got != c.local {
			t.Errorf("%s with the local network: allowed %t, want %t", c.ip, got, c.local)
		}
	}
}

// fakeDNS answers lookups from a table, as a resolver an attacker
// controls could.
func fakeDNS(table map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ss, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []netip.Addr
		for _, s := range ss {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func TestForwarderRefusesTheLocalNetwork(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }))
	defer origin.Close()
	port, _ := strconv.Atoi(strings.Split(origin.Listener.Addr().String(), ":")[1])
	dns := fakeDNS(map[string][]string{
		"public.test":   {"93.184.215.14"},
		"lan.test":      {"192.168.1.1"},
		"metadata.test": {"169.254.169.254"},
		// One public address and one private: refused, or a second
		// connection attempt would land on the private one.
		"mixed.test": {"93.184.215.14", "10.0.0.5"},
	})

	f := startForwarder(t, "", egressPolicy{})
	f.lookup = dns
	for _, host := range []string{"127.0.0.1", "localhost", "app.localhost", "LOCALHOST.", "169.254.169.254", "lan.test", "metadata.test", "mixed.test", "172.17.0.1"} {
		if _, err := socksGet(t, fwdAddrs[f], host, port); err == nil {
			t.Errorf("%s: connected", host)
		}
	}
	if got := f.refused.Load(); got != 9 {
		t.Errorf("refused %d connections, want 9", got)
	}
	// A name that doesn't resolve fails, but isn't the policy's doing.
	if _, err := socksGet(t, fwdAddrs[f], "nowhere.test", port); err == nil || f.refused.Load() != 9 {
		t.Errorf("unknown name: %v, refused %d", err, f.refused.Load())
	}

	// With the opt-in the LAN is reachable; loopback and metadata still aren't.
	lan := startForwarder(t, "", egressPolicy{localNetwork: true})
	lan.lookup = dns
	for _, host := range []string{"127.0.0.1", "localhost", "metadata.test", "169.254.169.254"} {
		if _, err := socksGet(t, fwdAddrs[lan], host, port); err == nil {
			t.Errorf("with the local network, %s: connected", host)
		}
	}
	if got := lan.refused.Load(); got != 4 {
		t.Errorf("with the local network, refused %d, want 4", got)
	}
	// Let it get as far as dialing, then stop: nothing listens there.
	lan.dialer.ControlContext = func(context.Context, string, string, syscall.RawConn) error { return errors.New("stop here") }
	if _, err := lan.dial(context.Background(), net.JoinHostPort("lan.test", "1")); err == nil || errors.Is(err, errRefused) {
		t.Errorf("with the local network, lan.test refused: %v", err)
	}
}

func TestForwarderConnectsToTheAddressItChecked(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }))
	defer origin.Close()
	port, _ := strconv.Atoi(strings.Split(origin.Listener.Addr().String(), ":")[1])

	// The test origin is on loopback, which pages never reach; stand in a
	// public address for it, and see the forwarder dial what it checked.
	f := startForwarder(t, "", egressPolicy{})
	f.lookup = fakeDNS(map[string][]string{"public.test": {"93.184.215.14"}})
	var dialed []string
	f.dialer.ControlContext = func(_ context.Context, _, addr string, _ syscall.RawConn) error {
		dialed = append(dialed, addr)
		return errors.New("stop here")
	}
	_, _ = socksGet(t, fwdAddrs[f], "public.test", port)
	if len(dialed) != 1 || dialed[0] != net.JoinHostPort("93.184.215.14", strconv.Itoa(port)) {
		t.Fatalf("dialed %v", dialed)
	}
}

// TestForwarderBlocksListedHosts: names on the ad and tracker list, and
// their subdomains, are refused before they are looked up; their siblings
// and parents aren't; the allowlist wins; addresses are the policy's alone.
func TestForwarderBlocksListedHosts(t *testing.T) {
	f := startForwarder(t, "", egressPolicy{})
	f.block = newBlocker(domainset.Build([]string{"ads.example.com", "tracker.example.net", "challenges.cloudflare.com", "hcaptcha.com"}))
	var looked, dialed []string
	resolve := fakeDNS(map[string][]string{})
	f.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		looked = append(looked, host)
		if host == "lan.example.org" {
			return resolve(ctx, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	f.dialer.ControlContext = func(_ context.Context, _, addr string, _ syscall.RawConn) error {
		dialed = append(dialed, addr)
		return errors.New("stop here")
	}
	dial := func(host string) error {
		_, err := f.dial(context.Background(), net.JoinHostPort(host, "443"))
		return err
	}
	for _, host := range []string{"ads.example.com", "x.ads.example.com", "a.b.tracker.example.net", "ADS.Example.COM.", "tracker.example.net."} {
		if err := dial(host); !errors.Is(err, errBlocked) {
			t.Errorf("%s: %v, want blocked", host, err)
		}
	}
	if len(looked) != 0 || len(dialed) != 0 {
		t.Errorf("blocked names were looked up %v or dialled %v", looked, dialed)
	}
	// Siblings and parents, the allowlist, and addresses get through to the dial.
	for _, host := range []string{"notads.example.com", "example.com", "ads.example.com.evil.org", "challenges.cloudflare.com", "js.hcaptcha.com", "93.184.215.14"} {
		if err := dial(host); err == nil || errors.Is(err, errBlocked) || errors.Is(err, errRefused) {
			t.Errorf("%s: %v, want it dialled", host, err)
		}
	}
	if len(dialed) != 6 {
		t.Errorf("dialled %d, want 6: %v", len(dialed), dialed)
	}
	// A list never makes an address reachable the policy refuses.
	if err := dial("10.0.0.1"); !errors.Is(err, errRefused) {
		t.Errorf("10.0.0.1: %v, want refused", err)
	}
	if err := dial("localhost"); !errors.Is(err, errRefused) {
		t.Errorf("localhost: %v, want refused", err)
	}

	// Over SOCKS, a blocked name is refused and counted apart from the policy's refusals.
	if _, err := socksGet(t, fwdAddrs[f], "ads.example.com", 443); err == nil {
		t.Error("connected to a blocked name")
	}
	if f.block.blocked.Load() != 1 || f.refused.Load() != 0 {
		t.Errorf("blocked %d, refused %d; want 1 and 0", f.block.blocked.Load(), f.refused.Load())
	}

	// A host opened directly is let through, with its subdomains, not its parent.
	if err := f.block.exemptHost("Ads.Example.com"); err != nil {
		t.Fatal(err)
	}
	for host, blocked := range map[string]bool{"ads.example.com": false, "cdn.ads.example.com": false, "tracker.example.net": true} {
		if err := dial(host); errors.Is(err, errBlocked) != blocked {
			t.Errorf("after the exemption, %s: %v", host, err)
		}
	}
	// Off for the session: nothing is blocked.
	f.block.on.Store(false)
	if err := dial("tracker.example.net"); errors.Is(err, errBlocked) {
		t.Errorf("blocking off, still blocked: %v", err)
	}
}

func TestBlockerControls(t *testing.T) {
	b := newBlocker(domainset.Build([]string{"ads.example.com", "tracker.example.org"}))
	mux := http.NewServeMux()
	b.register(mux)
	post := func(path, body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}
	if code := post("/proxy/exempt", `{"host": "1.2.3.4"}`); code != http.StatusBadRequest {
		t.Errorf("exempting an address: %d", code)
	}
	if code := post("/proxy/exempt", `{"host": "x.ads.example.com"}`); code != http.StatusNoContent || b.blocks("x.ads.example.com") || !b.blocks("ads.example.com") {
		t.Errorf("exempt: %d", code)
	}
	// A public suffix would let every site under it through; a single
	// label isn't a host name the list could hold.
	for _, host := range []string{"co.uk", "github.io", "com", "localhost"} {
		if code := post("/proxy/exempt", `{"host": "`+host+`"}`); code != http.StatusBadRequest {
			t.Errorf("exempting %s: %d", host, code)
		}
	}
	// Hosts the list doesn't block take no place: many of them, then
	// listed ones up to the cap still go in.
	for i := range 2 * maxExempt {
		if err := b.exemptHost("h" + strconv.Itoa(i) + ".example.net"); err != nil {
			t.Fatalf("an unlisted host: %v", err)
		}
	}
	if code := post("/proxy/exempt", `{"host": "challenges.cloudflare.com"}`); code != http.StatusNoContent {
		t.Errorf("an allowlisted host: %d", code)
	}
	b.mu.Lock()
	n := len(b.exempt)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d hosts exempted, want only the listed x.ads.example.com", n)
	}
	for i := range maxExempt - 1 {
		if err := b.exemptHost("h" + strconv.Itoa(i) + ".tracker.example.org"); err != nil {
			t.Fatalf("listed host %d: %v", i, err)
		}
	}
	if b.blocks("h3.tracker.example.org") || !b.blocks("tracker.example.org") {
		t.Error("the exemptions don't hold")
	}
	if code := post("/proxy/exempt", `{"host": "one-more.tracker.example.org"}`); code != http.StatusConflict || !b.blocks("one-more.tracker.example.org") {
		t.Errorf("past the cap: %d", code)
	}
	// Past the cap, an unlisted host still needs nothing.
	if code := post("/proxy/exempt", `{"host": "fine.example.net"}`); code != http.StatusNoContent {
		t.Errorf("an unlisted host past the cap: %d", code)
	}
	if code := post("/proxy/blocking/off", ""); code != http.StatusNoContent || b.blocks("ads.example.com") {
		t.Errorf("off: %d", code)
	}
	// Without a list, blocking is off, and a nil blocker blocks nothing.
	if newBlocker(nil).blocks("ads.example.com") || (*blocker)(nil).blocks("ads.example.com") {
		t.Error("blocked without a list")
	}
}
