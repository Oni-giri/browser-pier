//go:build docker

package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// TestStickyProxyKeepsEachProfilesExit (PROJ-153): with a {sticky}
// template, the upstream proxy sees only the profile's own code in the
// user name, for pages and the location lookup alike; the same one after
// a stop and start (and the login survives); another profile's for
// another profile; a new one after New exit IP. A placeholder in the
// password works too.
func TestStickyProxyKeepsEachProfilesExit(t *testing.T) {
	for kind, start := range map[string]func(*testing.T, browsertest.Auth) *browsertest.Proxy{
		"http":   browsertest.NewConnectProxyAuth,
		"socks5": browsertest.NewSOCKS5ProxyAuth,
	} {
		t.Run(kind, func(t *testing.T) {
			a := newRealAPI(t)
			ctx := context.Background()
			host := browsertest.SiteHost(t)
			site := browsertest.NewSite(t, "browserpier.test")
			userRE := regexp.MustCompile(`^pier-[a-z0-9]{8}$`)
			passRE := regexp.MustCompile(`^pw-[a-z0-9]{8}$`)
			up := start(t, func(user, pass string) bool {
				return userRE.MatchString(user) && pass == "pw" || user == "pier" && passRE.MatchString(pass)
			})
			up.Alias("browserpier.test", "127.0.0.1")
			a.m.SetGeoURL(site.URL + "/geo")
			addr := net.JoinHostPort(host, up.Port())

			stamp := time.Now().UnixNano() % 1e8
			profile := func(suffix, proxy string) string {
				t.Helper()
				name := fmt.Sprintf("s%d%s", stamp, suffix)
				t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
				p := profiles.New(name)
				p.DefaultMode, p.Width, p.Height = profiles.ModeAPI, 1280, 800
				p.Proxy = proxy
				if _, err := a.m.Profiles().Create(ctx, p); err != nil {
					t.Fatal(err)
				}
				return name
			}
			// run starts the profile's browser, signs in or checks the login
			// through do, stops it, and returns the credentials the proxy
			// saw meanwhile, which must all be the same.
			run := func(name string, do func(*browsertest.CDP)) browsertest.Login {
				t.Helper()
				from := len(up.Logins())
				var s sessionJSON
				if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated {
					t.Fatalf("start %s: %d %+v", name, code, s)
				}
				pg := browsertest.PageVia(t, strings.Replace(s.CDPURL, "ws://", "http://", 1)+"/json/list", a.header())
				do(pg)
				pg.Close()
				tok, err := a.m.Profiles().StickyToken(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				if out, _ := exec.Command("docker", "logs", "browserpier-"+name).CombinedOutput(); strings.Contains(string(out), tok) {
					t.Errorf("the browser's log holds the token:\n%s", out)
				}
				var stopped sessionJSON
				if code := a.do(http.MethodDelete, "/api/sessions/"+s.ID, "", &stopped); code != http.StatusOK {
					t.Fatalf("stop %s: %d", name, code)
				}
				if strings.Contains(stopped.LastLog+stopped.Error, tok) {
					t.Errorf("the session's last log or error holds the token: %+v", stopped)
				}
				logins := up.Logins()[from:]
				if len(logins) == 0 {
					t.Fatalf("%s: nothing went through the proxy", name)
				}
				for _, l := range logins[1:] {
					if l.User != logins[0].User || l.Pass != logins[0].Pass {
						t.Fatalf("%s: one run used two logins, %+v and %+v", name, logins[0], l)
					}
				}
				if got, _ := a.m.Profiles().Get(ctx, name); got.Location.Country != "DE" {
					t.Errorf("%s: the location lookup didn't go through: %+v", name, got.Location)
				}
				return logins[0]
			}
			look := func(pg *browsertest.CDP) { pg.Navigate(site.URL + "/account") }

			// One profile: its own code, kept across a stop and start,
			// with the login.
			first := profile("a", kind+"://pier-{sticky}:pw@"+addr)
			var token string
			l1 := run(first, func(pg *browsertest.CDP) { token = site.Login(t, pg) })
			tok, _ := a.m.Profiles().StickyToken(ctx, first)
			if l1.User != "pier-"+tok || l1.Pass != "pw" {
				t.Fatalf("the proxy saw %+v, want user pier-%s", l1, tok)
			}
			l2 := run(first, func(pg *browsertest.CDP) {
				if got := site.Account(t, pg); got != token {
					t.Errorf("after stop and start the site saw session %q, want %q", got, token)
				}
			})
			if l2 != l1 {
				t.Fatalf("the second start used %+v, the first %+v", l2, l1)
			}

			// Another profile with the same template: another code.
			second := profile("b", kind+"://pier-{sticky}:pw@"+addr)
			if l := run(second, look); l.User == l1.User || !userRE.MatchString(l.User) {
				t.Fatalf("the second profile used %+v, the first %+v", l, l1)
			}

			// New exit IP, while stopped: a new code at the next start.
			if err := a.m.Profiles().NewExit(ctx, first); err != nil {
				t.Fatal(err)
			}
			if l := run(first, look); l.User == l1.User || !userRE.MatchString(l.User) {
				t.Fatalf("after a new exit the profile used %+v, before %+v", l, l1)
			}

			// In the password.
			third := profile("c", kind+"://pier:pw-{sticky}@"+addr)
			tok3, _ := a.m.Profiles().StickyToken(ctx, third)
			if l := run(third, look); l.User != "pier" || l.Pass != "pw-"+tok3 {
				t.Fatalf("with {sticky} in the password the proxy saw %+v", l)
			}
		})
	}
}
