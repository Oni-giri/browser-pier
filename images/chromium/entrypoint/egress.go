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
	// metadata endpoint 169.254.169.254 among it), the metadata endpoints
	// that sit inside the local ranges, and addresses that aren't a single
	// host.
	alwaysBlocked = prefixes(
		"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24",
		"fd00:ec2::254/128", "100.100.100.200/32", // AWS over IPv6, Alibaba Cloud
		"198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::1/128", "fe80::/10", "fec0::/10", "ff00::/8", "100::/64",
		// IPv6 that hides an IPv4 address we can't check: Teredo, and
		// local-use NAT64, whose prefix length varies.
		"2001::/32", "64:ff9b:1::/48",
	)
	// localRanges are the local network: private addresses (the Docker
	// bridge and its gateway, the LAN), shared address space (Tailscale)
	// and IPv6 unique local addresses. A profile can be allowed them.
	localRanges = prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7")
	// nat64 addresses carry an IPv4 address in their last 32 bits, 6to4
	// addresses in the 32 after the prefix.
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
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

// egressPolicy decides where pages may connect.
type egressPolicy struct {
	// localNetwork is the profile's opt-in to the local ranges.
	localNetwork bool
	// host are the Docker host's own addresses, as the admin gave them
	// (its public IP, say). A page reaching the host there would get to
	// its services as surely as through the bridge, so they count as local.
	host []netip.Addr
}

// allows reports whether a page may connect to ip.
func (p egressPolicy) allows(ip netip.Addr) bool {
	ip = embedded(ip)
	if !ip.IsValid() || within(ip, alwaysBlocked) {
		return false
	}
	if p.localNetwork {
		return true
	}
	for _, h := range p.host {
		if embedded(h) == ip {
			return false
		}
	}
	return !within(ip, localRanges)
}

// embedded is the address ip reaches: the IPv4 address inside an
// IPv4-mapped, NAT64 or 6to4 one, and without a zone (fe80::1%eth0 would
// make every prefix miss).
func embedded(ip netip.Addr) netip.Addr {
	ip = ip.WithZone("").Unmap()
	if !ip.Is6() {
		return ip
	}
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16]))
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6]))
	}
	return ip
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
