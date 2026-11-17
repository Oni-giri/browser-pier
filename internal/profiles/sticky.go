package profiles

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// StickyPlaceholder, in a proxy URL's user name or password, becomes the
// profile's own sticky token when its browser starts (FillSticky). Many
// residential proxy providers keep a client on one exit IP for as long as
// it sends the same session code in its credentials, so each profile
// keeps an exit of its own while all of them share one proxy template.
const StickyPlaceholder = "{sticky}"

// stickyLen and stickyAlphabet shape a sticky token: short and plain
// enough for providers that take a session code in a user name.
const (
	stickyLen      = 8
	stickyAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var stickyRE = regexp.MustCompile(`^[a-z0-9]{8}$`)

// NewStickyToken returns a random token of 8 characters from a-z0-9.
// It is the profile's own and stands for nothing: never derived from the
// name, which would tell the proxy provider what the profile is for.
func NewStickyToken() string {
	b := make([]byte, stickyLen)
	out := make([]byte, 0, stickyLen)
	for len(out) < stickyLen {
		_, _ = rand.Read(b) // never fails (crypto/rand)
		for _, c := range b {
			// 252 is the largest multiple of 36 under 256: drop what
			// would bias the first letters.
			if c < 252 && len(out) < stickyLen {
				out = append(out, stickyAlphabet[c%36])
			}
		}
	}
	return string(out)
}

// HasSticky reports whether a proxy URL is a template holding the
// placeholder.
func HasSticky(proxy string) bool { return strings.Contains(proxy, StickyPlaceholder) }

// FillSticky returns the proxy URL with every placeholder in its user
// name and password replaced by token. Only the user info is touched; a
// URL without the placeholder comes back as it is, and the
// percent-encoded %7Bsticky%7D is not a placeholder. The orchestrator
// calls it only when it hands the proxy to a starting browser (and, with
// a sample token, to check a template): what is stored and shown stays
// the template.
func FillSticky(proxy, token string) string {
	return replaceInUserinfo(proxy, StickyPlaceholder, token)
}

// replaceInUserinfo replaces old with repl in the user info of a URL,
// split the way url.Parse splits it: the authority runs from "//" to the
// first '/', '?' or '#', and its user info ends at its last '@'.
func replaceInUserinfo(raw, old, repl string) string {
	if !strings.Contains(raw, old) {
		return raw
	}
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	start := i + len("://")
	rest := raw[start:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return raw
	}
	return raw[:start] + strings.ReplaceAll(rest[:at], old, repl) + rest[at:]
}

// sampleSticky fills a template when checking it: a token like any other.
const sampleSticky = "a0b1c2d3"

// maxSOCKS5Credential is the longest user name or password a SOCKS5
// proxy takes, in bytes (RFC 1929), which the browser's forwarder would
// only report when it connects. Basic authentication, for http and https,
// has no such limit.
const maxSOCKS5Credential = 255

// checkCredentials applies the limits a filled-in proxy URL must meet.
// Errors never echo the URL.
func checkCredentials(u *url.URL) error {
	if u.User == nil {
		return nil
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	if u.Scheme == "socks5" && (len(user) > maxSOCKS5Credential || len(pass) > maxSOCKS5Credential) {
		return errors.New("proxy: a SOCKS5 proxy's user name and password may have at most 255 bytes each")
	}
	if (u.Scheme == "http" || u.Scheme == "https") && strings.Contains(user, ":") {
		return errors.New("proxy: an http proxy's user name can't hold a colon")
	}
	return nil
}

// FormatProxy shows a proxy URL or template: show gets it parsed, and
// any placeholder in what show returns is put back as {sticky}, so a
// template is shown as a template, never filled in. A placeholder in the
// password goes wherever show puts the password.
func FormatProxy(raw string, show func(*url.URL) string) (string, error) {
	mark := ""
	if HasSticky(raw) {
		// A stand-in that url.Parse takes in a user name and nothing in
		// the URL can already hold.
		mark = "sticky" + NewStickyToken() + NewStickyToken()
		raw = replaceInUserinfo(raw, StickyPlaceholder, mark)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	out := show(u)
	if mark != "" {
		out = strings.ReplaceAll(out, mark, StickyPlaceholder)
	}
	return out, nil
}

// StickyToken returns the profile's sticky token, giving it one if it
// has none yet: a profile from before tokens, or imported from an archive
// without one. Once given, a token changes only with NewExit.
func (s *Store) StickyToken(ctx context.Context, name string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET sticky_token = ? WHERE name = ? AND sticky_token = ''`,
		NewStickyToken(), name); err != nil {
		return "", err
	}
	var tok string
	err := s.db.QueryRowContext(ctx, `SELECT sticky_token FROM profiles WHERE name = ?`, name).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tok, err
}

// NewExit gives the profile a new sticky token, so its proxy provider
// gives it a new exit at its next start. The caller makes sure its
// browser isn't running: the forwarder keeps its credentials for the
// whole run.
func (s *Store) NewExit(ctx context.Context, name string) error {
	old, err := s.StickyToken(ctx, name)
	if err != nil {
		return err
	}
	tok := NewStickyToken()
	for tok == old {
		tok = NewStickyToken()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE profiles SET sticky_token = ? WHERE name = ?`, tok, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
