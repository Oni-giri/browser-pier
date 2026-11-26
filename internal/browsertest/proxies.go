package browsertest

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Proxy is an upstream proxy for tests: it requires a username and
// password and records every target it was asked to reach, and the
// credentials each was reached with.
type Proxy struct {
	Addr    string // host:port it listens on, on every interface
	ln      net.Listener
	aliases map[string]string
	conns   map[net.Conn]struct{}
	mu      sync.Mutex
	targets []string
	logins  []Login
	denied  int
}

// Login is a user name and password a proxy let through, and the target
// they reached.
type Login struct{ User, Pass, Target string }

// Auth decides whether a user name and password may use a proxy.
type Auth func(user, pass string) bool

// Exactly is an Auth for one user name and password.
func Exactly(user, pass string) Auth {
	return func(u, p string) bool { return u == user && p == pass }
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

func (p *Proxy) dial(target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if a, ok := p.aliases[host]; ok {
		host = a
	}
	p.mu.Unlock()
	return net.Dial("tcp", net.JoinHostPort(host, port))
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

// Logins lists the credentials of every connection let through, in order.
func (p *Proxy) Logins() []Login {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Login(nil), p.logins...)
}

// Targets lists the host:port targets the proxy connected, in order.
func (p *Proxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// Denied counts attempts with wrong credentials.
func (p *Proxy) Denied() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.denied
}

func (p *Proxy) record(target, user, pass string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		p.targets = append(p.targets, target)
		p.logins = append(p.logins, Login{user, pass, target})
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
	return NewConnectProxyAuth(t, Exactly(user, pass))
}

// NewConnectProxyAuth is NewConnectProxy letting through whatever ok
// accepts, as a provider taking a session code in the user name does.
func NewConnectProxyAuth(t *testing.T, ok Auth) *Proxy {
	ln := listen(t)
	p := &Proxy{Addr: ln.Addr().String(), ln: ln}
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
				user, pass := basicAuth(req.Header.Get("Proxy-Authorization"))
				if req.Method != http.MethodConnect || !ok(user, pass) {
					p.record(req.Host, user, pass, false)
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
				p.record(req.Host, user, pass, true)
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

// basicAuth reads a Basic Proxy-Authorization header; empty if there is none.
func basicAuth(h string) (user, pass string) {
	enc, ok := strings.CutPrefix(h, "Basic ")
	if !ok {
		return "", ""
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", ""
	}
	user, pass, _ = strings.Cut(string(b), ":")
	return user, pass
}

// NewSOCKS5Proxy starts a SOCKS5 proxy with username and password
// authentication. It records targets as sent, so a host name arriving
// here was not resolved by the client.
func NewSOCKS5Proxy(t *testing.T, user, pass string) *Proxy {
	return NewSOCKS5ProxyAuth(t, Exactly(user, pass))
}

// NewSOCKS5ProxyAuth is NewSOCKS5Proxy letting through whatever ok accepts.
func NewSOCKS5ProxyAuth(t *testing.T, ok Auth) *Proxy {
	ln := listen(t)
	p := &Proxy{Addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.socks(c, ok)
		}
	}()
	return p
}

func (p *Proxy) socks(c net.Conn, ok Auth) {
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
	user, pass := string(u), string(pw)
	if !ok(user, pass) {
		p.record("", user, pass, false)
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
	p.record(target, user, pass, true)
	p.track(c)
	p.track(up)
	_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	pipe(c, up)
}
