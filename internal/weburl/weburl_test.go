package weburl

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for raw, want := range map[string]string{
		"https://example.com/":                  "https://example.com/",
		" http://example.com/a b?q=1#top ":      "http://example.com/a%20b?q=1#top",
		"HTTPS://Example.com:8443/x":            "https://Example.com:8443/x",
		"https://example.com/?q=a b&r=c\u00a0d": "https://example.com/?q=a%20b&r=c%C2%A0d",
	} {
		if got, err := Check(raw); err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", raw, got, err, want)
		}
	}
	for raw, why := range map[string]string{
		"%zz":                      "not a URL",
		"example.com":              "only http and https",
		"javascript:alert(1)":      "only http and https",
		"file:///etc/passwd":       "only http and https",
		"https://u:p@example.com/": "user name or password",
		"https:///x":               "no host",
		"https://example.com/" + strings.Repeat("a", MaxLen): "too long",
	} {
		if _, err := Check(raw); err == nil || !strings.Contains(err.Error(), why) || !strings.HasPrefix(err.Error(), "url: ") {
			t.Errorf("%.40q: %v, want %q", raw, err, why)
		}
	}
}
