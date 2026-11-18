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
	"unicode"
)

// MaxLen caps an address, before it is normalised.
const MaxLen = 8192

// Check refuses what pages must not be sent to: any scheme but http and
// https, and URLs carrying a user name or password. It returns the URL as
// Go writes it back (net/url), with spaces in its query percent-encoded
// too, which is what is opened.
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
	// Go escapes a space in the path and the fragment but keeps the
	// query as written; a browser sends it as %20, so do that here too:
	// the open action types the URL, where a space would end it.
	u.RawQuery = escapeSpaces(u.RawQuery)
	return u.String(), nil
}

// escapeSpaces percent-encodes every space character (Unicode's) in s.
func escapeSpaces(s string) string {
	if !strings.ContainsFunc(s, unicode.IsSpace) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
			continue
		}
		for _, c := range []byte(string(r)) {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
