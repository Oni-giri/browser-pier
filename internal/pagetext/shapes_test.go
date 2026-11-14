package pagetext

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestReadableShapes is the third review's list of page shapes: what
// each must and mustn't keep ("!" marks what must be absent). Forum
// threads (15, 16) and a wrapper whose classes are all boilerplate
// beside a longer block (17) are known misses, left to PROJ-92.
func TestReadableShapes(t *testing.T) {
	long := strings.Repeat("The committee met on Tuesday, and agreed on the budget, the hours and the plan. ", 6)
	short := "A short post, of one line."
	c := func(cls string, kids ...*Node) *Node { return at("div", map[string]string{"class": cls}, kids...) }
	id := func(i string, kids ...*Node) *Node { return at("div", map[string]string{"id": i}, kids...) }
	var comments, wpComments, navl, cards []*Node
	for i := 0; i < 60; i++ {
		comments = append(comments, c("comment", el("p", tx(fmt.Sprintf("COMMENT-%d I disagree, frankly, with all of this, and so on and on.", i)))))
		wpComments = append(wpComments, at("article", map[string]string{"class": "comment-body"}, c("comment-content", el("p", tx(fmt.Sprintf("COMMENT-%d I disagree, frankly, with all of this, and so on and on.", i))))))
		navl = append(navl, el("li", at("a", map[string]string{"href": "/s"}, tx(fmt.Sprintf("NAV-LINK section number %d", i)))))
	}
	for i := 0; i < 12; i++ {
		cards = append(cards, c("card", at("a", map[string]string{"href": "/a"}, el("h3", tx(fmt.Sprintf("CARD-%d headline of a story", i))), el("p", tx("A teaser sentence, short, for the card.")))))
	}
	post := func() *Node {
		return c("post", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx("POST-END "+long)))
	}
	hdr := el("header", el("nav", el("ul", navl[:8]...)))
	ftr := el("footer", el("p", tx("FOOTER-TEXT © Example, all rights reserved, and a long list of legal words here.")))
	sidebar := c("sidebar", el("ul", navl[10:30]...))
	art := func() *Node {
		return el("article", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx(long)), el("p", tx("POST-END "+long)))
	}
	divart := func() *Node {
		return c("inner", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx(long)), el("p", tx("POST-END "+long)))
	}
	wp := wpComments
	cases := []struct {
		name string
		n    *Node
		want string // comma list; ! prefix = must be absent
	}{
		{"1 post + long div comments", el("body", post(), id("comments", comments...)), "POST-TITLE,POST-END,!COMMENT-3 "},
		{"2 article + wp comments", el("body", el("article", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx("POST-END "+long))), at("section", map[string]string{"id": "comments", "class": "comments-area"}, wpComments...)), "POST-TITLE,POST-END,!COMMENT-3 "},
		{"3 one-line article + comments", el("body", el("article", el("h1", tx("POST-TITLE")), el("p", tx(short))), id("comments", comments...)), "POST-TITLE,short post,!COMMENT-3 "},
		{"4 post + site-footer div", el("body", c("mega-menu", el("ul", navl...)), post(), c("site-footer", el("p", tx("FOOTER-TEXT "+long+long+long+long+long)))), "POST-TITLE,POST-END,!FOOTER-TEXT,!NAV-LINK"},
		{"5 short main + cookie banner", el("body", el("main", el("h1", tx("POST-TITLE")), el("p", tx("Short product page, one line."))), c("cookie-banner", el("p", tx("COOKIE-TEXT "+long)))), "POST-TITLE,!COOKIE-TEXT"},
		{"6 underscore/camel classes", el("body", post(), c("cookie_notice", el("p", tx("COOKIE-TEXT "+long))), id("disqus_thread", comments[:5]...), c("siteFooter", el("p", tx("FOOTER-TEXT "+long)))), "POST-TITLE,!COOKIE-TEXT,!COMMENT-3 ,!FOOTER-TEXT"},
		{"7 wrapper has-sidebar>article", el("body", c("layout has-sidebar", el("article", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx("POST-END "+long))), sidebar)), "POST-TITLE,POST-END,!NAV-LINK"},
		{"8 wrapper community-page, no article", el("body", id("community-page", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx("POST-END "+long)))), "POST-TITLE,POST-END"},
		{"9 wrapper js-social > main", el("body", c("js-social-enabled", el("main", el("h1", tx("POST-TITLE")), el("p", tx("POST-END "+long))))), "POST-TITLE,POST-END"},
		{"10 section.remarkable", el("body", at("section", map[string]string{"class": "remarkable"}, el("h1", tx("POST-TITLE")), el("p", tx("POST-END "+long)))), "POST-TITLE,POST-END"},
		{"11 plain blog (div.post, hdr/ftr/sidebar)", el("body", hdr, c("container", post(), sidebar), ftr), "POST-TITLE,POST-END,!NAV-LINK,!FOOTER-TEXT"},
		{"12 news, no article, divs only", el("body", c("header", el("ul", navl[:8]...)), c("story-body", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx(long)), el("p", tx("POST-END "+long))), c("related-stories", el("ul", navl[30:40]...)), c("footer", el("p", tx("FOOTER-TEXT legal words")))), "POST-TITLE,POST-END,!NAV-LINK,!FOOTER-TEXT"},
		{"13 docs page (main + nav sidebar)", el("body", hdr, el("nav", el("ul", navl...)), el("main", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("pre", tx("CODE-SAMPLE x := 1")), el("h2", tx("Section")), el("p", tx("POST-END "+long))), ftr), "POST-TITLE,CODE-SAMPLE,POST-END,!NAV-LINK,!FOOTER-TEXT"},
		{"14 docs, wrapper class menu-open, no main", el("body", c("page menu-open", c("toc", el("ul", navl...)), c("doc", el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx("POST-END "+long))))), "POST-TITLE,POST-END"},
		{"18 short wrapper(unlikely) + long other", el("body", c("newsletter", el("h1", tx("POST-TITLE")), el("p", tx(short))), c("stuff", el("p", tx("OTHER "+long)))), "OTHER"},
		{"19 short page all in .share wrapper", el("body", c("share", el("h1", tx("POST-TITLE")), el("p", tx(short)))), "POST-TITLE,short post"},
		{"20 one-line article + comments in .comment-content divs", el("body", el("article", el("h1", tx("POST-TITLE")), el("p", tx(short))), c("post-comments", wpComments...)), "POST-TITLE,short post,!COMMENT-3 "},
		{"21 front page of cards", el("body", hdr, c("grid", cards...), ftr), "CARD-0,CARD-11,!NAV-LINK"},
		{"22 article in div#content.widget-area", el("body", at("div", map[string]string{"id": "primary", "class": "widget-area"}, el("h1", tx("POST-TITLE")), el("p", tx(long)), el("p", tx("POST-END "+long))), sidebar), "POST-TITLE,POST-END,!NAV-LINK"},
		{"23 long article + div.post-comments", el("body", art(), c("post-comments", wp...)), "POST-TITLE,POST-END,!COMMENT-3 "},
		{"24 long article + div.entry-comments", el("body", art(), c("entry-comments", wp...)), "POST-TITLE,POST-END,!COMMENT-3 "},
		{"25 has-sidebar wrapper, no article, + 300 B legal div", el("body", c("layout has-sidebar", divart()), c("legal", el("p", tx("LEGAL "+long[:300])))), "POST-TITLE,POST-END"},
		{"26 has-sidebar wrapper alone", el("body", c("layout has-sidebar", divart())), "POST-TITLE,POST-END"},
		{"27 div article + div.related-posts links", el("body", divart(), c("related-posts", el("ul", navl...))), "POST-TITLE,POST-END,!NAV-LINK"},
		{"28 div article + long div.footer-content", el("body", divart(), c("footer-content", el("p", tx("FOOTER-TEXT "+long+long+long+long+long+long)))), "POST-TITLE,POST-END,!FOOTER-TEXT"},
	}
	for _, cs := range cases {
		out, _, _ := Readable(context.Background(), el("html", cs.n), 0)
		for _, k := range strings.Split(cs.want, ",") {
			if strings.HasPrefix(k, "!") {
				if strings.Contains(out, k[1:]) {
					t.Errorf("%s: has %q:\n%.400s", cs.name, k[1:], out)
				}
			} else if !strings.Contains(out, k) {
				t.Errorf("%s: missing %q:\n%.400s", cs.name, k, out)
			}
		}
	}
}

// TestBoilerplateSpendsItsOwnBudget: 10 MB of navigation before the
// article doesn't push it out of the main text, and isn't reported as a
// cut there; 10 MB of scripts spends nothing.
func TestBoilerplateSpendsItsOwnBudget(t *testing.T) {
	long := strings.Repeat("The committee met on Tuesday, and agreed on the budget, the hours and the plan. ", 6)
	chunk := strings.Repeat("nav words ", 200000) // 2 MB
	var navs, scripts []*Node
	for range 5 {
		navs = append(navs, el("nav", tx(chunk)))
		scripts = append(scripts, el("script", tx(chunk)))
	}
	page := func(pre []*Node) *Node {
		return el("#document", el("html", el("body", append(append([]*Node{}, pre...), el("article", el("p", tx("ARTICLE-START "+long)), el("p", tx("ARTICLE-END "+long))))...)))
	}
	rd, full, _ := Readable(context.Background(), page(navs), 2<<20)
	if !strings.Contains(rd, "ARTICLE-START") || !strings.Contains(rd, "ARTICLE-END") || full {
		t.Errorf("readability after 10 MB of navigation: full %v, %.200q", full, rd)
	}
	for name, f := range map[string]func(*Node) (string, bool, error){
		"markdown":    func(n *Node) (string, bool, error) { return Markdown(context.Background(), n, nil, 2<<20) },
		"readability": func(n *Node) (string, bool, error) { return Readable(context.Background(), n, 2<<20) },
	} {
		out, full, _ := f(page(scripts))
		if !strings.Contains(out, "ARTICLE-START") || !strings.Contains(out, "ARTICLE-END") || full {
			t.Errorf("%s after 10 MB of scripts: full %v, %.200q", name, full, out)
		}
	}
}
