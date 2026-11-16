package domainset

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestMatchesHostAndParentsOnly(t *testing.T) {
	s := Build([]string{"ads.com", "track.example.org", "doubleclick.net"})
	for host, want := range map[string]bool{
		"ads.com":             true,
		"x.ads.com":           true,
		"a.b.c.ads.com":       true,
		"notads.com":          false, // never a substring
		"ads.com.evil.net":    false,
		"com":                 false,
		"example.org":         false, // a parent of a listed name isn't listed
		"track.example.org":   true,
		"x.track.example.org": true,
		"ADS.COM":             true, // case
		"X.Ads.Com.":          true, // and a trailing dot
		"ads.com..":           false,
		"1.2.3.4":             false, // IP literals: not names
		"::1":                 false,
		"ads.com:443":         false, // not a name
		"":                    false,
	} {
		if got := s.Matches(host); got != want {
			t.Errorf("Matches(%q) = %v, want %v", host, got, want)
		}
	}
	var empty *Set
	if empty.Matches("ads.com") || empty.Len() != 0 {
		t.Error("a nil set matched")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM.":                    "example.com",
		"a_b.example.com":                 "a_b.example.com",
		"xn--bcher-kva.de":                "xn--bcher-kva.de",
		"example":                         "", // one label
		"example..com":                    "",
		".example.com":                    "",
		"exa mple.com":                    "",
		"exa*mple.com":                    "",
		"bücher.de":                       "", // punycode only
		"10.0.0.1":                        "",
		"example.123":                     "", // reads as an IP address
		"[::1]":                           "",
		strings.Repeat("a", 64) + ".com":  "",
		strings.Repeat("a.", 127) + "com": "",
	} {
		got, ok := Normalize(in)
		if want == "" && ok {
			t.Errorf("Normalize(%q) = %q, want refused", in, got)
		}
		if want != "" && (got != want || !ok) {
			t.Errorf("Normalize(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
}

func TestFileRoundTrip(t *testing.T) {
	s := Build([]string{"b.com", "a.com", "c.org", "a.com", "Not.Normal.com", "bad name"})
	if s.Len() != 3 {
		t.Fatalf("Len %d, want 3 (duplicates and unnormalised names dropped): %v", s.Len(), s.Names())
	}
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	r, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Names(), " "); got != "a.com b.com c.org" {
		t.Errorf("read back %q", got)
	}
	if !r.Matches("x.c.org") || r.Matches("d.com") {
		t.Error("the read set matches wrongly")
	}
}

func TestReadRefusesBadFiles(t *testing.T) {
	for name, body := range map[string]string{
		"no header":      "a.com\n",
		"unsorted":       header + "b.com\na.com\n",
		"duplicate":      header + "a.com\na.com\n",
		"not normalised": header + "A.com\n",
		"not a name":     header + "a.com\n1.2.3.4\n",
		"cut short":      header + "a.com\nb.co",
		"empty line":     header + "a.com\n\nb.com\n",
	} {
		if _, err := Read(strings.NewReader(body)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	big := header + strings.Repeat("x", MaxFileBytes)
	if _, err := Read(strings.NewReader(big)); err == nil {
		t.Error("read a file over the cap")
	}
	if s, err := Read(strings.NewReader(header)); err != nil || s.Len() != 0 {
		t.Errorf("empty set: %v, %d", err, s.Len())
	}
}

func TestBuildCapsNames(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a million names")
	}
	names := make([]string, MaxNames+10)
	for i := range names {
		names[i] = fmt.Sprintf("n%d.example.com", i)
	}
	s := Build(names)
	if s.Len() != MaxNames {
		t.Fatalf("Len %d, want %d", s.Len(), MaxNames)
	}
	var buf bytes.Buffer
	_, _ = s.WriteTo(&buf)
	if buf.Len() > MaxFileBytes {
		t.Fatalf("a full set's file is %d bytes, over the read cap", buf.Len())
	}
	r, err := Read(&buf)
	if err != nil || r.Len() != MaxNames {
		t.Fatalf("read back: %v, %d", err, r.Len())
	}
	if !r.Matches("x.n5.example.com") {
		t.Error("a full set doesn't match")
	}
}

func TestAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"challenges.cloudflare.com":   true,
		"x.challenges.cloudflare.com": true,
		"cloudflare.com":              false,
		"js.hcaptcha.com":             true,
		"www.recaptcha.net":           true,
		"www.gstatic.com":             true,
		"ssl.gstatic.com":             false,
		"www.google.com":              true,
		"ads.google.com":              false,
		"nothcaptcha.com":             false,
	} {
		if got := Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func FuzzRead(f *testing.F) {
	f.Add([]byte(header + "a.com\nb.com\n"))
	f.Add([]byte(header + "\n"))
	f.Add([]byte("junk"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := Read(bytes.NewReader(b))
		if err != nil {
			return
		}
		for _, n := range s.Names() {
			if !s.Matches(n) {
				t.Fatalf("%q read but doesn't match", n)
			}
		}
	})
}

func FuzzNormalize(f *testing.F) {
	f.Add("Example.COM.")
	f.Add("a..b")
	f.Fuzz(func(t *testing.T, s string) {
		n, ok := Normalize(s)
		if !ok {
			return
		}
		if m, ok := Normalize(n); !ok || m != n {
			t.Fatalf("Normalize(%q) = %q, which normalises to %q, %v", s, n, m, ok)
		}
	})
}

func TestPublicSuffix(t *testing.T) {
	for host, want := range map[string]bool{"co.uk": true, "com": true, "github.io": true, "example.co.uk": false, "example.com": false, "user.github.io": false, "ads.example.com": false, "foo.unknowntld": false} {
		if got := PublicSuffix(host); got != want {
			t.Errorf("PublicSuffix(%q) = %v, want %v", host, got, want)
		}
	}
}
