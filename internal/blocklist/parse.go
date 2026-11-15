// Package blocklist keeps the ad and tracker domain lists the browsers'
// egress forwarders block (PROJ-119): it downloads them into the data dir,
// refreshes them every few days with conditional requests, and compiles
// them into one internal/domainset set, which each browser container gets
// as a file.
//
// Lists come from the internet, so parsing them is bounded: lines over
// maxLine bytes are skipped, a list over maxListBytes isn't read, the
// compiled set holds at most domainset.MaxNames names, and nothing a list
// says becomes a pattern: only whole names are kept.
package blocklist

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

const (
	// maxLine is the longest line read; a longer one is skipped and counted.
	maxLine = 1024
	// maxListBytes is the most a list may weigh, downloaded or on disk.
	maxListBytes = 32 << 20
)

// Stats counts what parsing a list kept and skipped.
type Stats struct {
	Lines    int // every line, blank and comment ones included
	Names    int // names kept (before duplicates across lists are removed)
	Comments int // blank lines and comments
	Skipped  int // lines in neither form: element hiding, paths, options, exceptions, other addresses
	Invalid  int // lines in a known form whose name isn't one we keep (a public suffix, an IP address, a bad name)
	Long     int // lines over maxLine bytes
}

func (s *Stats) add(o Stats) {
	s.Lines += o.Lines
	s.Names += o.Names
	s.Comments += o.Comments
	s.Skipped += o.Skipped
	s.Invalid += o.Invalid
	s.Long += o.Long
}

// Parse reads a list and calls add with each name it blocks. It knows
//
//   - the hosts format, "0.0.0.0 ads.example.com" (also 127.0.0.1, :: and
//     ::1), with one or more names a line and # comments;
//   - Adblock Plus rules of the exact form "||ads.example.com^", with !
//     comments and a [Adblock Plus …] header;
//   - a bare name alone on its line.
//
// Every other line is skipped and counted: an ABP rule with options
// ($third-party), a path or a wildcard can't be applied by name alone,
// and an exception (@@) can't be either. Names in Unicode are converted to
// punycode. Public suffixes (com, co.uk, github.io…) are refused, so a
// list can't block a whole top-level domain. Parse never panics and holds
// at most maxLine bytes of a line.
func Parse(r io.Reader, add func(name string)) (Stats, error) {
	var st Stats
	keep := func(raw string) {
		if name, ok := clean(raw); ok {
			st.Names++
			add(name)
		} else {
			st.Invalid++
		}
	}
	long, err := scanLines(r, maxLine, func(b []byte) {
		st.Lines++
		line := strings.TrimSpace(string(b))
		switch {
		case line == "", line[0] == '#', line[0] == '!', line[0] == '[':
			st.Comments++
		case strings.HasPrefix(line, "||"):
			name, ok := strings.CutSuffix(line[2:], "^")
			if !ok || name == "" || strings.ContainsAny(name, "/*^$|@#:?=&,~") {
				st.Skipped++
				return
			}
			keep(name)
		default:
			if i := strings.IndexByte(line, '#'); i >= 0 {
				if c := line[i-1]; c != ' ' && c != '\t' {
					// Not a comment: element hiding (example.com##.ad).
					st.Skipped++
					return
				}
				line = line[:i]
			}
			f := strings.Fields(line)
			if len(f) == 1 && !strings.ContainsAny(f[0], "/*^$|@:?=&,~") {
				keep(f[0]) // a bare name
				return
			}
			if len(f) < 2 {
				st.Skipped++
				return
			}
			ip, err := netip.ParseAddr(f[0])
			if err != nil || (!ip.IsUnspecified() && !ip.IsLoopback()) {
				// An address that sends the name somewhere real isn't a block.
				st.Skipped++
				return
			}
			for _, n := range f[1:] {
				keep(n)
			}
		}
	})
	st.Lines += long
	st.Long = long
	return st, err
}

// clean returns raw as a name the set keeps, or false: valid, at least two
// labels, in punycode, not an IP address, not a public suffix.
func clean(raw string) (string, bool) {
	if !utf8.ValidString(raw) {
		return "", false
	}
	if !isASCII(raw) {
		// idna's Lookup profile also maps case and width.
		a, err := idna.Lookup.ToASCII(raw)
		if err != nil {
			return "", false
		}
		raw = a
	}
	name, ok := domainset.Normalize(raw)
	if !ok {
		return "", false
	}
	if suffix, _ := publicsuffix.PublicSuffix(name); suffix == name {
		return "", false
	}
	return name, true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// scanLines calls line with each line of r, without its line ending.
// Lines over maxLen bytes are skipped whole and counted in long; no more
// than maxLen bytes of one are held.
func scanLines(r io.Reader, maxLen int, line func([]byte)) (long int, err error) {
	br := bufio.NewReaderSize(r, maxLen+2) // room for "\r\n"
	for {
		b, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long++
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			if errors.Is(err, io.EOF) {
				return long, nil
			}
			if err != nil {
				return long, err
			}
			continue
		}
		if len(b) > 0 {
			b = bytes.TrimSuffix(bytes.TrimSuffix(b, []byte("\n")), []byte("\r"))
			if len(b) > maxLen {
				long++
			} else {
				line(b)
			}
		}
		if errors.Is(err, io.EOF) {
			return long, nil
		}
		if err != nil {
			return long, err
		}
	}
}

// compiler gathers names from several lists into one set: the first
// domainset.MaxNames distinct names, in the order the lists give them.
type compiler struct {
	seen map[string]struct{}
	over int // distinct names past the cap, dropped
}

func newCompiler() *compiler { return &compiler{seen: map[string]struct{}{}} }

func (c *compiler) add(name string) {
	if _, ok := c.seen[name]; ok {
		return
	}
	if len(c.seen) >= domainset.MaxNames {
		c.over++
		return
	}
	c.seen[name] = struct{}{}
}

func (c *compiler) set() *domainset.Set {
	names := make([]string, 0, len(c.seen))
	for n := range c.seen {
		names = append(names, n)
	}
	return domainset.Build(names)
}
