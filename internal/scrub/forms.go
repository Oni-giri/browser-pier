package scrub

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// forms is every way a secret may show up in text: as typed, and as the
// encoders pages, servers and BrowserPier itself use would write it. New
// matches each in its normalized shape (normalize.go), which already
// stands for every HTML-escaped, Markdown-escaped or whitespace-changed
// variant, so those need no form here. Duplicates are removed by the
// caller.
func forms(s string) []string {
	out := []string{s}
	// JSON string contents, as JSON.stringify writes them, with "/"
	// escaped (PHP), HTML-safe (Go's encoding/json: \u003c, \u0026…) and
	// ASCII-only (Python's json.dumps, PHP, the X-Downloads header), with
	// lowercase hex or uppercase (Jackson, .NET); once, and twice for JSON
	// inside a JSON string.
	for _, o := range []jsonOpts{
		{}, {slash: true}, {html: true}, {ascii: true}, {slash: true, ascii: true}, {html: true, ascii: true},
		{slash: true, html: true}, {slash: true, html: true, ascii: true},
		{html: true, upper: true}, {ascii: true, upper: true}, {slash: true, ascii: true, upper: true},
		{html: true, ascii: true, upper: true}, {slash: true, html: true, upper: true}, {slash: true, html: true, ascii: true, upper: true},
	} {
		once := jsonEscape(s, o)
		out = append(out, once, jsonEscape(once, o))
	}
	// Percent-encodings: encodeURIComponent; application/x-www-form-
	// urlencoded as browsers submit a form (+ for a space); Go's
	// QueryEscape, RFC 3986's unreserved set alone (Python's quote) and a
	// path segment as RFC 3986 allows it (Go's PathEscape); and
	// what a browser itself escapes in a URL's query, path and fragment
	// when a page puts the raw value there. Each with uppercase hex, as
	// browsers write it, and lowercase, as some servers do; and each
	// encoded again, by encodeURIComponent or a form, as a URL carried in
	// another URL's query (?next=…) is.
	once := []string{
		percent(s, keepURIComponent, false),
		percent(s, keepForm, true),
		percent(s, keepUnreserved, true),
		percent(s, keepUnreserved, false),
		percent(s, keepSegment, false),
		percent(s, keepQuery, false),
		percent(s, keepPath, false),
		percent(s, keepFragment, false),
	}
	for _, p := range once {
		out = append(out, p, lowerHex(p), percent(p, keepURIComponent, false), percent(p, keepForm, true))
	}
	return out
}

type jsonOpts struct {
	slash bool // "/" as "\/"
	html  bool // <, >, & as \u003c, \u003e, \u0026
	ascii bool // everything past ASCII as \uXXXX
	upper bool // \uXXXX with uppercase hex
}

// jsonEscape is s as the inside of a JSON string. U+2028 and U+2029 are
// escaped (\u2028) in the HTML-safe form, as encoding/json does.
func jsonEscape(s string, o jsonOpts) string {
	u := `\u%04x`
	if o.upper {
		u = `\u%04X`
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '/' && o.slash:
			b.WriteString(`\/`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20, o.html && (r == '<' || r == '>' || r == '&'), o.html && (r == 0x2028 || r == 0x2029):
			fmt.Fprintf(&b, u, r)
		case r >= utf8.RuneSelf && o.ascii:
			if r > 0xFFFF {
				r -= 0x10000
				fmt.Fprintf(&b, u+u, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			} else {
				fmt.Fprintf(&b, u, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Bytes kept as they are by each percent-encoding, besides ASCII letters
// and digits (always kept, except by the browser's own sets below, which
// keep everything printable they don't list).
const (
	keepURIComponent = "-_.!~*'()"
	keepForm         = "*-._"
	keepUnreserved   = "-_.~"
	keepSegment      = "-_.~$&+,:;=@"
)

// The browser's own sets list what it escapes, not what it keeps.
var (
	keepQuery    = escapeOnly(" \"#<>'")
	keepPath     = escapeOnly(" \"#<>?`{}")
	keepFragment = escapeOnly(" \"<>`")
)

// escapeOnly is a keep-set of every printable ASCII byte but those in esc
// ("%" is kept: a browser leaves it as it is in a URL).
func escapeOnly(esc string) string {
	var b strings.Builder
	for c := byte(0x21); c < 0x7F; c++ {
		if !strings.ContainsRune(esc, rune(c)) && !isAlnum(c) {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// percent percent-encodes s's UTF-8 bytes (uppercase hex, as browsers
// write it), keeping letters, digits and the bytes in keep; plus writes a
// space as "+".
func percent(s, keep string, plus bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlnum(c) || (c < utf8.RuneSelf && c != 0 && strings.IndexByte(keep, c) >= 0):
			b.WriteByte(c)
		case c == ' ' && plus:
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// lowerHex is a percent-encoding with its hex digits in lowercase.
func lowerHex(p string) string {
	b := []byte(p)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' && hexVal(b[i+1]) >= 0 && hexVal(b[i+2]) >= 0 {
			b[i+1], b[i+2] = lower(b[i+1]), lower(b[i+2])
			i += 2
		}
	}
	return string(b)
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
