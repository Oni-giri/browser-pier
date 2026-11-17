package profiles

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestValidProxyTakesAStickyTemplate(t *testing.T) {
	for _, proxy := range []string{
		"http://user-{sticky}:pw@proxy.example:8080",
		"https://user-{sticky}:pw@proxy.example:8443",
		"socks5://u:pw-{sticky}@proxy.example:1080",
		"http://a-{sticky}-b-{sticky}:pw@proxy.example:8080",
		"socks5://{sticky}:{sticky}@proxy.example:1080",
		"http://user-%7Bsticky%7D:pw@proxy.example:8080", // a literal, not a placeholder
		"http://" + strings.Repeat("u", 247) + "{sticky}:" + strings.Repeat("p", 255) + "@proxy.example:8080",
	} {
		if err := validProxy(proxy); err != nil {
			t.Errorf("validProxy(%q) = %v", proxy, err)
		}
	}
}

func TestValidProxyRefusesBadTemplates(t *testing.T) {
	for name, proxy := range map[string]string{
		"in the host":       "http://user:pw@{sticky}.proxy.example:8080",
		"in the port":       "http://user:pw@proxy.example:{sticky}",
		"in the scheme":     "{sticky}://user:pw@proxy.example:8080",
		"in the path":       "http://user:pw@proxy.example:8080/{sticky}",
		"another one":       "http://user-{other}:pw@proxy.example:8080",
		"a lone brace":      "http://user-{:pw@proxy.example:8080",
		"upper case":        "http://user-{STICKY}:pw@proxy.example:8080",
		"long user":         "http://" + strings.Repeat("u", 254) + "{sticky}:pw@proxy.example:8080",
		"long password":     "socks5://u:" + strings.Repeat("p", 254) + "{sticky}@proxy.example:1080",
		"long plain user":   "socks5://" + strings.Repeat("u", 256) + ":pw@proxy.example:1080",
		"colon in user":     "http://a%3A{sticky}:pw@proxy.example:8080",
		"colon, https":      "https://a%3Ab:pw@proxy.example:8443",
		"no host, template": "http://user-{sticky}:pw@:8080",
	} {
		if err := validProxy(proxy); err == nil {
			t.Errorf("%s: validProxy(%q) accepted it", name, proxy)
		}
	}
	// SOCKS5 carries a colon in a user name; Basic authentication can't.
	if err := validProxy("socks5://a%3A{sticky}:pw@proxy.example:1080"); err != nil {
		t.Errorf("socks5 user name with a colon: %v", err)
	}
}

func TestFillSticky(t *testing.T) {
	const tok = "k3y9x2m1"
	for in, want := range map[string]string{
		"http://user-{sticky}:pw@proxy.example:8080":         "http://user-k3y9x2m1:pw@proxy.example:8080",
		"socks5://u:pw-{sticky}@proxy.example:1080":          "socks5://u:pw-k3y9x2m1@proxy.example:1080",
		"http://{sticky}-{sticky}:{sticky}@proxy.example:80": "http://k3y9x2m1-k3y9x2m1:k3y9x2m1@proxy.example:80",
		"http://user-%7Bsticky%7D:pw@proxy.example:8080":     "http://user-%7Bsticky%7D:pw@proxy.example:8080",
		"http://user:pw@proxy.example:8080":                  "http://user:pw@proxy.example:8080",
		"http://proxy.example:8080":                          "http://proxy.example:8080",
		// Only the user info: braces elsewhere stay for validProxy to refuse.
		"http://u:p@{sticky}.example:8080":    "http://u:p@{sticky}.example:8080",
		"http://u-{sticky}:p@h:1/{sticky}":    "http://u-k3y9x2m1:p@h:1/{sticky}",
		"http://u-{sticky}:p@h:1?q={sticky}@": "http://u-k3y9x2m1:p@h:1?q={sticky}@",
		"":                                    "",
	} {
		if got := FillSticky(in, tok); got != want {
			t.Errorf("FillSticky(%q) = %q, want %q", in, got, want)
		}
	}
	u, err := url.Parse(FillSticky("http://u-{sticky}:p%40ss@proxy.example:8080", tok))
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != "p@ss" || u.User.Username() != "u-"+tok {
		t.Fatalf("filled user info %v", u.User)
	}
}

func TestNewStickyToken(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{8}$`)
	seen := map[string]bool{}
	for range 1000 {
		tok := NewStickyToken()
		if !re.MatchString(tok) {
			t.Fatalf("token %q", tok)
		}
		if seen[tok] {
			t.Fatalf("token %q came twice", tok)
		}
		seen[tok] = true
	}
}

func TestFormatProxyShowsTheTemplate(t *testing.T) {
	show := func(u *url.URL) string { return u.Redacted() }
	for in, want := range map[string]string{
		"http://user-{sticky}:pw@proxy.example:8080": "http://user-{sticky}:xxxxx@proxy.example:8080",
		"socks5://u:pw-{sticky}@proxy.example:1080":  "socks5://u:xxxxx@proxy.example:1080",
		"http://user:pw@proxy.example:8080":          "http://user:xxxxx@proxy.example:8080",
	} {
		if got, err := FormatProxy(in, show); err != nil || got != want {
			t.Errorf("FormatProxy(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := FormatProxy("http://u:p@[::1", show); err == nil {
		t.Error("FormatProxy took a broken URL")
	}
}

func TestStickyTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := New("google")
	p.Proxy = "http://user-{sticky}:pw@proxy.example:8080"
	created, err := s.Create(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	tok := created.StickyToken
	if !stickyRE.MatchString(tok) {
		t.Fatalf("a new profile's token is %q", tok)
	}
	got, _ := s.Get(ctx, "google")
	if got.StickyToken != tok || got.Proxy != p.Proxy {
		t.Fatalf("stored %+v", got)
	}

	// Editing the proxy, dropping the placeholder and adding it back,
	// or an Update that doesn't carry the token: the token stays.
	for _, proxy := range []string{"socks5://u:pw@other.example:1080", "", "http://x-{sticky}:pw@proxy.example:8080"} {
		got.Proxy = proxy
		got.StickyToken = ""
		if got, err = s.Update(ctx, got); err != nil || got.StickyToken != tok || got.Proxy != proxy {
			t.Fatalf("after Update to %q: %+v, %v", proxy, got, err)
		}
	}
	if again, err := s.StickyToken(ctx, "google"); err != nil || again != tok {
		t.Fatalf("StickyToken = %q, %v; want %q", again, err, tok)
	}

	if err := s.NewExit(ctx, "google"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "google")
	if got.StickyToken == tok || !stickyRE.MatchString(got.StickyToken) {
		t.Fatalf("after NewExit the token is %q (was %q)", got.StickyToken, tok)
	}
	if err := s.NewExit(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("NewExit(missing) = %v", err)
	}
	if _, err := s.StickyToken(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("StickyToken(missing) = %v", err)
	}
}

// TestCreateKeepsAGivenToken: an imported profile keeps the token its
// archive carried, even one another profile here already has.
func TestCreateKeepsAGivenToken(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, name := range []string{"bank", "bank-copy"} {
		p := New(name)
		p.StickyToken = "abcd1234"
		if got, err := s.Create(ctx, p); err != nil || got.StickyToken != "abcd1234" {
			t.Fatalf("%s: %+v, %v", name, got, err)
		}
	}
	p := New("bad")
	p.StickyToken = "NOT-OK"
	if _, err := s.Create(ctx, p); err == nil || strings.Contains(err.Error(), "NOT-OK") {
		t.Fatalf("Create with a malformed token = %v", err)
	}
}

// TestExistingProfilesGetATokenWhenNeeded: a profile from before tokens
// (an empty column, as the migration leaves it) still reads, and gets a
// token the first time one is asked for, which it then keeps.
func TestExistingProfilesGetATokenWhenNeeded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := New("old")
	p.Proxy = "http://user-{sticky}:pw@proxy.example:8080"
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET sticky_token = '' WHERE name = 'old'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "old")
	if err != nil || got.StickyToken != "" || got.Proxy != p.Proxy {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if list, err := s.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	tok, err := s.StickyToken(ctx, "old")
	if err != nil || !stickyRE.MatchString(tok) {
		t.Fatalf("StickyToken = %q, %v", tok, err)
	}
	if again, _ := s.StickyToken(ctx, "old"); again != tok {
		t.Fatalf("the token changed from %q to %q", tok, again)
	}
	if got, _ := s.Get(ctx, "old"); got.StickyToken != tok {
		t.Fatalf("stored token %q, want %q", got.StickyToken, tok)
	}
}
