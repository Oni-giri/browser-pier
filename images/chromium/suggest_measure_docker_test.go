//go:build docker && measure

package chromium

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestMeasureSuggest records what typing into the address bar sends to
// the search engine's suggest service (PROJ-191). It's a measurement, not
// a check, so it only runs with the measure tag:
//
//	go test -tags 'docker measure' -run TestMeasureSuggest -v ./images/chromium/
//
// With a search engine the test reads (suggest.test), it types a plain
// word and addresses with a secret in the query or the path, over http,
// https and without a scheme, at instant pace (open's) and human pace,
// and logs which suggest requests each sent and how much of the secret
// they carried; then whether focusing the address bar on a page with a
// secret in its address sends anything. With the image's own engine
// (DuckDuckGo), which the isolated proxy refuses, it logs which hosts the
// browser asked for while a word and an address were typed.
func TestMeasureSuggest(t *testing.T) {
	image := testImage(t)
	var summary []string
	report := func(pace string, tp typed, secret string) {
		var qs []string
		for _, s := range tp.suggests {
			qs = append(qs, s.uri)
		}
		var early []string
		for _, s := range tp.early {
			early = append(early, s.uri)
		}
		line := fmt.Sprintf("%-7s %-45q suggest requests %2d", pace, tp.text, len(tp.suggests))
		if secret != "" {
			line += fmt.Sprintf(", secret carried %q of %q", leaked(tp.suggests, secret), secret)
		}
		line += fmt.Sprintf("; site before Return %d; refused %v", len(tp.early), otherHosts(tp.refused))
		summary = append(summary, line)
		t.Logf("%s\n  suggest: %s\n  site before Return: %s\n  proxy reached %v", line, strings.Join(qs, " | "), strings.Join(early, " | "), tp.targets)
	}

	r := newSuggestRig(t, image, true)
	forms := []string{
		"http://site.test/reset?token=%s",
		"http://site.test/reset/%s",
		"https://site.test/reset?token=%s",
		"https://site.test/reset/%s",
		"site.test/login/%s",
	}
	for _, pace := range []string{"instant", "human"} {
		report(pace, r.open(t, "pierword", pace), "")
		for i, form := range forms {
			secret := fmt.Sprintf("Zq%dx%dWv", i, time.Now().UnixNano()%1e6)
			report(pace, r.open(t, fmt.Sprintf(form, secret), pace), secret)
		}
	}

	// Focus alone, on a page whose address carries a secret (zero suggest).
	secret := fmt.Sprintf("Zq9x%dWv", time.Now().UnixNano()%1e6)
	r.open(t, "http://site.test/reset?token="+secret, "instant")
	from := time.Now()
	r.b.input(t, "/input/key", `{"keys": "ctrl+l", "pace": "instant"}`)
	time.Sleep(3 * time.Second)
	r.b.input(t, "/input/key", `{"keys": "Escape", "pace": "instant"}`)
	n := 0
	for _, s := range r.since("suggest.test", from) {
		if strings.HasPrefix(s.uri, "/suggest") {
			n++
			t.Logf("focus: %s", s.uri)
		}
	}
	summary = append(summary, fmt.Sprintf("focus on a page with a secret: %d suggest requests, secret carried %q", n, leaked(r.since("suggest.test", from), secret)))
	docker(t, "rm", "-f", r.b.name)

	// The image's own engine: which hosts typing reaches for.
	d := newSuggestRig(t, image, false)
	// Addresses first: a word's search, refused, is retried for a while
	// after its Return, which would count against what is typed next.
	for _, form := range []string{"http://site.test/reset/Zq8x%dWv", "pierword"} {
		for _, pace := range []string{"instant", "human"} {
			text := form
			if strings.Contains(form, "%d") {
				text = fmt.Sprintf(form, time.Now().UnixNano()%1e6)
			}
			tp := d.open(t, text, pace)
			n := 0
			for _, tg := range tp.refused {
				if host, _, _ := net.SplitHostPort(tg); host == "duckduckgo.com" { // its suggest host; not start.duckduckgo.com
					n++
				}
			}
			line := fmt.Sprintf("image's engine: %-7s %-40q duckduckgo.com asked for %d times; refused %v", pace, text, n, otherHosts(tp.refused))
			summary = append(summary, line)
			t.Logf("%s\n  refused, in order: %v", line, tp.refused)
		}
	}
	for _, s := range summary {
		t.Log(s)
	}
}
