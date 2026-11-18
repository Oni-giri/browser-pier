package browsertest

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// Proxy is an upstream proxy for tests: it requires a username and
// password and records every target it was asked to reach.
type Proxy struct {
	Addr    string // host:port it listens on, on every interface
	ln      net.Listener
	aliases map[string]string
	routes  map[string]string
	only    bool
	conns   map[net.Conn]struct{}
	mu      sync.Mutex
	targets []string
	refused []string
	denied  int
}

// Alias makes the proxy reach name at addr (an IP), like a DNS entry only
// the proxy has. A page that loads through such a name proves the name
// reached the proxy unresolved.
func (p *Proxy) Alias(name, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aliases == nil {
		p.aliases = map[string]string{}
	}
	p.aliases[name] = addr
}

// Route makes the proxy reach target (host:port, as the client asks for
// it) at addr (ip:port), so a test can serve a name on its usual port
// from a server on any port.
func (p *Proxy) Route(target, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routes == nil {
		p.routes = map[string]string{}
	}
	p.routes[target] = addr
}

// Isolate makes the proxy refuse every target that isn't aliased or
// routed, so nothing a test's browser asks for reaches the internet.
// Refused lists what it refused.
func (p *Proxy) Isolate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.only = true
}

func (p *Proxy) dial(target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	addr, routed := p.routes[target]
	a, aliased := p.aliases[host]
	if !routed && !aliased && p.only {
		p.refused = append(p.refused, target)
		p.mu.Unlock()
		return nil, errors.New("isolated: " + target + " is neither aliased nor routed")
	}
	p.mu.Unlock()
	switch {
	case routed:
	case aliased:
		addr = net.JoinHostPort(a, port)
	default:
		addr = target
	}
	return net.Dial("tcp", addr)
}

// Close takes the proxy down: no new connections, and every open tunnel cut.
func (p *Proxy) Close() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}

// track remembers an open connection so Close can cut it.
func (p *Proxy) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = map[net.Conn]struct{}{}
	}
	p.conns[c] = struct{}{}
}

// Port is the port it listens on.
func (p *Proxy) Port() string { _, port, _ := net.SplitHostPort(p.Addr); return port }

// Targets lists the host:port targets the proxy connected, in order.
func (p *Proxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// Refused lists the targets an isolated proxy refused, in order.
func (p *Proxy) Refused() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.refused...)
}

// Denied counts attempts with wrong credentials.
func (p *Proxy) Denied() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.denied
}

func (p *Proxy) record(target string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		p.targets = append(p.targets, target)
	} else {
		p.denied++
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // reached from browser containers
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func pipe(a, b net.Conn) {
	go func() { _, _ = io.Copy(a, b); _ = a.Close() }()
	_, _ = io.Copy(b, a)
	_ = b.Close()
}

// NewConnectProxy starts an HTTP proxy that only does CONNECT, with
// basic authentication.
func NewConnectProxy(t *testing.T, user, pass string) *Proxy {
	ln := listen(t)
	p := &Proxy{Addr: ln.Addr().String(), ln: ln}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					_ = c.Close()
					return
				}
				if req.Method != http.MethodConnect || req.Header.Get("Proxy-Authorization") != want {
					p.record(req.Host, false)
					_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
					_ = c.Close()
					return
				}
				up, err := p.dial(req.Host)
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
					_ = c.Close()
					return
				}
				p.record(req.Host, true)
				p.track(c)
				p.track(up)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				if n := br.Buffered(); n > 0 {
					b, _ := br.Peek(n)
					_, _ = up.Write(b)
				}
				pipe(c, up)
			}(c)
		}
	}()
	return p
}

// NewSOCKS5Proxy starts a SOCKS5 proxy with username and password
// authentication. It records targets as sent, so a host name arriving
// here was not resolved by the client.
func NewSOCKS5Proxy(t *testing.T, user, pass string) *Proxy {
	ln := listen(t)
	p := &Proxy{Addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.socks(c, user, pass)
		}
	}()
	return p
}

func (p *Proxy) socks(c net.Conn, user, pass string) {
	defer func() { _ = c.Close() }()
	h := make([]byte, 2)
	if _, err := io.ReadFull(c, h); err != nil {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, h[1])); err != nil {
		return
	}
	_, _ = c.Write([]byte{0x05, 0x02})
	v := make([]byte, 2)
	if _, err := io.ReadFull(c, v); err != nil {
		return
	}
	u := make([]byte, v[1])
	_, _ = io.ReadFull(c, u)
	l := make([]byte, 1)
	_, _ = io.ReadFull(c, l)
	pw := make([]byte, l[0])
	_, _ = io.ReadFull(c, pw)
	if string(u) != user || string(pw) != pass {
		p.record("", false)
		_, _ = c.Write([]byte{0x01, 0x01})
		return
	}
	_, _ = c.Write([]byte{0x01, 0x00})
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		ip := make([]byte, 4)
		_, _ = io.ReadFull(c, ip)
		host = net.IP(ip).String()
	case 0x03:
		n := make([]byte, 1)
		_, _ = io.ReadFull(c, n)
		name := make([]byte, n[0])
		_, _ = io.ReadFull(c, name)
		host = string(name)
	case 0x04:
		ip := make([]byte, 16)
		_, _ = io.ReadFull(c, ip)
		host = net.IP(ip).String()
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(c, pb)
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
	up, err := p.dial(target)
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	p.record(target, true)
	p.track(c)
	p.track(up)
	_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	pipe(c, up)
}
