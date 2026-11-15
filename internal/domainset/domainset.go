// Package domainset is a compact set of DNS names that matches a host by
// its exact name or a parent domain, never by substring: with ads.com in
// the set, ads.com and x.ads.com match, notads.com doesn't (PROJ-119).
//
// The orchestrator builds one from ad and tracker lists (internal/blocklist)
// and hands it to each browser container as a file; the container's egress
// forwarder reads it back and refuses connections to what it matches. The
// file is the set's names, sorted, one per line, after a header line, so
// the forwarder keeps the file's bytes plus 4 bytes a name: a million
// names cost it about 25 MB.
//
// Nothing here compiles a pattern from list content: names are checked
// character by character and compared as strings.
package domainset

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sort"
	"strings"
)

// Caps on a set, so a list from the internet can't make a browser
// container read or keep more than this.
const (
	// MaxNames is the most names a set holds.
	MaxNames = 1_000_000
	// MaxFileBytes is the largest set file Read accepts: MaxNames names
	// of a typical length fit with room to spare.
	MaxFileBytes = 64 << 20
	// maxName is the longest DNS name (RFC 1035), maxLabel its longest label.
	maxName, maxLabel = 253, 63
)

// header is the set file's first line; Read refuses a file without it.
const header = "# browserpier domain set 1\n"

// Set is an immutable set of normalised DNS names. The zero value and nil
// are empty sets.
type Set struct {
	// data is every name followed by '\n', in ascending order; offs[i] is
	// where name i starts.
	data string
	offs []uint32
}

// Build makes a set of names, which must already be normalised (see
// Normalize); names that aren't are dropped. Duplicates are fine. At most
// MaxNames are kept: the first ones, in sorted order.
func Build(names []string) *Set {
	sorted := make([]string, 0, len(names))
	for _, n := range names {
		if m, ok := Normalize(n); ok && m == n {
			sorted = append(sorted, n)
		}
	}
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if len(sorted) > MaxNames {
		sorted = sorted[:MaxNames]
	}
	var b strings.Builder
	size := 0
	for _, n := range sorted {
		size += len(n) + 1
	}
	b.Grow(size)
	offs := make([]uint32, len(sorted))
	for i, n := range sorted {
		offs[i] = uint32(b.Len()) //nolint:gosec // at most MaxNames names of at most 254 bytes: under 2^32
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return &Set{data: b.String(), offs: offs}
}

// Len is how many names the set holds.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.offs)
}

func (s *Set) name(i int) string {
	end := len(s.data) - 1
	if i+1 < len(s.offs) {
		end = int(s.offs[i+1]) - 1
	}
	return s.data[s.offs[i]:end]
}

// Contains reports whether name, as given, is in the set.
func (s *Set) Contains(name string) bool {
	if s.Len() == 0 {
		return false
	}
	i := sort.Search(len(s.offs), func(i int) bool { return s.name(i) >= name })
	return i < len(s.offs) && s.name(i) == name
}

// Matches reports whether host or one of its parent domains is in the set.
// host is normalised first (case, a trailing dot); an IP address or a name
// that isn't a valid DNS name matches nothing.
func (s *Set) Matches(host string) bool {
	if s.Len() == 0 {
		return false
	}
	h, ok := Normalize(host)
	if !ok {
		return false
	}
	return matches(h, s.Contains)
}

// matches tries host and each parent domain of it with in.
func matches(host string, in func(string) bool) bool {
	for {
		if in(host) {
			return true
		}
		i := strings.IndexByte(host, '.')
		if i < 0 {
			return false
		}
		host = host[i+1:]
	}
}

// Names returns the set's names, in order.
func (s *Set) Names() []string {
	out := make([]string, s.Len())
	for i := range out {
		out[i] = s.name(i)
	}
	return out
}

// WriteTo writes the set file: the header, then each name on its own line.
func (s *Set) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, header)
	if err != nil || s == nil {
		return int64(n), err
	}
	m, err := io.WriteString(w, s.data)
	return int64(n + m), err
}

// Read reads a set file written by WriteTo. It accepts at most
// MaxFileBytes and MaxNames, and checks every line: each must be a
// normalised name, in strictly ascending order, so the file's bytes can be
// searched as they are.
func Read(r io.Reader) (*Set, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, fmt.Errorf("domain set: over %d MB", MaxFileBytes>>20)
	}
	if !bytes.HasPrefix(b, []byte(header)) {
		return nil, errors.New("domain set: not a BrowserPier domain set file")
	}
	data := string(b[len(header):])
	if data != "" && !strings.HasSuffix(data, "\n") {
		return nil, errors.New("domain set: the last line isn't complete")
	}
	var offs []uint32
	prev := ""
	for at := 0; at < len(data); {
		end := at + strings.IndexByte(data[at:], '\n')
		name := data[at:end]
		if n, ok := Normalize(name); !ok || n != name {
			return nil, fmt.Errorf("domain set: line %d isn't a normalised name", len(offs)+2)
		}
		if len(offs) > 0 && name <= prev {
			return nil, fmt.Errorf("domain set: line %d is out of order", len(offs)+2)
		}
		if len(offs) == MaxNames {
			return nil, fmt.Errorf("domain set: over %d names", MaxNames)
		}
		offs = append(offs, uint32(at)) //nolint:gosec // at < MaxFileBytes < 2^32
		prev, at = name, end+1
	}
	return &Set{data: data, offs: offs}, nil
}

// Normalize returns host as the set keeps names: lower case, without a
// trailing dot. ok is false for what isn't a DNS name of at least two
// labels made of ASCII letters, digits, '-' and '_' (some trackers' names
// have one), at most 253 bytes with labels of at most 63, or for what
// reads as an IP address (a last label of digits only, as browsers read
// one). Internationalised names must come as punycode (xn--…), which is
// how browsers send them.
func Normalize(host string) (string, bool) {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > maxName {
		return "", false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", false
	}
	var b []byte // allocated only when a letter needs lowering
	labelStart, dots := 0, 0
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		case c >= 'A' && c <= 'Z':
			if b == nil {
				b = []byte(host)
			}
			b[i] = c + 'a' - 'A'
		case c == '.':
			if i == labelStart || i-labelStart > maxLabel {
				return "", false
			}
			labelStart = i + 1
			dots++
		default:
			return "", false
		}
	}
	if labelStart == len(host) || len(host)-labelStart > maxLabel || dots == 0 {
		return "", false
	}
	if allDigits(host[labelStart:]) {
		return "", false
	}
	if b != nil {
		return string(b), true
	}
	return host, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// allow are names that are never blocked, nor their subdomains, whatever
// a list says: challenge and CAPTCHA providers, whose absence makes a
// site's check fail or look harder at the browser. It is built in: neither
// an agent nor a list can change it.
var allow = []string{
	"challenges.cloudflare.com", // Turnstile and Cloudflare's challenge pages
	"hcaptcha.com",
	"recaptcha.net",
	"www.gstatic.com",      // reCAPTCHA's scripts
	"www.google.com",       // reCAPTCHA's frames (www.google.com/recaptcha)
	"captcha-delivery.com", // DataDome's challenge
	"arkoselabs.com",       // Arkose Labs (FunCaptcha)
	"funcaptcha.com",       // Arkose Labs' older name
}

// Allowed reports whether host is on the built-in allowlist, or a
// subdomain of a name on it. host must be normalised.
func Allowed(host string) bool {
	return matches(host, func(n string) bool { return slices.Contains(allow, n) })
}

// Allowlist returns the built-in allowlist, for documentation and tests.
func Allowlist() []string { return slices.Clone(allow) }
