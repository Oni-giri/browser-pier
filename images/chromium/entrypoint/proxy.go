package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// forwarderAddr is where Chromium finds its proxy when the profile has one.
const forwarderAddr = "127.0.0.1:1080"

// forwarder is the SOCKS5 proxy every connection Chromium makes goes
// through. It refuses destinations the profile may not reach (egress.go).
// When the profile has an upstream proxy, it adds the credentials (Chrome's
// --proxy-server can't carry a username and password) and counts traffic.
// Chromium sends host names over SOCKS5, and the upstream proxy resolves
// them: nothing resolves DNS locally, so lookups don't leak around it.
// Without one, the forwarder resolves names itself, checks every address
// and connects to the ones it checked. With the profile's ad and tracker
// list on, it also refuses names on the list (block.go), before any lookup
// or upstream dial.
type forwarder struct {
	upstream *url.URL // nil: connect directly
	policy   egressPolicy
	block    *blocker // nil: no list
	dialer   net.Dialer
	lookup   func(ctx context.Context, host string) ([]netip.Addr, error)
	up, down atomic.Int64 // bytes sent and received
	refused  atomic.Int64 // connections refused by the policy
	log      *slog.Logger
}

func newForwarder(upstream *url.URL, policy egressPolicy, log *slog.Logger) (*forwarder, error) {
	if upstream != nil {
		switch upstream.Scheme {
		case "http", "https", "socks5":
		default:
			return nil, fmt.Errorf("proxy scheme %q: want http, https or socks5", upstream.Scheme)
		}
		if upstream.Port() == "" {
			return nil, errors.New("proxy: no port")
		}
	}
	return &forwarder{
		upstream: upstream, policy: policy, dialer: net.Dialer{Timeout: 15 * time.Second}, log: log,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}, nil
}

// readProxyFile reads the upstream proxy URL the orchestrator left in path
// and deletes the file before Chromium starts, so the credentials don't
// linger where it could read them. A missing file means no proxy.
func readProxyFile(path string) (*url.URL, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the container's own config
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	var v struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.URL == "" {
		return nil, errors.New("proxy file: want {\"url\": \"...\"}")
	}
	u, err := url.Parse(v.URL)
	if err != nil {
		return nil, errors.New("proxy file: the URL doesn't parse")
	}
	return u, nil
}

// serve accepts SOCKS5 connections from Chromium until ln closes.
func (f *forwarder) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

// handle serves one SOCKS5 CONNECT (RFC 1928, no authentication: only
// processes in this container can reach the loopback listener).
func (f *forwarder) handle(client net.Conn) {
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	addr, err := socks5Accept(client)
	if err != nil {
		_ = client.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	up, err := f.dial(ctx, addr)
	cancel()
	if err != nil {
		code := byte(0x01) // general failure
		switch {
		case errors.Is(err, errRefused):
			code = 0x02 // not allowed by ruleset
			f.refused.Add(1)
			f.log.Info("refused a connection to the local network", "to", addr)
		case errors.Is(err, errBlocked):
			code = 0x02
			f.block.blocked.Add(1)
		case f.upstream != nil:
			f.log.Warn("proxy: upstream refused a connection", "err", err)
		}
		_, _ = client.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		_ = client.Close()
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = up.Close()
		_ = client.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	f.splice(client, up)
}

// socks5Accept reads a SOCKS5 greeting and CONNECT request and returns
// the requested host:port.
func socks5Accept(c net.Conn) (string, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil || head[0] != 0x05 {
		return "", errors.New("not SOCKS5")
	}
	if _, err := io.ReadFull(c, make([]byte, head[1])); err != nil {
		return "", err
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return "", err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return "", err
	}
	if req[0] != 0x05 || req[1] != 0x01 {
		_, _ = c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // command not supported
		return "", errors.New("only CONNECT is supported")
	}
	var host string
	switch req[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", err
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", errors.New("bad address type")
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(c, port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), nil
}

func (f *forwarder) splice(client, up net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyCount := func(dst, src net.Conn, n *atomic.Int64) {
		defer wg.Done()
		_, _ = io.Copy(counter{dst, n}, src)
		// Unblock the other direction.
		_ = dst.Close()
		_ = src.Close()
	}
	go copyCount(up, client, &f.up)
	go copyCount(client, up, &f.down)
	wg.Wait()
}

// dial opens a connection for Chromium to addr (host:port), unless the
// profile may not reach it.
func (f *forwarder) dial(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !f.policy.allows(ip) {
			return nil, fmt.Errorf("%w: %s", errRefused, host)
		}
		return f.connect(ctx, addr)
	}
	if localhostName(host) {
		return nil, fmt.Errorf("%w: %s", errRefused, host)
	}
	// Names only: an address was judged above, by the policy alone. The
	// list is checked before the name is looked up or sent upstream, so a
	// blocked name doesn't even leave the container.
	if f.block.blocks(host) {
		return nil, fmt.Errorf("%w: %s", errBlocked, host)
	}
	if f.upstream != nil {
		// The upstream resolves the name, on its own network, which is
		// what the profile's proxy is for. Looking it up here to check it
		// would send the lookup around the proxy.
		return f.connect(ctx, addr)
	}
	ips, err := f.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !f.policy.allows(ip) {
			return nil, fmt.Errorf("%w: %s is %s", errRefused, host, ip)
		}
	}
	// Connect to the addresses checked, not to the name: looked up again,
	// it could answer differently.
	var errs []error
	for _, ip := range ips {
		conn, err := f.dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// connect opens a connection to addr through the upstream proxy, or
// straight to it without one. It checks nothing: dial does, for pages.
// An error reaching or going through the upstream is a *proxyError,
// which says what kind of failure it was; its text is the cause's own.
func (f *forwarder) connect(ctx context.Context, addr string) (net.Conn, error) {
	if f.upstream == nil {
		return f.dialer.DialContext(ctx, "tcp", addr)
	}
	conn, err := f.dialer.DialContext(ctx, "tcp", f.upstream.Host)
	if err != nil {
		return nil, &proxyError{kind: failKind(err, proxyUnreachable), err: err}
	}
	if f.upstream.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: f.upstream.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, &proxyError{kind: failKind(err, proxyTLS), err: err}
		}
		conn = tc
	}
	// The handshake with the proxy ends with the caller's deadline, if
	// sooner: a proxy that never answers is then a timeout, not a hang.
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if f.upstream.Scheme == "socks5" {
		err = socks5Connect(conn, addr, f.upstream.User)
	} else {
		err = httpConnect(conn, addr, f.upstream.User)
	}
	if err != nil {
		_ = conn.Close()
		var pe *proxyError
		if !errors.As(err, &pe) {
			// Cut off or garbled: the proxy answered, but no tunnel.
			err = &proxyError{kind: failKind(err, proxyRefused), err: err}
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// The kinds of proxyError: a fixed set, which the proxy check reports
// (proxycheck.go) instead of any text.
const (
	proxyUnreachable = "unreachable" // the name doesn't resolve, or nothing answers
	proxyTLS         = "tls_proxy"   // the TLS handshake with an https proxy failed
	proxyAuth        = "auth"        // 407, or SOCKS5 refusing the method or the credentials
	proxyRefused     = "refused"     // the proxy answered but opened no tunnel
	proxyTimeout     = "timeout"     // the deadline passed
)

// proxyError is a failure reaching the upstream proxy or getting a
// tunnel through it. Error is the cause's own text, as before kinds
// existed; kind is what the proxy check reports, never the text, which
// may hold the proxy's own words.
type proxyError struct {
	kind string
	err  error
}

func (e *proxyError) Error() string { return e.err.Error() }
func (e *proxyError) Unwrap() error { return e.err }

// failKind is kind, unless err is a deadline passing.
func failKind(err error, kind string) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return proxyTimeout
	}
	return kind
}

// httpConnect asks an HTTP proxy for a tunnel to addr.
func httpConnect(conn net.Conn, addr string, user *url.Userinfo) error {
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if user != nil {
		pass, _ := user.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+pass)) + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		return err
	}
	// Read the response byte by byte up to the blank line, so nothing of
	// the tunnel's first bytes is swallowed by a buffer.
	br := bufio.NewReaderSize(&oneByte{conn}, 1)
	// Telling ReadResponse this answers a CONNECT makes it expect no body.
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		kind := proxyRefused
		if resp.StatusCode == http.StatusProxyAuthRequired {
			kind = proxyAuth
		}
		return &proxyError{kind: kind, err: fmt.Errorf("the proxy answered %s", resp.Status)}
	}
	return nil
}

// oneByte reads at most one byte at a time.
type oneByte struct{ r io.Reader }

func (o *oneByte) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

// socks5Connect does a SOCKS5 handshake (RFC 1928), with username and
// password authentication (RFC 1929) when given, and connects to addr by
// name, so the proxy resolves it.
func socks5Connect(conn net.Conn, addr string, user *url.Userinfo) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return err
	}
	method := byte(0x00) // no authentication
	if user != nil {
		method = 0x02 // username and password
	}
	if _, err := conn.Write([]byte{0x05, 0x01, method}); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x05 || reply[1] != method {
		// Not SOCKS5 at all is no tunnel; a SOCKS5 proxy that wants
		// another method wants credentials, or others.
		kind := proxyAuth
		if reply[0] != 0x05 {
			kind = proxyRefused
		}
		return &proxyError{kind: kind, err: errors.New("the SOCKS5 proxy refused our authentication method")}
	}
	if user != nil {
		pass, _ := user.Password()
		name := user.Username()
		if len(name) > 255 || len(pass) > 255 {
			return &proxyError{kind: proxyAuth, err: errors.New("SOCKS5 username or password over 255 bytes")}
		}
		msg := append([]byte{0x01, byte(len(name))}, name...) //nolint:gosec // checked ≤ 255 above
		msg = append(append(msg, byte(len(pass))), pass...)   //nolint:gosec // checked ≤ 255 above
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
		if reply[1] != 0x00 {
			return &proxyError{kind: proxyAuth, err: errors.New("the SOCKS5 proxy rejected the username or password")}
		}
	}
	if len(host) > 255 {
		return errors.New("host name too long")
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = append(append(req, 0x01), ip.To4()...)
	} else if ip != nil {
		req = append(append(req, 0x04), ip.To16()...)
	} else {
		req = append(append(req, 0x03, byte(len(host))), host...) //nolint:gosec // checked ≤ 255 above
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[1] != 0x00 {
		return &proxyError{kind: proxyRefused, err: fmt.Errorf("the SOCKS5 proxy couldn't connect (code %d)", head[1])}
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		skip = int(l[0])
	default:
		return errors.New("bad SOCKS5 reply")
	}
	_, err = io.ReadFull(conn, make([]byte, skip+2))
	return err
}

// counter counts bytes as they pass, so a long-lived tunnel shows its
// traffic before it closes.
type counter struct {
	w io.Writer
	n *atomic.Int64
}

func (c counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// stats serves the counters on the control socket.
func (f *forwarder) stats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]int64{"bytes_sent": f.up.Load(), "bytes_received": f.down.Load(), "refused": f.refused.Load(), "blocked": 0, "block_list": 0}
	if f.block != nil {
		out["blocked"], out["block_list"] = f.block.blocked.Load(), int64(f.block.set.Len())
	}
	_ = json.NewEncoder(w).Encode(out)
}
