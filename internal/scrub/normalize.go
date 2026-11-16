package scrub

import (
	"strings"
	"unicode/utf8"
)

// Text and patterns are both matched in a normalized shape, so the escapes
// that writers mix freely don't each need a form of their own:
//
//   - an HTML character reference, numeric (&#47; &#x2F; &#x2f; &#0047;)
//     or one of the named ones encoders use (&amp; &lt; &gt; &quot; &apos;
//     &nbsp;), stands for its character: HTML encoders mix them at will
//     (mustache writes "/" as &#x2F; beside &amp;, Go's html/template "+"
//     as &#43;, .NET "é" as &#233;);
//   - a backslash before ASCII punctuation stands for that punctuation:
//     Markdown's escapes (page_content's \_ \* \[ \# 1\.), JSON's (\" \\
//     \/) and JavaScript's (\') alike;
//   - a run of whitespace, no-break spaces included, is one space, as
//     Markdown and the reader view collapse it.
//
// Normalizing only ever turns a few bytes into fewer, so the scan maps
// each normalized byte back to where it came from (matcher.find) and the
// text is never copied. What the shapes lose (which of two writers' escapes
// it was) only ever widens what is hidden.

// Byte kinds for norm: 0 is a byte that stands for itself.
const (
	kindSpace     = 1 + iota // whitespace: collapses with its neighbours
	kindAmp                  // may start a character reference
	kindBackslash            // may escape punctuation
	kindNBSPLead             // 0xC2, the first byte of a no-break space
)

var normKind = func() (k [256]byte) {
	for _, c := range []byte(" \t\n\r\f") {
		k[c] = kindSpace
	}
	k['&'] = kindAmp
	k['\\'] = kindBackslash
	k[0xC2] = kindNBSPLead
	return k
}()

// norm is what text[i:] starts with once normalized: the rune it stands
// for (' ' for any whitespace) and the index after it; r < 0 when the
// byte text[i] stands for itself. Only called on bytes with a normKind.
func norm(text string, i int) (r rune, next int) {
	switch normKind[text[i]] {
	case kindSpace:
		return ' ', i + 1
	case kindNBSPLead:
		if i+1 < len(text) && text[i+1] == 0xA0 {
			return ' ', i + 2
		}
	case kindBackslash:
		if i+1 < len(text) && isPunct(text[i+1]) {
			return rune(text[i+1]), i + 2
		}
	case kindAmp:
		if r, n := charRef(text[i:]); n > 0 {
			if r == 0xA0 || r < utf8.RuneSelf && normKind[r] == kindSpace {
				r = ' '
			}
			return r, i + n
		}
	}
	return -1, i + 1
}

func isPunct(c byte) bool {
	return c > ' ' && c < 0x7F && !isAlnum(c)
}

// named are the character references encoders write by name.
var named = map[string]rune{"amp": '&', "lt": '<', "gt": '>', "quot": '"', "apos": '\'', "nbsp": 0xA0}

// charRef reads the character reference s starts with ("&...;"), giving
// its character and length, or n == 0 when it doesn't start with one.
func charRef(s string) (r rune, n int) {
	end := strings.IndexByte(s[:min(len(s), 12)], ';')
	if end < 3 {
		return 0, 0
	}
	body := s[1:end]
	if body[0] != '#' {
		if r, ok := named[body]; ok {
			return r, end + 1
		}
		return 0, 0
	}
	digits, base := body[1:], rune(10)
	if digits != "" && (digits[0] == 'x' || digits[0] == 'X') {
		digits, base = digits[1:], 16
	}
	if digits == "" {
		return 0, 0
	}
	for i := 0; i < len(digits); i++ {
		d := hexVal(digits[i])
		if d < 0 || d >= base {
			return 0, 0
		}
		r = r*base + d
	}
	if r == 0 || r > utf8.MaxRune || r >= 0xD800 && r <= 0xDFFF {
		return 0, 0
	}
	return r, end + 1
}

func hexVal(c byte) rune {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0')
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10
	}
	return -1
}

// normalize is s in the normalized shape, as the scan sees text.
func normalize(s string) string {
	var b strings.Builder
	space := false
	for i := 0; i < len(s); {
		if normKind[s[i]] == 0 {
			b.WriteByte(s[i])
			space = false
			i++
			continue
		}
		r, next := norm(s, i)
		switch {
		case r == ' ':
			if !space {
				b.WriteByte(' ')
			}
			space = true
		case r < 0:
			b.WriteByte(s[i])
			space = false
		default:
			b.WriteRune(r)
			space = false
		}
		i = next
	}
	return b.String()
}

// loneBackslash reports whether s ends with an unpaired backslash, one the
// text's next byte could pair with: "ab\" followed by "." reads as "ab.".
func loneBackslash(s string) bool {
	n := len(s) - len(strings.TrimRight(s, `\`))
	return n%2 == 1
}
