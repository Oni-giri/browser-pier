// Package weburl checks the addresses BrowserPier opens for an agent: the
// page tools' navigate and new tab (API mode) and the open action (Visual
// mode), so they all take the same ones. The browser image's entrypoint
// imports it too, so keep it to the standard library.
package weburl

import (
	"errors"
	"net/url"
	"strings"
)

// MaxLen caps an address, before it is normalised.
const MaxLen = 8192

// Check refuses what pages must not be sent to: any scheme but http and
// https, and URLs carrying a user name or password. It returns the URL as
// Go writes it back (net/url), which is what is opened.
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
	return u.String(), nil
}
