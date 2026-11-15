package scrub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// secret holds a space, "/", '"', "&", "+", "%", "'", "<", a non-ASCII
// letter, an emoji and what Markdown escapes ("_*[`]").
const secret = `p@ss w/rd"&+%'<é🙂x_*[` + "`]"

// mdSecret is secret as page_content's Markdown writes it.
const mdSecret = `p@ss w/rd"&+%'<é🙂x\_\*\[\` + "`" + `\]`

// entities is s with every character but ASCII letters and digits as a
// numeric character reference, in format (&#%d; &#x%x; &#x%X;).
func entities(s, format string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, format, r)
	}
	return b.String()
}

// goJSON is s inside a JSON string as encoding/json writes it.
func goJSON(s string, escapeHTML bool) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(escapeHTML)
	_ = enc.Encode(s)
	out := strings.TrimSpace(b.String())
	return out[1 : len(out)-1]
}

func TestEveryEncodingIsHidden(t *testing.T) {
	once := goJSON(secret, false)
	cases := map[string]string{
		"raw":                   secret,
		"JSON":                  once,
		"JSON, Go's HTML-safe":  goJSON(secret, true),
		`JSON with \/`:          strings.ReplaceAll(once, "/", `\/`),
		"JSON twice":            goJSON(once, false),
		`JSON twice with \/`:    strings.ReplaceAll(goJSON(strings.ReplaceAll(once, "/", `\/`), false), "/", `\/`),
		"JSON twice, HTML-safe": goJSON(goJSON(secret, true), true),
		"JSON, ASCII only":      `p@ss w/rd\"&+%'<\u00e9\ud83d\ude42x_*[` + "`]",
		"JSON, ASCII, upper":    `p@ss w/rd\"&+%'<\u00E9\uD83D\uDE42x_*[` + "`]",
		"JSON, HTML-safe upper": `p@ss w/rd\"\u0026+%'\u003Cé🙂x_*[` + "`]",
		"encodeURIComponent":    `p%40ss%20w%2Frd%22%26%2B%25'%3C%C3%A9%F0%9F%99%82x_*%5B%60%5D`,
		"form-urlencoded":       `p%40ss+w%2Frd%22%26%2B%25%27%3C%C3%A9%F0%9F%99%82x_*%5B%60%5D`,
		"form, lowercase hex":   `p%40ss+w%2frd%22%26%2b%25%27%3c%c3%a9%f0%9f%99%82x_*%5b%60%5d`,
		"Go QueryEscape":        url.QueryEscape(secret),
		"QueryEscape twice":     url.QueryEscape(url.QueryEscape(secret)),
		"Go PathEscape":         url.PathEscape(secret),
		"url.Values":            strings.TrimPrefix(url.Values{"p": {secret}}.Encode(), "p="),
		"a URL's query":         `p@ss%20w/rd%22&+%%27%3C%C3%A9%F0%9F%99%82x_*[` + "`]",
		"HTML, Go":              html.EscapeString(secret),
		"HTML text":             `p@ss w/rd"&amp;+%'&lt;é🙂x_*[` + "`]",
		"HTML &quot; &#x27;":    `p@ss w/rd&quot;&amp;+%&#x27;&lt;é🙂x_*[` + "`]",
		"HTML &quot; &#039;":    `p@ss w/rd&quot;&amp;+%&#039;&lt;é🙂x_*[` + "`]",
		"HTML attribute":        `p@ss w/rd&quot;&amp;+%'<é🙂x_*[` + "`]",
		"HTML, mustache":        `p@ss w&#x2F;rd&quot;&amp;+%&#39;&lt;é🙂x_*[&#x60;]`,
		"HTML, html/template":   `p@ss w/rd&#34;&amp;&#43;%&#39;&lt;é🙂x_*[` + "`]",
		"HTML, .NET":            `p@ss w/rd&quot;&amp;+%&#39;&lt;&#233;🙂x_*[` + "`]",
		"HTML, every &#NN;":     entities(secret, "&#%d;"),
		"HTML, every &#xnn;":    entities(secret, "&#x%x;"),
		"HTML, every &#xNN;":    entities(secret, "&#x%X;"),
		"Markdown":              mdSecret,
		"no-break space":        strings.Replace(secret, " ", "\u00a0", 1),
		"&nbsp;":                strings.Replace(secret, " ", "&nbsp;", 1),
		"line break":            strings.Replace(secret, " ", "\n", 1),
		"two spaces":            strings.Replace(secret, " ", "  ", 1),
	}
	s := New([]string{secret})
	for name, enc := range cases {
		t.Run(name, func(t *testing.T) {
			in := "before " + enc + " after"
			if got := s.String(in); got != "before "+Hidden+" after" {
				t.Errorf("%s\n in: %q\nout: %q", name, in, got)
			}
		})
	}
	// A URL carried in another's query: what comes before the secret stays.
	in := url.QueryEscape("https://example.com/?p=" + url.QueryEscape(secret))
	if got, want := s.String(in), url.QueryEscape("https://example.com/?p=")+Hidden; got != want {
		t.Errorf("a URL in a query: %q, want %q", got, want)
	}
}

func TestEachSpecialCharacterAlone(t *testing.T) {
	// One special character per secret, so a form that mangles it is
	// caught on its own, through the stdlib's own encoders.
	for _, c := range []string{" ", "/", `"`, "&", "+", "%", "é", "🙂", "<", "'", `\`, "\n", "_", "*", "[", "]", "`", "#", "|", "\u00a0"} {
		sec := "ab" + c + "cd"
		s := New([]string{sec})
		for _, enc := range []string{sec, goJSON(sec, false), goJSON(sec, true), goJSON(goJSON(sec, false), false),
			url.QueryEscape(sec), url.PathEscape(sec), html.EscapeString(sec), strings.ToLower(url.QueryEscape(sec)),
			url.QueryEscape(url.QueryEscape(sec)), entities(sec, "&#%d;"), entities(sec, "&#x%x;")} {
			if got := s.String("x " + enc + " y"); got != "x "+Hidden+" y" {
				t.Errorf("secret %q as %q: %q", sec, enc, got)
			}
		}
	}
}

func TestOverlapsRepeatsShortSecretsAndNearMisses(t *testing.T) {
	cases := []struct {
		name    string
		secrets []string
		in, out string
	}{
		{"one inside another", []string{"hunter22", "xxhunter22yy"}, "a xxhunter22yy b hunter22 c", "a <hidden> b <hidden> c"},
		{"overlapping", []string{"abcd", "cdefgh"}, "-abcdefgh-", "-<hidden>-"},
		{"touching", []string{"abcd", "efgh"}, "abcdefgh", "<hidden>"},
		{"twice", []string{"s3cret!"}, "s3cret! and s3cret!", "<hidden> and <hidden>"},
		{"three characters ignored", []string{"abc"}, "abc abc", "abc abc"},
		{"three characters ignored, others kept", []string{"abc", "abcdef"}, "abc abcdef", "abc <hidden>"},
		{"four runes, not bytes", []string{"éé"}, "éé", "éé"},
		{"near misses", []string{"hunter22"}, "hunter2 Hunter22 hunter 22 hunter%2022 huntér22", "hunter2 Hunter22 hunter 22 hunter%2022 huntér22"},
		{"empty text", []string{"hunter22"}, "", ""},
		{"whole text", []string{"hunter22"}, "hunter22", "<hidden>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := New(c.secrets).String(c.in); got != c.out {
				t.Errorf("%v on %q: %q, want %q", c.secrets, c.in, got, c.out)
			}
		})
	}
}

func TestNothingToHideCostsNothing(t *testing.T) {
	in := strings.Repeat("an ordinary page with no secret in it ", 100)
	var none *Scrubber
	if got := none.String(in); unsafe.StringData(got) != unsafe.StringData(in) {
		t.Error("a nil Scrubber copied the text")
	}
	if New(nil) != nil || New([]string{"abc", ""}) != nil {
		t.Error("New with no usable secret isn't nil")
	}
	if n := testing.AllocsPerRun(100, func() { _ = none.String(in) }); n != 0 {
		t.Errorf("nil Scrubber: %v allocations", n)
	}
	s := New([]string{"hunter22", secret})
	if got := s.String(in); unsafe.StringData(got) != unsafe.StringData(in) {
		t.Error("text with no secret was copied")
	}
	if n := testing.AllocsPerRun(100, func() { _ = s.String(in) }); n != 0 {
		t.Errorf("no match: %v allocations", n)
	}
}

// pageText is n bytes of text that looks like a page's: words, markup,
// URLs and escapes, so the secrets' first bytes come up often.
func pageText(n int) string {
	words := []string{"the", "<div class=\"x\">", "https://example.com/a?b=c&d=e", "&amp;", "p@ss", "w/rd", "%20", `\"`, "hunter2", "é", "🙂", "\n"}
	r := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(words[r.IntN(len(words))])
		b.WriteByte(' ')
	}
	return b.String()
}

func tenSecrets() []string {
	var out []string
	for i := range 10 {
		out = append(out, fmt.Sprintf("%s-%d-%s", secret, i, strings.Repeat("k", i)))
	}
	return out
}

func TestTwoMegabytesWithTenSecretsUnder50ms(t *testing.T) {
	text := pageText(2 << 20)
	text = text[:1<<20] + tenSecrets()[3] + text[1<<20:] // one to find
	s := New(tenSecrets())
	best := time.Hour
	for range 5 {
		start := time.Now()
		out := s.String(text)
		best = min(best, time.Since(start))
		if strings.Count(out, Hidden) != 1 {
			t.Fatalf("%d hidden, want 1", strings.Count(out, Hidden))
		}
	}
	t.Logf("2 MB, 10 secrets: %v", best)
	if limit := 50 * time.Millisecond * raceSlowdown; best > limit {
		t.Errorf("scrubbing 2 MB took %v, want under %v", best, limit)
	}
}

func BenchmarkScrub2MB(b *testing.B) {
	text := pageText(2 << 20)
	s := New(tenSecrets())
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		_ = s.String(text)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if r.For("s1") != nil {
		t.Fatal("an empty registry hides something")
	}
	r.Add("s1", "abc") // too short: ignored
	if r.Count("s1") != 0 || r.For("s1") != nil {
		t.Fatal("a 3-character secret was kept")
	}
	r.Add("s1", "first-secret")
	r.Add("s1", "first-secret")
	r.Add("s2", "second-secret")
	if r.Count("s1") != 1 || r.Count("s2") != 1 {
		t.Fatalf("counts %d, %d", r.Count("s1"), r.Count("s2"))
	}
	text := "first-secret second-secret"
	if got := r.For("s1").String(text); got != "<hidden> second-secret" {
		t.Errorf("s1 alone: %q", got)
	}
	if got := r.For("s2", "s1", "s3").String(text); got != "<hidden> <hidden>" {
		t.Errorf("both: %q", got)
	}
	// Built once per change of the set.
	if r.For("s1", "s2") != r.For("s2", "s1") {
		t.Error("the Scrubber wasn't cached")
	}
	before := r.For("s1")
	r.Add("s1", "more-secret")
	if r.For("s1") == before || r.For("s1").String("more-secret") != Hidden {
		t.Error("adding a secret didn't rebuild the Scrubber")
	}
	r.Forget("s1")
	if r.Count("s1") != 0 || r.For("s1") != nil {
		t.Error("s1's secrets outlived Forget")
	}
	if got := r.For("s1", "s2").String(text); got != "first-secret <hidden>" {
		t.Errorf("after forgetting s1: %q", got)
	}
}

func TestJSON(t *testing.T) {
	s := New([]string{secret, "plain-secret"})
	doc := map[string]any{
		"text":  "page says " + secret + " and " + url.QueryEscape(secret),
		"list":  []any{"plain-secret", 1234, true, nil},
		"plain": "nothing here",
		secret:  "a key",
	}
	body, _ := json.Marshal(doc) // HTML-safe, as the API writes it
	out := s.JSON(body)
	if !json.Valid(out) {
		t.Fatalf("not JSON any more: %s", out)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	want := map[string]any{
		"text":  "page says <hidden> and <hidden>",
		"list":  []any{"<hidden>", 1234.0, true, nil},
		"plain": "nothing here",
		Hidden:  "a key",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// A quote in the secret can't make a match across strings.
	s2 := New([]string{`a","b`})
	body = []byte(`["a","b","x a\",\"b y"]`)
	if out := s2.JSON(body); string(out) != `["a","b","x \u003chidden\u003e y"]` {
		t.Errorf("across strings: %q", out)
	}
	// Untouched documents are the same bytes; invalid JSON is text.
	clean := []byte(`{"a": "b", "n": 12345}`)
	if out := s.JSON(clean); &out[0] != &clean[0] {
		t.Error("a clean document was copied")
	}
	if out := s.JSON([]byte("not json plain-secret")); string(out) != "not json <hidden>" {
		t.Errorf("text: %s", out)
	}
	var none *Scrubber
	if out := none.JSON(body); &out[0] != &body[0] {
		t.Error("nil Scrubber copied")
	}
}

func TestUnion(t *testing.T) {
	a, b := New([]string{"alpha-secret"}), New([]string{"bravo-secret"})
	if Union(nil, b) != b || Union(a, nil) != a || Union(a, a) != a || Union(nil, nil) != nil {
		t.Error("trivial unions")
	}
	if got := Union(a, b).String("alpha-secret bravo-secret"); got != "<hidden> <hidden>" {
		t.Errorf("union: %q", got)
	}
}

func TestNormalizedMatchesMapBackToTheirBytes(t *testing.T) {
	cases := []struct {
		name, secret, in, out string
	}{
		{"around entities", "s3cr&t!x", "a &amp; b s3cr&#38;t&#x21;x c &lt; d", "a &amp; b <hidden> c &lt; d"},
		{"multi-byte reference", "café-pass", "x caf&#233;-pass y", "x <hidden> y"},
		{"escaped backslash before", "*star-pass", `x \\\*star-pass y`, `x \\<hidden> y`},
		{"lone backslash before", "*star-pass", `x \*star-pass y`, `x <hidden> y`},
		{"ends in a backslash, then punctuation", `back-pass\`, `x back-pass\. y`, `x <hidden> y`},
		{"ends in a backslash, then a letter", `back-pass\`, `x back-pass\n y`, `x <hidden>n y`},
		{"whitespace run", "two words", "x two \n\t words y", "x <hidden> y"},
		{"no-break space", "two words", "x two words y", "x <hidden> y"},
		{"at the very end", "s3cr&t!x", "end s3cr&amp;t!x", "end <hidden>"},
		{"near miss, entity", "hunter22", "hunter&#50;3 hunter&#50;2", "hunter&#50;3 <hidden>"},
		{"not a reference", "a&b;c-pass", "x a&b;c-pass &bogus; &#xZZ; &#0; &#55296; y", "x <hidden> &bogus; &#xZZ; &#0; &#55296; y"},
		{"a backslash before a letter stays", `ab\ncd-pass`, `x ab\ncd-pass y abncd-pass`, `x <hidden> y abncd-pass`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := New([]string{c.secret}).String(c.in); got != c.out {
				t.Errorf("%q in %q: %q, want %q", c.secret, c.in, got, c.out)
			}
		})
	}
}

func TestLongPatternsUseAWiderRing(t *testing.T) {
	long := strings.Repeat("Ab1/é", 200) // forms well past 1024 bytes
	s := New([]string{long})
	for _, enc := range []string{long, url.QueryEscape(url.QueryEscape(long)), entities(long, "&#x%X;")} {
		if got := s.String("x " + enc + " y"); got != "x "+Hidden+" y" {
			t.Errorf("a %d-byte form: %.60q…", len(enc), got)
		}
	}
}
