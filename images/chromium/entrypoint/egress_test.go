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
