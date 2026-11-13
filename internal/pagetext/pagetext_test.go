package pagetext

import (
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// parse builds a Node tree from HTML as a browser with default styles
// would lay it out: elements with the hidden attribute or display:none
// in their style are left out, and pre keeps its line breaks.
func parse(t *testing.T, doc string) *Node {
	t.Helper()
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var conv func(n *html.Node, pre bool) *Node
	conv = func(n *html.Node, pre bool) *Node {
		switch n.Type {
		case html.TextNode:
			return &Node{Text: n.Data, Pre: pre}
		case html.DocumentNode:
			out := &Node{Tag: "#document"}
			for k := n.FirstChild; k != nil; k = k.NextSibling {
				if c := conv(k, pre); c != nil {
					out.Kids = append(out.Kids, c)
				}
			}
			return out
		case html.ElementNode:
		default:
			return nil
		}
		out := &Node{Tag: n.Data, Attrs: map[string]string{}}
		for _, a := range n.Attr {
			out.Attrs[a.Key] = a.Val
		}
		if _, hidden := out.Attrs["hidden"]; hidden || strings.Contains(strings.ReplaceAll(out.Attrs["style"], " ", ""), "display:none") {
			return nil
		}
		pre = pre || n.Data == "pre"
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			if c := conv(k, pre); c != nil {
				out.Kids = append(out.Kids, c)
			}
		}
		return out
	}
	return conv(root, false)
}

func md(t *testing.T, doc string) string {
	t.Helper()
	base, _ := url.Parse("https://example.com/dir/page.html")
	out, full := Markdown(parse(t, doc), base, 1<<20)
	if full {
		t.Fatal("cut short")
	}
	return out
}

func TestMarkdownBlocks(t *testing.T) {
	got := md(t, `<!doctype html><title>T</title><style>p{color:red}</style><script>var x = "SCRIPT";</script>
<h1>Main  title</h1>
<p>First paragraph with <strong>bold</strong>, <em>emphasis</em> and <code>a_code</code>.<br>Second line.</p>
<div>A div <span>with</span> <b> spaced </b>words.</div>
<h3>Lists</h3>
<ul><li>One</li><li>Two<ul><li>Nested</li></ul></li><li></li></ul>
<ol start="3"><li>Third</li><li><p>Fourth</p></li></ol>
<blockquote><p>Quoted</p><p>Twice</p></blockquote>
<pre class="language-go">func main() {
	fmt.Println("hi")
}</pre>
<hr>
<p>The end.</p>`)
	want := "# Main title\n\n" +
		"First paragraph with **bold**, _emphasis_ and `a_code`.\nSecond line.\n\n" +
		"A div with **spaced** words.\n\n" +
		"### Lists\n\n" +
		"- One\n- Two\n  - Nested\n\n" +
		"3. Third\n4. Fourth\n\n" +
		"> Quoted\n>\n> Twice\n\n" +
		"```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\n" +
		"---\n\n" +
		"The end."
	if got != want {
		t.Errorf("got:\n%s\n---\nwant:\n%s", got, want)
	}
}

func TestMarkdownLinksAndImages(t *testing.T) {
	got := md(t, `<p><a href="other.html">Relative</a> <a href="/abs?q=a b">Absolute</a> <a href="#top">Fragment</a>
<a href="javascript:alert(1)">Script</a> <a href="mailto:a@example.com">Mail</a> <a href="https://x.example/(paren)">Paren</a>
<a href="/empty"></a> <a href="/long?`+strings.Repeat("x", 600)+`">Long</a></p>
<p><img src="pic.png" alt="A cat"> <img src="deco.png"> <img src="data:image/png;base64,AAAA" alt="Inline"></p>
<a href="/card"><div>Card title</div><p>Card text</p></a>`)
	for _, want := range []string{
		"[Relative](https://example.com/dir/other.html)",
		"[Absolute](https://example.com/abs?q=a%20b)",
		" Fragment ", " Script ",
		"[Mail](mailto:a@example.com)",
		"[Paren](https://x.example/%28paren%29)",
		" Long",
		"![A cat](https://example.com/dir/pic.png)",
		"Inline",
		"Card title\n\nCard text",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"deco", "javascript", "#top", "/empty", "data:", "xxxx"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q in:\n%s", bad, got)
		}
	}
}

func TestMarkdownEscapes(t *testing.T) {
	got := md(t, `<p>*not bold* and [not](a link) and a_b</p><p># not a heading</p><p>1. not a list</p><p>- nor this</p><p>&gt; nor a quote</p><p>===</p>`)
	want := `\*not bold\* and \[not\](a link) and a\_b` + "\n\n" + `\# not a heading` + "\n\n" + `1\. not a list` + "\n\n" + `\- nor this` + "\n\n" + `\> nor a quote` + "\n\n" + `\===`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Code is left as it is, with a fence longer than what it holds.
	got = md(t, "<p><code>a `tick` *b*</code></p><pre>```\nfenced\n```</pre>")
	if want := "``a `tick` *b*``\n\n````\n```\nfenced\n```\n````"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownTables(t *testing.T) {
	got := md(t, `<table><caption>Prices</caption><thead><tr><th>Item</th><th>Price</th></tr></thead>
<tbody><tr><td>Tea | hot</td><td>2 €</td></tr><tr><td>Cake<br>slice</td></tr></tbody></table>
<table><tr><td><ul><li>Layout</li></ul></td><td>cell</td></tr></table>
<table><tr><td>One column</td></tr></table>`)
	want := "Prices\n\n| Item | Price |\n| --- | --- |\n| Tea \\| hot | 2 € |\n| Cake slice |  |\n\n- Layout\n\ncell\n\nOne column"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownLeavesOutFieldsAndHidden(t *testing.T) {
	got := md(t, `<form><label>User <input name="u" value="FIELD-VALUE"></label>
<label>Password <input type="password" value="PASSWORD-VALUE"></label>
<textarea>TEXTAREA-VALUE</textarea><select><option>OPTION-TEXT</option></select>
<button>Sign in</button></form>
<p hidden>HIDDEN-TEXT</p><p style="display: none">NONE-TEXT</p>
<svg><text>SVG-TEXT</text></svg><noscript>NOSCRIPT</noscript><template><p>TEMPLATE</p></template>
<iframe src="https://frame.example"></iframe>`)
	for _, bad := range []string{"VALUE", "OPTION", "HIDDEN", "NONE", "SVG", "NOSCRIPT", "TEMPLATE"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s in:\n%s", bad, got)
		}
	}
	for _, want := range []string{"User", "Password", "Sign in", "[iframe: https://frame.example]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdownLimit(t *testing.T) {
	doc := strings.Repeat("<p>"+strings.Repeat("word ", 20)+"</p>", 1000)
	out, full := Markdown(parse(t, doc), nil, 5000)
	if !full || len(out) > 5000 || len(out) < 4000 {
		t.Errorf("limit: full=%v, %d bytes", full, len(out))
	}
	if _, full := Markdown(parse(t, "<p>short</p>"), nil, 5000); full {
		t.Error("a short page counts as cut")
	}
}

func TestPlain(t *testing.T) {
	out, _ := Plain(parse(t, `<h2>Head</h2><p>Some <a href="/x">link</a> and <b>bold</b> <img src="a.png" alt="pic"></p><ul><li>Item</li></ul><pre>  code</pre>
<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>`), 0)
	want := "Head\n\nSome link and bold\n\n- Item\n\n  code\n\nA | B\n1 | 2"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

const prose = "The committee met on Tuesday to discuss the new library, which opens next spring. Members agreed on the budget, the opening hours and the staffing plan, after a long debate about parking. "

func TestReadableFindsTheArticle(t *testing.T) {
	doc := `<header><a href="/">Home</a> Site name</header>
<nav><a href="/a">Section A</a><a href="/b">Section B</a></nav>
<div class="cookie-banner">We use cookies. <button>Accept</button></div>
<main><article><header><h1>Library opens</h1></header>
<p>` + prose + `</p><p>` + prose + `</p>
<div class="share-links"><a href="/s">Share</a></div>
<ul><li><a href="/1">Related one</a></li><li><a href="/2">Related two</a></li></ul>
<footer>Filed under news.</footer></article>
<aside>Sidebar ad text</aside></main>
<footer>Copyright footer</footer>`
	out, _ := Readable(parse(t, doc), 0)
	for _, want := range []string{"Library opens", "The committee met", "Filed under news."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Home", "Section A", "cookies", "Accept", "Share", "Related", "Sidebar", "Copyright"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q in:\n%s", bad, out)
		}
	}
}

func TestReadableScoresDivSoup(t *testing.T) {
	links := strings.Repeat(`<div><a href="/x">A link somewhere else, with words</a></div>`, 20)
	doc := `<div id="top">` + links + `</div>
<div id="wrap"><div class="sidebar"><p>` + prose + `</p></div>
<div class="post-body"><div>` + prose + `</div><div>` + prose + `</div><div>` + prose + `</div></div>
<div class="comments"><p>` + prose + `</p></div></div>`
	out, _ := Readable(parse(t, doc), 0)
	if n := strings.Count(out, "The committee met"); n != 3 {
		t.Errorf("want the post's 3 paragraphs, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "A link somewhere") {
		t.Errorf("the link list is in:\n%s", out)
	}
}

func TestReadableShortPageComesBackWhole(t *testing.T) {
	out, _ := Readable(parse(t, `<nav><a href="/">Home</a></nav><h1>Signed in</h1><p>Welcome back.</p>`), 0)
	if out != "Signed in\n\nWelcome back." {
		t.Errorf("got %q", out)
	}
}
