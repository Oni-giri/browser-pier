// Package weburl checks the addresses BrowserPier opens for an agent: the
// page tools' navigate and new tab (API mode) and the open action (Visual
// mode), so they all take the same ones. The browser image's entrypoint
// imports it too, so keep it to the standard library.
package weburl

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// MaxLen caps an address, before it is normalised.
const MaxLen = 8192

// Check refuses what pages must not be sent to: any scheme but http and
// https, and URLs carrying a user name or password. It returns the URL as
// Go writes it back (net/url), with spaces and anything past ASCII in its
// query percent-encoded too, which is what is opened.
func Check(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return "", errors.New("url: not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("url: only http and https URLs can be opened")
	case u.User != nil:
		return "", errors.New("url: URLs with a user name or password (user:pass@) aren't opened; sign in on the page")
	case u.Host == "":
		return "", errors.New("url: no host")
	case len(raw) > MaxLen:
		return "", errors.New("url: too long")
	}
	// Go escapes a space and anything past ASCII in the path and the
	// fragment but keeps the query as written; a browser sends them as
	// UTF-8 bytes, %20 and %C3%A9, so do that here too: the open action
	// types the URL, where a space would end it, and xdotool's typing can
	// drop a character past ASCII (é, say) on its way into Chromium.
	u.RawQuery = escapeQuery(u.RawQuery)
	return u.String(), nil
}

// escapeQuery percent-encodes each byte of every space and every
// character past ASCII in s, as UTF-8. What is already escaped, and the
// delimiters (&, =, +…), stay as they are.
func escapeQuery(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r == ' ' || r >= utf8.RuneSelf }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != ' ' && c < utf8.RuneSelf {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
