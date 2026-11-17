package pagetools

import (
	"regexp"
	"strings"
	"testing"
)

// env is a page envelope (PROJ-167), as an answer has it.
type env struct {
	marker, source, url, title, format, content, after string
	hasTitle                                           bool
}

var fenceOpen = regexp.MustCompile(`<untrusted-content-([0-9a-f]{16}) source="([^"]*)">\n[^\n]*\n<page>\n`)

// envelopes are out's page envelopes, in order, failing on one that isn't
// whole.
func envelopes(t *testing.T, out string) []env {
	t.Helper()
	var all []env
	for _, m := range fenceOpen.FindAllStringSubmatchIndex(out, -1) {
		e := env{marker: out[m[2]:m[3]], source: out[m[4]:m[5]]}
		rest := out[m[1]:]
		closing := "\n</untrusted-content-" + e.marker + ">"
		end := strings.Index(rest, closing)
		if end < 0 {
			t.Fatalf("an envelope isn't closed:\n%s", out)
		}
		rest = rest[:end]
		u, ok := strings.CutPrefix(rest, "<url>")
		if !ok {
			t.Fatalf("no <url> first:\n%s", out)
		}
		i := strings.Index(u, "</url>\n")
		if i < 0 {
			t.Fatalf("<url> isn't closed:\n%s", out)
		}
		e.url, rest = u[:i], u[i+len("</url>\n"):]
		if tl, ok := strings.CutPrefix(rest, "<title>"); ok {
			i := strings.Index(tl, "</title>\n")
			if i < 0 {
				t.Fatalf("<title> isn't closed:\n%s", out)
			}
			e.title, e.hasTitle, rest = tl[:i], true, tl[i+len("</title>\n"):]
		}
		open := "<page_content-" + e.marker + ` format="`
		c, ok := strings.CutPrefix(rest, open)
		if !ok {
			t.Fatalf("no content tag with the fence's marker:\n%s", out)
		}
		i = strings.Index(c, "\">\n")
		e.format, c = c[:i], c[i+3:]
		close := "</page_content-" + e.marker + ">\n</page>"
		j := strings.Index(c, close)
		if j < 0 || strings.Count(c, "</page_content-"+e.marker+">") != 1 {
			t.Fatalf("the content tag isn't closed once:\n%s", out)
		}
		e.content = strings.TrimSuffix(c[:j], "\n")
		e.after = strings.TrimPrefix(c[j+len(close):], "\n")
		all = append(all, e)
	}
	return all
}

// envelope is out's one page envelope.
func envelope(t *testing.T, out string) env {
	t.Helper()
	all := envelopes(t, out)
	if len(all) != 1 {
		t.Fatalf("%d page envelopes, want 1:\n%s", len(all), out)
	}
	return all[0]
}
