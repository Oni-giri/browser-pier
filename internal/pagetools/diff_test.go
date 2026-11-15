package pagetools

import (
	"fmt"
	"strings"
	"testing"
)

// ctl and txtItem make items for diffItems; parent is an index + 1 (0: none).
func ctl(backend int64, role, name string, parent int, states ...string) item {
	return item{control: true, backend: backend, role: role, name: name, parent: parent, states: states}
}

func txtItem(backend int64, s string, parent int) item {
	return item{backend: backend, text: s, parent: parent}
}

// testLine is how a test item reads: its "ref" is its backend.
func testLine(it item, _ bool) string {
	if !it.control && !it.iframe {
		return it.text
	}
	l := fmt.Sprintf("e%d %s %q", it.backend, it.role, it.name)
	if it.value != "" {
		l += " = " + it.value
	}
	for _, st := range it.states {
		l += " [" + st + "]"
	}
	return l
}

func renderDiff(old, new []item) (string, string) {
	return diffItems(old, new).render(testLine)
}

func TestDiffAddedUnderItsParents(t *testing.T) {
	old := []item{txtItem(1, "Account", 0), txtItem(2, "Your orders", 1), ctl(3, "button", "Menu", 0, "collapsed")}
	new := []item{txtItem(1, "Account", 0), txtItem(2, "Your orders", 1), ctl(4, "link", "Order 42", 2), ctl(3, "button", "Menu", 0, "collapsed")}
	body, trailer := renderDiff(old, new)
	want := "  Account\n    Your orders\n+     e4 link \"Order 42\""
	if body != want || trailer != "" {
		t.Errorf("got\n%s\n(%s)\nwant\n%s", body, trailer, want)
	}
}

func TestDiffRemovedUnderItsOldParents(t *testing.T) {
	old := []item{txtItem(1, "Cart", 0), ctl(2, "button", "Remove", 1), ctl(3, "link", "Checkout", 1), txtItem(9, "Footer", 0)}
	new := []item{txtItem(1, "Cart", 0), ctl(3, "link", "Checkout", 1), txtItem(9, "Footer", 0)}
	body, _ := renderDiff(old, new)
	want := "  Cart\n-   e2 button \"Remove\""
	if body != want {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
	// A parent shown once for two changes under it.
	new = []item{txtItem(1, "Cart", 0), txtItem(9, "Footer", 0)}
	body, _ = renderDiff(old, new)
	want = "  Cart\n-   e2 button \"Remove\"\n-   e3 link \"Checkout\""
	if body != want {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
}

func TestDiffStateChangeIsOneRemovedOneAdded(t *testing.T) {
	old := []item{ctl(1, "button", "Menu", 0, "collapsed"), ctl(2, "checkbox", "Terms", 0), ctl(3, "button", "Pay", 0)}
	new := []item{ctl(1, "button", "Menu", 0, "expanded"), ctl(2, "checkbox", "Terms", 0, "checked"), ctl(3, "button", "Pay", 0, underFrame)}
	body, _ := renderDiff(old, new)
	want := strings.Join([]string{
		`- e1 button "Menu" [collapsed]`, `+ e1 button "Menu" [expanded]`,
		`- e2 checkbox "Terms"`, `+ e2 checkbox "Terms" [checked]`,
		`- e3 button "Pay"`, `+ e3 button "Pay" [under an iframe]`,
	}, "\n")
	if body != want {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
	// A field's value, a secret's too, is its redacted value only.
	pw := ctl(4, "textbox", "Password", 0)
	pw.value = "[empty]"
	filled := pw
	filled.value = "[filled]"
	body, _ = renderDiff([]item{pw}, []item{filled})
	if body != "- e4 textbox \"Password\" = [empty]\n+ e4 textbox \"Password\" = [filled]" {
		t.Errorf("got\n%s", body)
	}
}

func TestDiffIgnoresFocusAlone(t *testing.T) {
	old := []item{ctl(1, "textbox", "Email", 0, "focused"), ctl(2, "button", "Go", 0)}
	new := []item{ctl(1, "textbox", "Email", 0), ctl(2, "button", "Go", 0, "focused")}
	if body, trailer := renderDiff(old, new); body != "" || trailer != diffNoChanges {
		t.Errorf("focus moving: %q (%s)", body, trailer)
	}
}

func TestDiffCapsItsLines(t *testing.T) {
	var new []item
	for i := range 35 {
		new = append(new, ctl(int64(i+1), "link", fmt.Sprint("Result ", i), 0))
	}
	body, trailer := renderDiff([]item{}, new)
	if n := strings.Count(body, "\n") + 1; n != maxDiffLines {
		t.Errorf("%d lines, want %d:\n%s", n, maxDiffLines, body)
	}
	if trailer != "(15 more lines; snapshot for the whole page)" {
		t.Errorf("trailer %q", trailer)
	}
}

func TestDiffWithoutBaseline(t *testing.T) {
	new := []item{ctl(1, "button", "Go", 0)}
	if body, trailer := renderDiff(nil, new); body != "" || trailer != diffNoBaseline {
		t.Errorf("no baseline: %q (%s)", body, trailer)
	}
	// An empty page before is a baseline: everything on it now is new.
	if body, _ := renderDiff([]item{}, new); body != `+ e1 button "Go"` {
		t.Errorf("empty baseline: %q", body)
	}
}

func TestDiffIgnoresMoves(t *testing.T) {
	// The same items, in another order and other tiers (scrolled), in
	// another group: the snapshot would read differently, the page hasn't
	// changed.
	old := []item{txtItem(1, "Intro", 0), ctl(2, "link", "A", 0), ctl(3, "link", "B", 0), txtItem(4, "Outro", 0)}
	new := []item{ctl(3, "link", "B", 0), txtItem(1, "Intro", 0), ctl(2, "link", "A", 0), txtItem(4, "Outro", 0)}
	for i := range new {
		new[i].tier, new[i].group = tierRest, 1
	}
	if body, trailer := renderDiff(old, new); body != "" || trailer != diffNoChanges {
		t.Errorf("moved: %q (%s)", body, trailer)
	}
}

func TestDiffTextChanges(t *testing.T) {
	old := []item{txtItem(1, "Status: idle", 0)}
	new := []item{txtItem(1, "Status: open", 0)}
	if body, _ := renderDiff(old, new); body != "- Status: idle\n+ Status: open" {
		t.Errorf("got\n%s", body)
	}
}

func TestCompactKeepsWholeKeys(t *testing.T) {
	long := strings.Repeat("a", 5000)
	a := compact([]item{txtItem(1, long+"x", 0)})
	b := compact([]item{txtItem(1, long+"y", 0)})
	if a[0].text != b[0].text {
		t.Fatal("compact kept different text")
	}
	if len(a[0].text) > 4*(maxDiffText+1) {
		t.Errorf("compact kept %d bytes of text", len(a[0].text))
	}
	if body, _ := renderDiff(a, b); !strings.HasPrefix(body, "- ") {
		t.Errorf("a change past what is kept isn't seen: %q", body)
	}
	// Uncompacted and compacted copies of one item match.
	if body, trailer := renderDiff(a, []item{txtItem(1, long+"x", 0)}); body != "" || trailer != diffNoChanges {
		t.Errorf("compacted against whole: %q (%s)", body, trailer)
	}
}

func TestDiffDeepParents(t *testing.T) {
	// A chain deeper than maxIndent: indented and looked up at most that far.
	var old []item
	for i := range 30 {
		old = append(old, txtItem(int64(i+1), fmt.Sprint("level ", i), i))
	}
	new := append(append([]item{}, old...), ctl(99, "button", "Deep", 30))
	body, _ := renderDiff(old, new)
	lines := strings.Split(body, "\n")
	last := lines[len(lines)-1]
	if want := "+ " + strings.Repeat("  ", maxIndent) + `e99 button "Deep"`; last != want {
		t.Errorf("last line %q, want %q", last, want)
	}
	if len(lines) != maxIndent+1 {
		t.Errorf("%d lines, want %d context lines and the change:\n%s", len(lines), maxIndent, body)
	}
}

func TestBaselinesStayBounded(t *testing.T) {
	tl := New(nil, nil)
	big := []item{txtItem(1, strings.Repeat("x", 600), 0)}
	for len(big) < 30000 {
		big = append(big, txtItem(int64(len(big)+1), strings.Repeat("y", 600), 0))
	}
	tl.setBaseline("s", "t1", "f", "l", big)
	if tl.baseline("s", "t1") != nil || tl.baseBytes != 0 {
		t.Errorf("a list past maxBaseline was kept (%d bytes)", tl.baseBytes)
	}
	mid := big[:3000] // about 2.4 MB kept
	for i := range 40 {
		tl.setBaseline(fmt.Sprint("s", i%4), fmt.Sprint("t", i), "f", "l", mid)
		if tl.baseBytes > maxBaselines {
			t.Fatalf("%d bytes kept, more than %d", tl.baseBytes, maxBaselines)
		}
	}
	if tl.baseline("s3", "t39") == nil || tl.baseline("s0", "t0") != nil {
		t.Error("not the oldest list dropped first")
	}
	tl.forgetSession("s3")
	for k := range tl.bases {
		if k.session == "s3" {
			t.Errorf("%v kept after its session ended", k)
		}
	}
	sum := 0
	for _, b := range tl.bases {
		sum += b.size
	}
	if sum != tl.baseBytes {
		t.Errorf("baseBytes %d, lists %d", tl.baseBytes, sum)
	}
}

// TestDiffRemovedChildUnderItsChangedParent: a removed item whose
// enclosing item changed (or went too) is printed right after that one's
// removed line, at its depth, never before or outside it (PROJ-114).
func TestDiffRemovedChildUnderItsChangedParent(t *testing.T) {
	old := []item{txtItem(9, "Header", 0), ctl(1, "button", "Menu", 0, "expanded"), ctl(2, "menuitem", "Settings", 2), ctl(3, "menuitem", "Log out", 2), txtItem(8, "Footer", 0)}
	new := []item{txtItem(9, "Header", 0), ctl(1, "button", "Menu", 0, "collapsed"), txtItem(8, "Footer", 0)}
	body, _ := renderDiff(old, new)
	want := strings.Join([]string{
		`- e1 button "Menu" [expanded]`,
		`-   e2 menuitem "Settings"`,
		`-   e3 menuitem "Log out"`,
		`+ e1 button "Menu" [collapsed]`,
	}, "\n")
	if body != want {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
	// Both gone: the parent, then what was in it.
	new = []item{txtItem(9, "Header", 0), txtItem(8, "Footer", 0)}
	body, _ = renderDiff(old, new)
	want = strings.Join([]string{`- e1 button "Menu" [expanded]`, `-   e2 menuitem "Settings"`, `-   e3 menuitem "Log out"`}, "\n")
	if body != want {
		t.Errorf("both gone: got\n%s\nwant\n%s", body, want)
	}
}

// TestDiffSaysAChangePastTheLine: a changed item whose two lines read the
// same (the change is past what a line shows) says so on its added line.
func TestDiffSaysAChangePastTheLine(t *testing.T) {
	long := strings.Repeat("word ", 60)
	old := []item{txtItem(1, long+"old ending", 0)}
	new := []item{txtItem(1, long+"new ending", 0)}
	cut := func(it item, _ bool) string { return capText(it.text, maxDiffText) }
	body, _ := diffItems(old, new).render(cut)
	lines := strings.Split(body, "\n")
	if len(lines) != 2 || lines[0] != "- "+capText(long, maxDiffText) || lines[1] != "+ "+capText(long, maxDiffText)+diffChangedPast {
		t.Errorf("got\n%s", body)
	}
	// Lines that differ say nothing more.
	body, _ = renderDiff([]item{ctl(2, "button", "Menu", 0, "collapsed")}, []item{ctl(2, "button", "Menu", 0, "expanded")})
	if strings.Contains(body, diffChangedPast) {
		t.Errorf("got\n%s", body)
	}
}
