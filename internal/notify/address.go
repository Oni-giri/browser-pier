package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// MaxURLLength is the longest webhook URL accepted, in bytes.
const MaxURLLength = 2048

// Lookup resolves a host name to its addresses.
type Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

func systemLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// errAddress is a refused address, at saving or at connecting. Its words
// never hold the URL.
var errAddress = errors.New("the webhook's address isn't allowed")

// cgnat is 100.64.0.0/10, which Tailscale uses.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// private is what a plain http:// webhook may reach: loopback, RFC 1918,
// fc00::/7 and 100.64.0.0/10, as the handoff page's "private" (in
// internal/handoff/web.go) defines it.
func private(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || cgnat.Contains(a)
}

// forbidden is what no webhook may reach, over either scheme: link-local
// (a VPS's metadata endpoint is 169.254.169.254), unspecified, multicast
// and broadcast.
func forbidden(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// allowed checks one address a webhook would connect to. A plain http://
// webhook (plain) may only reach private addresses.
func allowed(a netip.Addr, plain bool) error {
	if forbidden(a) {
		return fmt.Errorf("%w: link-local, multicast, broadcast and unspecified addresses never are", errAddress)
	}
	if plain && !private(a) {
		return fmt.Errorf("%w: a plain http:// webhook must be on this machine or its local network; use https://", errAddress)
	}
	return nil
}

// resolve returns a host's addresses, checked: one refused address
// refuses them all, so a name can't mix a good address with a bad one.
func resolve(ctx context.Context, lookup Lookup, host string, plain bool) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
	}
	for _, a := range addrs {
		if err := allowed(a, plain); err != nil {
			return nil, err
		}
	}
	return addrs, nil
}

// checkURL checks a webhook URL before it is saved. Its errors never echo
// the URL's path or query, which for Slack or Discord is the credential.
func checkURL(ctx context.Context, lookup Lookup, raw string) error {
	if len(raw) > MaxURLLength {
		return fmt.Errorf("the webhook URL is longer than %d bytes", MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("the webhook URL isn't a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("the webhook URL must start with https://")
	}
	if u.User != nil {
		return errors.New("the webhook URL can't hold a user name or password")
	}
	if u.Hostname() == "" || u.Opaque != "" {
		return errors.New("the webhook URL has no host")
	}
	plain := u.Scheme == "http"
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = resolve(ctx, lookup, u.Hostname(), plain)
	var dns *net.DNSError
	switch {
	case errors.Is(err, errAddress):
		return err
	case err != nil && plain:
		// A plain one is only allowed once its addresses are known.
		return fmt.Errorf("%w: couldn't look up its host to check it is on this network", errAddress)
	case errors.As(err, &dns) && dns.IsNotFound:
		return errors.New("the webhook URL's host doesn't exist")
	}
	// An https:// host that can't be looked up now is checked again
	// when connecting, like every other.
	return nil
}

// dialFunc connects to one address.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// netDial connects to one address, checking the address rules on the
// address actually dialled: a name can change what it resolves to between
// saving and sending.
func netDial(plain bool) dialFunc {
	d := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return errAddress
		}
		return allowed(ap.Addr(), plain)
	}}
	return d.DialContext
}

// dialPlan is how a webhook connection is made: budget is the time it may
// take in all, the lookup included; each of the host's addresses gets its
// share of what is left, at least minShare while that much is left; dial
// connects to one address.
type dialPlan struct {
	budget   time.Duration
	minShare time.Duration
	dial     dialFunc
}

// minDialShare is the least time an address gets before the next is
// tried, while that much time is left.
const minDialShare = 2 * time.Second

// dialer connects to a webhook. No proxy: the address check must see the
// real address. net/http dials with a context that may carry no deadline
// of its own, so the plan's budget is set before the lookup: time spent
// resolving is time the addresses don't get.
func dialer(lookup Lookup, plain bool, plan func() dialPlan) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		p := plan()
		ctx, cancel := context.WithTimeout(ctx, p.budget) // an earlier deadline stays
		defer cancel()
		addrs, err := resolve(ctx, lookup, host, plain)
		if err != nil {
			return nil, err
		}
		deadline, _ := ctx.Deadline()
		var last error
		for i, a := range addrs {
			c, err := dialOne(ctx, p, network, net.JoinHostPort(a.String(), port), deadline, len(addrs)-i)
			if err == nil {
				return c, nil
			}
			last = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, last
	}
}

// dialOne dials one of a host's addresses with its share of the time left,
// so an address that drops packets silently (often the IPv6 one) doesn't
// use the whole POST's time and the next is still tried. A share is never
// more than the time left.
func dialOne(ctx context.Context, p dialPlan, network, addr string, deadline time.Time, left int) (net.Conn, error) {
	remaining := time.Until(deadline)
	share := remaining
	if left > 1 {
		share = min(max(remaining/time.Duration(left), p.minShare), remaining)
	}
	ctx, cancel := context.WithTimeout(ctx, share)
	defer cancel()
	return p.dial(ctx, network, addr)
}
