package scrub_test

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"

	"github.com/Oni-giri/browser-pier/internal/pagetext"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// tree builds a pagetext tree from HTML, as page_content gets it from the
// browser (pre keeps its line breaks).
func tree(t *testing.T, doc string) *pagetext.Node {
	t.Helper()
	root, err := xhtml.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var conv func(n *xhtml.Node, pre bool) *pagetext.Node
	conv = func(n *xhtml.Node, pre bool) *pagetext.Node {
		var out *pagetext.Node
		switch n.Type {
		case xhtml.TextNode:
			return &pagetext.Node{Text: n.Data, Pre: pre}
		case xhtml.DocumentNode:
			out = &pagetext.Node{Tag: "#document"}
		case xhtml.ElementNode:
			out = &pagetext.Node{Tag: n.Data, Attrs: map[string]string{}}
			for _, a := range n.Attr {
				out.Attrs[a.Key] = a.Val
			}
			pre = pre || n.Data == "pre"
		default:
			return nil
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			if c := conv(k, pre); c != nil {
				out.Kids = append(out.Kids, c)
			}
		}
		return out
	}
	return conv(root, false)
}

// TestPageContentHidesSecrets puts real secrets in a page wherever text
// can be, renders it with page_content's own renderers (Markdown, which
// escapes, and the reader view and plain text, which collapse
// whitespace), and scrubs the result: no piece of a secret may be left.
// Each secret is built from distinctive words (Kq9, Vx4…) around the
// characters Markdown escapes or that start a Markdown block.
func TestPageContentHidesSecrets(t *testing.T) {
	words := []string{"Kq9", "Vx4", "Wm2", "Jt5"}
	secrets := []string{
		"Kq9_Vx4*Wm2[Jt5]",
		"Kq9`Vx4`Wm2",
		`Kq9\Vx4\_Wm2`,
		`Kq9 Vx4\`,
		"Kq9  Vx4", // two spaces: collapsed on the page
		"Kq9|Vx4|Wm2",
		"1. Kq9 Vx4",
		"12) Kq9Vx4",
		"# Kq9 Vx4",
		"> Kq9 Vx4",
		"- Kq9 Vx4",
		"+ Kq9 Vx4",
		"--Kq9Vx4",
		"~~~Kq9Vx4",
		"|Kq9Vx4",
		`p@ss w/rd"&+%'<é🙂Kq9_*[` + "`]",
	}
	for _, sec := range secrets {
		t.Run(sec, func(t *testing.T) {
			e := html.EscapeString(sec)
			q := url.QueryEscape(sec)
			doc := `<html><head><title>T</title></head><body><article>
<h1>Welcome ` + e + `</h1>
<p>` + e + `</p>
<p>Your password is ` + e + ` and that is all.</p>
<p><em>` + e + `</em> and <strong>` + e + `</strong></p>
<p>Last line<br>` + e + `<br>next</p>
<ul><li>` + e + `</li><li>item ` + e + `</li></ul>
<blockquote><p>` + e + `</p></blockquote>
<table><tr><th>Name</th><th>Value</th></tr><tr><td>pw</td><td>` + e + `</td></tr></table>
<p><a href="/s?q=` + q + `">` + e + `</a> <img alt="` + e + `" src="/i.png"></p>
<p><code>` + e + `</code></p>
<pre>` + e + `
` + e + `</pre>
<p>` + strings.ReplaceAll(e, " ", "&nbsp;") + `</p>
</article></body></html>`
			root := tree(t, doc)
			base, _ := url.Parse("https://example.com/page")
			ctx := context.Background()
			md, _, err := pagetext.Markdown(ctx, root, base, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			plain, _, _ := pagetext.Plain(ctx, root, 1<<20)
			readable, _, _ := pagetext.Readable(ctx, root, 1<<20)
			s := scrub.New([]string{sec})
			for name, out := range map[string]string{"Markdown": md, "plain": plain, "reader view": readable} {
				got := s.String(out)
				for _, w := range words {
					if strings.Contains(got, w) {
						t.Errorf("%s: %q left after scrubbing\nrendered:\n%s\nscrubbed:\n%s", name, w, out, got)
						break
					}
				}
				if !strings.Contains(got, scrub.Hidden) {
					t.Errorf("%s: nothing hidden:\n%s", name, got)
				}
			}
		})
	}
}
