package blocklist

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

func parseAll(t *testing.T, list string) ([]string, Stats) {
	t.Helper()
	var got []string
	st, err := Parse(strings.NewReader(list), func(n string) { got = append(got, n) })
	if err != nil {
		t.Fatal(err)
	}
	return got, st
}

func TestParseHostsFormat(t *testing.T) {
	got, st := parseAll(t, `# Peter Lowe's list, say
127.0.0.1 localhost
::1 localhost ip6-localhost
255.255.255.255 broadcasthost
0.0.0.0 ads.example.com
127.0.0.1	Tracker.Example.NET.  # tab, case, a trailing dot and a comment
0.0.0.0 one.example.org two.example.org
:: v6.example.org
192.168.1.10 printer.example.org
0.0.0.0 com
0.0.0.0 co.uk
0.0.0.0 1.2.3.4
0.0.0.0 bücher.de
0.0.0.0 xn--mnchen-3ya.de

`)
	want := []string{"ads.example.com", "tracker.example.net", "one.example.org", "two.example.org", "v6.example.org", "xn--bcher-kva.de", "xn--mnchen-3ya.de"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// localhost twice and ip6-localhost: one label; com, co.uk: public
	// suffixes; 1.2.3.4: an address.
	if st.Invalid != 6 {
		t.Errorf("invalid %d, want 6: %+v", st.Invalid, st)
	}
	// broadcasthost and the printer: real addresses, not blocks.
	if st.Skipped != 2 {
		t.Errorf("skipped %d, want 2: %+v", st.Skipped, st)
	}
	if st.Comments != 2 || st.Lines != 15 {
		t.Errorf("%+v", st)
	}
}

func TestParseABP(t *testing.T) {
	got, st := parseAll(t, `[Adblock Plus 2.0]
! Title: EasyPrivacy
||tracker.example.com^
||Metrics.Example.ORG^
||third.example.com^$third-party
||example.com/pixel.gif
||*.wild.example.com^
||ads.example.net^|
@@||allowed.example.com^
example.com##.ad-banner
/banner/ad.js
|https://ads.example.com^
||com^
||^
||bücher.de^
plain.example.com
`)
	want := []string{"tracker.example.com", "metrics.example.org", "xn--bcher-kva.de", "plain.example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if st.Skipped != 9 || st.Invalid != 1 || st.Comments != 2 {
		t.Errorf("%+v", st)
	}
}

func TestParseOversizedLine(t *testing.T) {
	list := "0.0.0.0 a.example.com\n0.0.0.0 " + strings.Repeat("x", 5000) + ".com\n0.0.0.0 b.example.com\n" + strings.Repeat("y", 3000)
	got, st := parseAll(t, list)
	if !slices.Equal(got, []string{"a.example.com", "b.example.com"}) {
		t.Errorf("got %q", got)
	}
	if st.Long != 2 || st.Lines != 4 {
		t.Errorf("%+v", st)
	}
	// A line exactly at the cap is read; with CRLF too.
	name := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + ".com"
	line := "0.0.0.0 " + name + strings.Repeat(" ", maxLine-len("0.0.0.0 ")-len(name))
	got, st = parseAll(t, line+"\r\n")
	if len(got) != 1 || st.Long != 0 {
		t.Errorf("a line of exactly %d bytes: %q %+v", maxLine, got, st)
	}
}

// TestParseLargeList: two million lines are read within the caps the
// package states: in a few seconds, holding no more than the set.
func TestParseLargeList(t *testing.T) {
	if testing.Short() {
		t.Skip("two million lines")
	}
	const lines = 2_000_000
	r := &generated{n: lines}
	c := newCompiler()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	st, err := Parse(r, c.add)
	if err != nil {
		t.Fatal(err)
	}
	set := c.set()
	took := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if st.Lines != lines {
		t.Fatalf("read %d lines", st.Lines)
	}
	// Three lines in four are distinct names: 1.5 million, over the cap.
	if set.Len() != domainset.MaxNames || c.over != lines*3/4-domainset.MaxNames {
		t.Errorf("kept %d names and dropped %d, want the cap %d and the rest", set.Len(), c.over, domainset.MaxNames)
	}
	limit := 60 * time.Second // generous: -race slows parsing several times
	if took > limit {
		t.Errorf("parsing took %s, over %s", took, limit)
	}
	// What stays is the set: its bytes, 4 a name, and the compiler's map.
	held := int64(after.HeapInuse) - int64(before.HeapInuse)
	if held > 400<<20 {
		t.Errorf("parsing holds %d MB", held>>20)
	}
	runtime.KeepAlive(c)
	t.Logf("2M lines in %s; %d names kept, %d over the cap; heap +%d MB", took, set.Len(), c.over, held>>20)
}

// generated is a list of n lines, three in four a hosts line with a
// distinct name and the fourth a comment, made as it is read.
type generated struct {
	n, i int
	buf  bytes.Buffer
}

func (g *generated) Read(p []byte) (int, error) {
	for g.buf.Len() < len(p) && g.i < g.n {
		switch g.i % 4 {
		case 0, 1, 2:
			fmt.Fprintf(&g.buf, "0.0.0.0 host%d.tracker-example.com\n", g.i)
		default:
			g.buf.WriteString("# a comment\n")
		}
		g.i++
	}
	if g.buf.Len() == 0 {
		return 0, io.EOF
	}
	return g.buf.Read(p)
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("0.0.0.0 ads.example.com\n||t.example.com^\n# x\n"))
	f.Add([]byte("||bücher.de^\n:: a.b\n"))
	f.Add([]byte(strings.Repeat("z", 2000) + "\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Parse(bytes.NewReader(b), func(n string) {
			if m, ok := domainset.Normalize(n); !ok || m != n {
				t.Fatalf("kept %q, which isn't a normalised name", n)
			}
		})
	})
}
