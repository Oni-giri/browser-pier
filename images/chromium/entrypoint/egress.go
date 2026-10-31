package main

import (
	"errors"
	"net/netip"
	"strings"
)

// Pages are untrusted, so they don't get to reach what sits next to the
// browser: the Docker host and its other containers, the LAN behind a home
// server, a VPS's metadata endpoint. Every connection Chromium makes goes
// through the forwarder, which checks it against these ranges.
var (
	// alwaysBlocked are never reachable, whatever the profile allows: the
	// container's own loopback (CDP listens there), link-local (the cloud
	// metadata endpoint 169.254.169.254 among it), and addresses that
	// aren't a single host.
	alwaysBlocked = prefixes(
		"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24",
		"198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fe80::/10", "fec0::/10", "ff00::/8",
	)
	// localRanges are the local network: private addresses (the Docker
	// bridge and its gateway, the LAN), shared address space (Tailscale)
	// and IPv6 unique local addresses. A profile can be allowed them.
	localRanges = prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7")
	// nat64 addresses carry an IPv4 address in their last 32 bits.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// errRefused is a destination the profile may not reach.
var errRefused = errors.New("destination not allowed")

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// allowedDest reports whether a page may connect to ip; localNetwork is
// the profile's opt-in to the local ranges.
func allowedDest(ip netip.Addr, localNetwork bool) bool {
	// A zone (fe80::1%eth0) would make every prefix miss.
	ip = ip.WithZone("").Unmap()
	if nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte(b[12:]))
	}
	if !ip.IsValid() || within(ip, alwaysBlocked) {
		return false
	}
	return localNetwork || !within(ip, localRanges)
}

func within(ip netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// localhostName is a name that always means this machine (RFC 6761),
// whoever resolves it.
func localhostName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}
