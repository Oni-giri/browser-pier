package pagetools

import (
	"context"
	"strings"
	"testing"
)

func TestNavigateSaysWhenItWasRedirected(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.fb.redirects = map[string]string{
		"https://example.com/secure":        "https://example.com/login",
		"https://example.com/docs":          "https://example.com/docs/",
		"https://Example.COM:443/a?q=a%20b": "https://example.com/a?q=a b#top",
	}
	nav := func(u string) string {
		t.Helper()
		out, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: u})
		if err != nil {
			t.Fatal(err)
		}
		f.closedAfter()
		return out
	}
	out := nav("https://example.com/secure")
	if !strings.Contains(out, "URL: https://example.com/login\n") || !strings.Contains(out, "Redirected from: https://example.com/secure\n") {
		t.Errorf("a redirect:\n%s", out)
	}
	if i, j := strings.Index(out, "Redirected from"), strings.LastIndex(out, "</untrusted-content-"); i > j {
		t.Errorf("the note is outside the fence:\n%s", out)
	}
	for _, u := range []string{"https://example.com/docs", "https://Example.COM:443/a?q=a%20b", "https://example.com/plain#section"} {
		if out := nav(u); strings.Contains(out, "Redirected") {
			t.Errorf("%s: a redirect that isn't one:\n%s", u, out)
		}
	}
	// Going back isn't a redirect, whatever the URL.
	out, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{Action: "back"})
	if err != nil || strings.Contains(out, "Redirected") {
		t.Errorf("back: %v\n%s", err, out)
	}
}

func TestSameAddress(t *testing.T) {
	for _, c := range []struct {
		requested, final string
		same             bool
	}{
		{"https://example.com", "https://example.com/", true},
		{"https://example.com/docs", "https://example.com/docs/", true},
		{"https://example.com/a#x", "https://example.com/a", true},
		{"https://example.com/a", "https://example.com/a#y", true},
		{"HTTPS://EXAMPLE.com/a", "https://example.com/a", true},
		{"http://example.com:80/", "http://example.com/", true},
		{"https://example.com:443/", "https://example.com/", true},
		{"https://example.com/a b?q=\"x\"", "https://example.com/a%20b?q=%22x%22", true},
		{"https://bücher.example/", "https://xn--bcher-kva.example/", true},
		{"https://example.com/secure", "https://example.com/login", false},
		{"https://example.com/", "https://www.example.com/", false},
		{"http://example.com/", "https://example.com/", false},
		{"https://example.com:8443/", "https://example.com/", false},
		{"https://example.com/?a=1", "https://example.com/?a=2", false},
		{"https://example.com/A", "https://example.com/a", false},
		{"https://bücher.example/", "https://example.com/", false},
		{"http://bücher.example/", "http://xn--evil.example/", false},
		{"http://BÜCHER.example/", "http://xn--bcher-kva.example/", true},
		{"http://bücher.example:8080/a", "http://xn--bcher-kva.example:8080/a", true},
		{"http://bücher.example/", "http://xn--bcher-kva.evil/", false},
		{"http://bücher.example/a", "http://xn--bcher-kva.example/b", false},
		{"http://bücher.example:8080/", "http://xn--bcher-kva.example/", false},
		{"http://www.bücher.example/", "http://xn--bcher-kva.example/", false},
		{"http://пример.рф/", "http://xn--e1afmkfd.xn--p1ai/", true},
		{"http://пример.рф/", "http://xn--evil-kva.xn--p1ai/", false},
	} {
		if got := sameAddress(c.requested, c.final); got != c.same {
			t.Errorf("sameAddress(%q, %q) = %t", c.requested, c.final, got)
		}
	}
}
