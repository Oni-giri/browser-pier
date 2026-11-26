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
		// Past ASCII, the query is sent as UTF-8 bytes, as a browser
		// sends it; escapes and delimiters stay as they are.
		"https://example.com/café?q=café&r=%C3%A9+x&s=日本#é": "https://example.com/caf%C3%A9?q=caf%C3%A9&r=%C3%A9+x&s=%E6%97%A5%E6%9C%AC#%C3%A9",
		"https://example.com/?q=😀%20a=b;c":                  "https://example.com/?q=%F0%9F%98%80%20a=b;c",
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
