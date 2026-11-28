package main

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// TestLookupRedirectRule (PROJ-204): a lookup follows a redirect only on
// the endpoint's host, http to https at most, and three at most.
func TestLookupRedirectRule(t *testing.T) {
	cases := []struct {
		name  string
		chain []string // the URLs asked for so far, then the redirect's
		ok    bool
	}{
		{"http to https, default ports", []string{"http://geo.example/json", "https://geo.example/json"}, true},
		{"http to https, another path", []string{"http://geo.example/json", "https://geo.example/v2/json"}, true},
		{"http on 8080 to https on 443", []string{"http://geo.example:8080/json", "https://geo.example/json"}, true},
		{"http to https, explicit 443", []string{"http://geo.example/json", "https://geo.example:443/json"}, true},
		{"host name in another case", []string{"http://geo.example/json", "https://GEO.Example/json"}, true},
		{"https, another path", []string{"https://geo.example/json", "https://geo.example/json/"}, true},
		{"http, another path", []string{"http://geo.example:8080/a", "http://geo.example:8080/b"}, true},
		{"three redirects", []string{"http://geo.example/0", "https://geo.example/1", "https://geo.example/2", "https://geo.example/3"}, true},

		{"another host", []string{"https://geo.example/json", "https://other.example/json"}, false},
		{"a subdomain", []string{"https://geo.example/json", "https://www.geo.example/json"}, false},
		{"the parent domain", []string{"https://www.geo.example/json", "https://geo.example/json"}, false},
		{"another host by IP", []string{"http://127.0.0.1:8080/json", "http://127.0.0.2:8080/json"}, false},
		{"https to http", []string{"https://geo.example/json", "http://geo.example/json"}, false},
		{"https to http on 443", []string{"https://geo.example/json", "http://geo.example:443/json"}, false},
		{"back to http after the upgrade", []string{"http://geo.example/0", "https://geo.example/1", "http://geo.example/2"}, false},
		{"http to https on another port", []string{"http://geo.example:8080/json", "https://geo.example:8443/json"}, false},
		{"http to https on the same port", []string{"http://geo.example:8080/json", "https://geo.example:8080/json"}, true},
		{"http to https on 80, the same port", []string{"http://geo.example/json", "https://geo.example:80/json"}, true},
		{"https to another port", []string{"https://geo.example/json", "https://geo.example:8443/json"}, false},
		{"http to another port", []string{"http://geo.example/json", "http://geo.example:8080/json"}, false},
		{"a trailing dot added", []string{"https://geo.example/json", "https://geo.example./json"}, true},
		{"a trailing dot dropped", []string{"https://geo.example./json", "https://geo.example/json"}, true},
		{"IPv6, long form to canonical", []string{"http://[2001:db8:0:0:0:0:0:1]:8080/json", "http://[2001:db8::1]:8080/json"}, true},
		{"IPv6, canonical to long form", []string{"http://[2001:db8::1]:8080/json", "http://[2001:0db8::0001]:8080/json"}, true},
		{"IPv4, the same address", []string{"http://192.0.2.1/json", "https://192.0.2.1/json"}, true},

		{"user name and password in the redirect", []string{"https://geo.example/json", "https://attacker:secret@geo.example/json"}, false},
		{"a user name in the redirect", []string{"https://geo.example/json", "https://attacker@geo.example/json"}, false},
		{"only dots", []string{"https://./json", "https://./json"}, false},
		{"an empty host", []string{"https://geo.example/json", "https:///json"}, false},
		{"IPv4-mapped IPv6 to its IPv4", []string{"http://[::ffff:192.0.2.1]/json", "http://192.0.2.1/json"}, false},
		{"IPv4 to its IPv4-mapped IPv6", []string{"http://192.0.2.1/json", "http://[::ffff:192.0.2.1]/json"}, false},
		{"IPv6 with a zone", []string{"http://[fe80::1%25eth0]/json", "http://[fe80::1%25eth0]/json2"}, false},
		{"IPv6 gaining a zone", []string{"http://[fe80::1]/json", "http://[fe80::1%25eth0]/json"}, false},
		{"a name to an address", []string{"http://localhost/json", "http://127.0.0.1/json"}, false},
		{"another scheme, same host", []string{"http://geo.example/json", "ftp://geo.example/json"}, false},
		{"another scheme throughout", []string{"foo://geo.example/a", "foo://geo.example/b"}, false},
		{"four redirects", []string{"http://geo.example/0", "https://geo.example/1", "https://geo.example/2", "https://geo.example/3", "https://geo.example/4"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var via []*http.Request
			for _, u := range c.chain[:len(c.chain)-1] {
				via = append(via, mustReq(t, u))
			}
			err := lookupRedirect(mustReq(t, c.chain[len(c.chain)-1]), via)
			if c.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !c.ok && !errors.Is(err, http.ErrUseLastResponse) {
				t.Fatalf("got %v, want the redirect's own answer (ErrUseLastResponse)", err)
			}
		})
	}
}

func mustReq(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestDirectLookupRedirects: without a proxy, a start's lookup follows a
// redirect on the same host and refuses one to another, which gets no
// request.
func TestDirectLookupRedirects(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		_, _ = io.WriteString(w, tlsGeo)
	}))
	t.Cleanup(other.Close)
	_, otherPort, _ := net.SplitHostPort(other.Listener.Addr().String())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/geo":
			_, _ = io.WriteString(w, plainGeo)
		case "/same":
			http.Redirect(w, req, "/geo", http.StatusFound)
		case "/other":
			// Another host name, which reaches the other listener.
			http.Redirect(w, req, "http://localhost:"+otherPort+"/geo", http.StatusFound)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)

	ip, _, _, err := startLookup(context.Background(), &forwarder{}, srv.URL+"/same")()
	if err != nil || ip != "198.51.100.23" {
		t.Fatalf("same host: %q, %v", ip, err)
	}
	ip, _, _, err = startLookup(context.Background(), &forwarder{}, srv.URL+"/other")()
	var ae geoAnswerError
	if !errors.As(err, &ae) {
		t.Fatalf("another host: %q, %v; want an unusable answer", ip, err)
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the other host got %d requests", n)
	}
}

// TestLookupClientUsesTheRule: the client a start's lookup and the check
// go out with has the rule, with a proxy or without one.
func TestLookupClientUsesTheRule(t *testing.T) {
	up, _ := url.Parse("http://u:p@127.0.0.1:1")
	proxied, err := newForwarder(up, egressPolicy{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	direct := &forwarder{}
	for name, fwd := range map[string]*forwarder{"proxied": proxied, "direct": direct} {
		for which, c := range map[string]*http.Client{"lookupClient": lookupClient(fwd), "tunnelTracked": must2(tunnelTracked(fwd))} {
			if c.CheckRedirect == nil {
				t.Fatalf("%s %s: no redirect rule (Go's default follows any host)", name, which)
			}
			err := c.CheckRedirect(mustReq(t, "https://other.example/"), []*http.Request{mustReq(t, "https://geo.example/")})
			if !errors.Is(err, http.ErrUseLastResponse) {
				t.Fatalf("%s %s: another host's redirect gave %v", name, which, err)
			}
		}
	}
}

func must2[A, B any](a A, _ B) A { return a }

// Answers that tell who gave them.
const (
	tlsGeo   = `{"ip":"198.51.100.44","country_iso":"FR","time_zone":"Europe/Paris"}`
	plainGeo = `{"ip":"198.51.100.23","country_iso":"DE","time_zone":"Europe/Berlin"}`
)

// redirectRig is a lookup endpoint as example.com (the name httptest's
// certificate is for), served over http on a port of its own and over
// https on 443 through the proxy, plus other.test, another host.
type redirectRig struct {
	up                               *browsertest.Proxy
	plainPort, tlsPort, otherPort    string
	plainGeoHits, tlsHits, otherHits atomic.Int32
	plainURL                         string // http://example.com:<plainPort>
}

func newRedirectRig(t *testing.T) *redirectRig {
	t.Helper()
	r := &redirectRig{}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.tlsHits.Add(1)
		switch {
		case req.URL.Path == "/geo":
			_, _ = io.WriteString(w, tlsGeo)
		case req.URL.Path == "/down":
			// The same host, back to http.
			http.Redirect(w, req, "http://example.com:"+r.plainPort+"/geo", http.StatusFound)
		case strings.HasPrefix(req.URL.Path, "/hops/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(req.URL.Path, "/hops/"))
			if n == 0 {
				_, _ = io.WriteString(w, tlsGeo)
				return
			}
			http.Redirect(w, req, "/hops/"+strconv.Itoa(n-1), http.StatusFound)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(tlsSrv.Close)
	_, r.tlsPort, _ = net.SplitHostPort(tlsSrv.Listener.Addr().String())

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.otherHits.Add(1)
		_, _ = io.WriteString(w, plainGeo)
	}))
	t.Cleanup(other.Close)
	_, r.otherPort, _ = net.SplitHostPort(other.Listener.Addr().String())

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/geo":
			r.plainGeoHits.Add(1)
			_, _ = io.WriteString(w, plainGeo)
		case "/up":
			http.Redirect(w, req, "https://example.com/geo", http.StatusMovedPermanently)
		case "/up-case":
			http.Redirect(w, req, "https://EXAMPLE.Com/geo", http.StatusMovedPermanently)
		case "/up-port":
			// https, but on another port than 443.
			http.Redirect(w, req, "https://example.com:"+r.tlsPort+"/geo", http.StatusFound)
		case "/hops":
			http.Redirect(w, req, "https://example.com/hops/"+req.URL.Query().Get("n"), http.StatusFound)
		case "/other":
			http.Redirect(w, req, "http://other.test:"+r.otherPort+"/geo", http.StatusFound)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(plain.Close)
	_, r.plainPort, _ = net.SplitHostPort(plain.Listener.Addr().String())
	r.plainURL = "http://example.com:" + r.plainPort

	roots := x509.NewCertPool()
	roots.AddCert(tlsSrv.Certificate())
	was := lookupRoots
	lookupRoots = roots
	t.Cleanup(func() { lookupRoots = was })

	r.up = browsertest.NewConnectProxy(t, checkUser, checkPass)
	r.up.Isolate()
	r.up.Alias("example.com", "127.0.0.1")
	r.up.Route("example.com:443", tlsSrv.Listener.Addr().String())
	// The proxy's names are case-sensitive; DNS isn't.
	r.up.Route("EXAMPLE.Com:443", tlsSrv.Listener.Addr().String())
	r.up.Alias("other.test", "127.0.0.1")
	return r
}

func (r *redirectRig) proxyURL() string {
	return "http://" + checkUser + ":" + checkPass + "@" + r.up.Addr
}

// TestLookupRedirects (PROJ-204): a start's lookup and the proxy check
// follow the same redirects, through the proxy: only on the endpoint's
// host, and a refused one is a lookup with no usable answer that sent
// nothing to where it pointed.
func TestLookupRedirects(t *testing.T) {
	cases := []struct {
		name     string
		endpoint func(r *redirectRig) string
		wantIP   string // "" for a refused redirect
		check    func(t *testing.T, r *redirectRig)
	}{
		{"same host, http to https", func(r *redirectRig) string { return r.plainURL + "/up" }, "198.51.100.44", nil},
		{"same host in another case", func(r *redirectRig) string { return r.plainURL + "/up-case" }, "198.51.100.44", nil},
		{"three redirects", func(r *redirectRig) string { return r.plainURL + "/hops?n=2" }, "198.51.100.44", nil},
		{"another host", func(r *redirectRig) string { return r.plainURL + "/other" }, "", func(t *testing.T, r *redirectRig) {
			if n := r.otherHits.Load(); n != 0 {
				t.Errorf("the other host got %d requests", n)
			}
			for _, target := range r.up.Targets() {
				if strings.HasPrefix(target, "other.test") {
					t.Errorf("the proxy was asked for %s", target)
				}
			}
		}},
		{"https to http", func(*redirectRig) string { return "https://example.com/down" }, "", func(t *testing.T, r *redirectRig) {
			if n := r.plainGeoHits.Load(); n != 0 {
				t.Errorf("the http endpoint got %d requests", n)
			}
		}},
		{"upgrade to another port", func(r *redirectRig) string { return r.plainURL + "/up-port" }, "", func(t *testing.T, r *redirectRig) {
			if n := r.tlsHits.Load(); n != 0 {
				t.Errorf("the https endpoint got %d requests", n)
			}
		}},
		{"four redirects", func(r *redirectRig) string { return r.plainURL + "/hops?n=3" }, "", func(t *testing.T, r *redirectRig) {
			// Three followed (/hops/3, /2, /1), the fourth (/hops/0) not.
			if n := r.tlsHits.Load(); n != 3 {
				t.Errorf("the https endpoint got %d requests, want 3", n)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Run("start", func(t *testing.T) {
				r := newRedirectRig(t)
				u, _ := url.Parse(r.proxyURL())
				fwd, err := newForwarder(u, egressPolicy{}, slog.New(slog.DiscardHandler))
				if err != nil {
					t.Fatal(err)
				}
				endpoint := c.endpoint(r)
				ip, _, _, err := startLookup(context.Background(), fwd, endpoint)()
				if c.wantIP != "" {
					if err != nil || ip != c.wantIP {
						t.Fatalf("lookup = %q, %v; want %s", ip, err, c.wantIP)
					}
				} else {
					var ae geoAnswerError
					if !errors.As(err, &ae) {
						t.Fatalf("lookup = %q, %v; want an unusable answer", ip, err)
					}
					// With nothing stored, a proxied start doesn't go ahead.
					if _, err := decideGeo(config{GeoURL: endpoint}, true, func() (string, string, string, error) { return "", "", "", ae }); err == nil {
						t.Fatal("the start went ahead")
					}
				}
				if c.check != nil {
					c.check(t, r)
				}
			})
			t.Run("check", func(t *testing.T) {
				r := newRedirectRig(t)
				res, _ := runCheck(t, r.proxyURL(), c.endpoint(r))
				if c.wantIP != "" {
					if !res.OK || res.IP != c.wantIP {
						t.Fatalf("got %+v, want ip %s", res, c.wantIP)
					}
				} else if res.OK || res.Reason != checkLookup {
					t.Fatalf("got %+v, want reason %s", res, checkLookup)
				}
				if c.check != nil {
					c.check(t, r)
				}
			})
		})
	}
}
