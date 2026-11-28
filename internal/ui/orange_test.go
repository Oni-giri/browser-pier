package ui

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Orange means a person is needed (DESIGN.md), and nothing else: the
// Needs-you status, the header chip, the emphasis card, the warm light, the
// handoff page's reason and the live view's needs-help pill. These tests list
// the places that may be warm, so a caution, a notice or a missing setting
// that reaches for orange fails here, whatever its class is called.

var (
	// What makes a CSS rule orange: an orange token, the orange light's
	// literal, an orange hex, or the pulsing dot's keyframes.
	orangeMark = regexp.MustCompile(`(?i)orange|255,\s*106,\s*61|#(?:ff6a3d|c2410c|fda387|fff1eb|43291d)\b|\bpulse\b|--emphasis-bg\b`)

	// The tokens that may hold orange.
	orangeTokens = []string{"--orange", "--orange-text", "--orange-tint", "--glow-orange", "--ring-orange", "--emphasis-bg"}

	// The rules that may use them: all of them say a person is needed.
	orangeRules = []string{
		"body.has-help",
		".chip-help", ".chip-help::before",
		".pill.help", ".pill.help::before",
		".dot-text.help", ".dot-text.help::before",
		".rows>li.help", ".table tr.help td",
		".card.help", ".cell.emph,.card.emph",
		"#who.help", "#who.help::before",
		".log li.attention", ".log li.attention::before",
	}

	// The classes those rules hang on.
	warmClasses = []string{"help", "emph", "attention", "has-help", "chip-help"}

	// Where the markup may use a warm class, word for word (lit) or, where
	// the words may change but the state may not, as a pattern. Paths are
	// relative to this package; templates/ and static/ are embedded.
	warmMarkup = map[string][]*regexp.Regexp{
		"templates/layout.html": {
			lit(`<body{{if .Help.Count}} class="has-help"{{end}}>`),
			lit(`<a class="chip-help" href="{{.Link}}">{{.Count}} {{plural .Count "needs" "need"}} you</a>`),
		},
		"templates/overview.html": {
			lit(`<section class="cell emph span-8" aria-label="{{.Name}} needs you">`),
			lit(`<span class="pill help">Needs you</span>`),
			lit(`<li{{if eq .Status.Key "help"}} class="help"{{end}}>`),
		},
		"templates/parts.html": {
			lit(`<article class="card{{if eq .Status.Key "help"}} help{{if .Emph}} emph{{end}}{{end}}" id="card-{{.Name}}">`),
			lit(`<tr{{if eq .Status.Key "help"}} class="help"{{end}}>`),
			// The activity log's entry: Attention is set only for a handoff.
			lit(`<li class="{{.Kind}}{{if .Attention}} attention{{end}}">`),
		},
		"../handoff/handoff.html": {
			lit(`<span class="pill help plain">{{.Reason}}</span>`),
		},
		"static/app.js": {
			lit(`document.body.classList.toggle("has-help", !!$(".chip-help", e.target))`),
		},
		"../webrtc/live.js": {
			// The live view's pill while the browser needs help, whatever its
			// label says (it may be reworded). That only this state is
			// orange, and that the pill doesn't stay orange after it, is
			// TestLiveViewPillIsOrangeOnlyWhileHelpIsNeeded's, in a browser.
			regexp.MustCompile(`else if \(s === "needs_help"\) \{\s*label = [^;]+;\s*cls = "help";\s*\}`),
		},
	}
)

func lit(s string) *regexp.Regexp { return regexp.MustCompile(regexp.QuoteMeta(s)) }

type cssRule struct{ at, sel, decl string }

// cssRules splits a stylesheet into its innermost rules, each with the
// at-rules around it (so the first rule in an @media block keeps its own
// selector).
func cssRules(css string) []cssRule {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var rules []cssRule
	var open []string
	start := 0
	for i := 0; i < len(css); i++ {
		switch css[i] {
		case '{':
			open = append(open, strings.TrimSpace(css[start:i]))
			start = i + 1
		case '}':
			if len(open) == 0 {
				continue
			}
			sel := open[len(open)-1]
			open = open[:len(open)-1]
			if body := strings.TrimSpace(css[start:i]); body != "" {
				rules = append(rules, cssRule{strings.Join(open, " "), sel, body})
			}
			start = i + 1
		}
	}
	return rules
}

// themeRoots are the selectors that define the tokens, per theme. Only
// their custom properties are tokens; a rule under them (":root[data-theme=
// dark] .notice") or an ordinary declaration on them is checked like any.
var themeRoots = []string{":root", ":root[data-theme=light]", ":root[data-theme=dark]", ":root:not([data-theme=light])"}

// orangeCSSProblems lists what in css is orange outside orangeRules and
// orangeTokens, and the orangeRules that aren't orange any more.
func orangeCSSProblems(css string) []string {
	var problems []string
	seen := map[string]bool{}
	for _, r := range cssRules(css) {
		if slices.Contains(themeRoots, r.sel) {
			for _, d := range strings.Split(r.decl, ";") {
				d = strings.TrimSpace(d)
				name, value, ok := strings.Cut(d, ":")
				name = strings.TrimSpace(name)
				switch {
				case !ok || !orangeMark.MatchString(value):
				case !strings.HasPrefix(name, "--"):
					problems = append(problems, fmt.Sprintf("%s %s{%s}: orange on the page itself; orange is only for a person needed", r.at, r.sel, d))
				case !slices.Contains(orangeTokens, name):
					problems = append(problems, fmt.Sprintf("%s %s{%s}: a new orange token; orange is only for a person needed", r.at, r.sel, d))
				}
			}
			continue
		}
		if !orangeMark.MatchString(r.decl) || r.at == "@keyframes pulse" {
			continue
		}
		if slices.Contains(orangeRules, r.sel) {
			seen[r.sel] = true
			continue
		}
		problems = append(problems, fmt.Sprintf("%s %s{%s}: orange is only for a person needed; a note, caution or setting is neutral", r.at, r.sel, r.decl))
	}
	for _, sel := range orangeRules {
		if !seen[sel] {
			problems = append(problems, sel+" is no longer orange: update orangeRules")
		}
	}
	return problems
}

func TestOrangeCSSOnlyWhereSomeoneIsNeeded(t *testing.T) {
	b, err := embedded.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cssRules(string(b))); n < 100 {
		t.Fatalf("app.css: read only %d rules", n)
	}
	for _, p := range orangeCSSProblems(string(b)) {
		t.Errorf("app.css: %s", p)
	}
}

// attrValues returns the value of every name attribute in src, in double
// quotes, single quotes or none, whatever the name's case. A template
// action in a value ({{if …}}) may hold quotes of its own; it is left out,
// as a space, but for keepActions, and the words it switches on are kept.
func attrValues(src, name string, keepActions bool) []string {
	var all []string
	lower := strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, src) // ASCII only, so its indices are src's
	for i := 0; ; {
		j := strings.Index(lower[i:], name)
		if j < 0 {
			return all
		}
		i += j + len(name)
		if at := i - len(name); at > 0 && !strings.ContainsRune(" \t\n\r/", rune(src[at-1])) {
			continue // the end of another name, data-class
		}
		k := i
		for k < len(src) && strings.ContainsRune(" \t\n\r", rune(src[k])) {
			k++
		}
		if k == len(src) || src[k] != '=' {
			continue
		}
		k++
		for k < len(src) && strings.ContainsRune(" \t\n\r", rune(src[k])) {
			k++
		}
		end := func(c byte) bool { return strings.ContainsRune(" \t\n\r>", rune(c)) }
		if k < len(src) && (src[k] == '"' || src[k] == '\'') {
			q := src[k]
			end = func(c byte) bool { return c == q }
			k++
		}
		var v strings.Builder
		for k < len(src) && !end(src[k]) {
			if strings.HasPrefix(src[k:], "{{") {
				e := strings.Index(src[k:], "}}")
				if e < 0 {
					return all
				}
				if keepActions {
					v.WriteString(src[k : k+e+2])
				} else {
					v.WriteByte(' ')
				}
				k += e + 2
				continue
			}
			v.WriteByte(src[k])
			k++
		}
		all = append(all, v.String())
		i = k
	}
}

// classes returns the literal class names of every class attribute.
func classes(src string) [][]string {
	var all [][]string
	for _, v := range attrValues(src, "class", false) {
		all = append(all, strings.Fields(v))
	}
	return all
}

// stripJSComments removes the comments from JS source: prose, which may
// quote a label. It knows strings and template literals, not regular
// expression literals, which in these files hold no quote, // or /*.
func stripJSComments(src string) string {
	var out strings.Builder
	var quote byte // the string being read, if any
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == quote || (c == '\n' && quote != '`') {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
			out.WriteByte(c)
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out.WriteByte('\n')
			}
		case strings.HasPrefix(src[i:], "/*"):
			e := strings.Index(src[i+2:], "*/")
			if e < 0 {
				return out.String()
			}
			i += e + 3
			out.WriteByte(' ')
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

var (
	jsString  = regexp.MustCompile("\"([^\"\\\\\n]*)\"|'([^'\\\\\n]*)'|`([^`]*)`")
	selectorS = regexp.MustCompile(`[\s.#,>+~:()\[\]]+`)
)

func readMarkup(t *testing.T, name string) string {
	t.Helper()
	var b []byte
	var err error
	if strings.HasPrefix(name, "../") {
		b, err = os.ReadFile(name)
	} else {
		b, err = embedded.ReadFile(name)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// warmMarkupProblems lists the warm classes and orange in src, the file
// name, outside its warmMarkup places, and the places it no longer has.
func warmMarkupProblems(name, src string) []string {
	var problems []string
	js := strings.HasSuffix(name, ".js")
	if js {
		src = stripJSComments(src)
	}
	for _, ok := range warmMarkup[name] {
		if !ok.MatchString(src) {
			problems = append(problems, fmt.Sprintf("no longer has %s: update warmMarkup", ok))
		}
		src = ok.ReplaceAllLiteralString(src, "")
	}
	if js {
		for _, m := range jsString.FindAllStringSubmatch(src, -1) {
			s := m[1] + m[2] + m[3]
			for _, w := range selectorS.Split(s, -1) {
				if slices.Contains(warmClasses, w) {
					problems = append(problems, fmt.Sprintf("%q: a warm class outside a Needs-you place", s))
				}
			}
			if orangeMark.MatchString(s) {
				problems = append(problems, fmt.Sprintf("%q: orange is only for a person needed", s))
			}
		}
		return problems
	}
	for _, cs := range classes(src) {
		for _, c := range cs {
			if slices.Contains(warmClasses, c) {
				problems = append(problems, fmt.Sprintf("class %q: %q is warm; a note, caution or setting is neutral", strings.Join(cs, " "), c))
			}
		}
	}
	for _, s := range attrValues(src, "style", true) {
		if orangeMark.MatchString(s) {
			problems = append(problems, fmt.Sprintf("style %q: orange is only for a person needed", s))
		}
	}
	return problems
}

func TestWarmMarkupOnlyWhereSomeoneIsNeeded(t *testing.T) {
	templates, err := fs.Glob(embedded, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	files := append(templates, "../handoff/handoff.html", "static/app.js", "../webrtc/live.js", "../handoff/handoff.js")
	for _, name := range files {
		for _, p := range warmMarkupProblems(name, readMarkup(t, name)) {
			t.Errorf("%s: %s", name, p)
		}
	}
}

// The checks above catch what the Codex review of PROJ-173/175 showed
// they missed, each seeded into the real file.
func TestOrangeChecksCatchSeededViolations(t *testing.T) {
	css, err := embedded.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if p := orangeCSSProblems(string(css)); len(p) != 0 {
		t.Fatalf("app.css as it is: %q", p)
	}
	for _, seed := range []string{
		// A rule under a theme root is a rule like any.
		`:root[data-theme=dark] .notice{background:var(--orange-tint)}`,
		`@media (prefers-color-scheme:dark){:root:not([data-theme=light]) .notice{color:var(--orange-text)}}`,
		// An ordinary declaration on a theme root colours the page.
		`:root[data-theme=dark]{color:var(--orange)}`,
		// A new orange token, as before.
		`:root{--caution:#FF6A3D}`,
	} {
		if p := orangeCSSProblems(string(css) + "\n" + seed); len(p) != 1 {
			t.Errorf("app.css with %s: %q, want one problem", seed, p)
		}
	}

	const page = "templates/settings.html"
	html := readMarkup(t, page)
	if p := warmMarkupProblems(page, html); len(p) != 0 {
		t.Fatalf("%s as it is: %q", page, p)
	}
	for _, seed := range []string{
		`<p class='notice' style='color:var(--orange-text)'>Export</p>`,
		`<p class="notice" style='color:var(--orange-text)'>Export</p>`,
		`<p class='notice help'>Export</p>`,
		`<p class=help>Export</p>`,
		`<p CLASS = "help">Export</p>`,
		`<p class=notice style=color:var(--orange)>Export</p>`,
	} {
		if p := warmMarkupProblems(page, html+seed); len(p) != 1 {
			t.Errorf("%s with %s: %q, want one problem", page, seed, p)
		}
	}
	// Not a class attribute.
	if p := warmMarkupProblems(page, html+`<p data-class="help">x</p>`); len(p) != 0 {
		t.Errorf("data-class: %q", p)
	}

	const live = "../webrtc/live.js"
	js := readMarkup(t, live)
	if p := warmMarkupProblems(live, js); len(p) != 0 {
		t.Fatalf("%s as it is: %q", live, p)
	}
	// A comment may quote a label, at the end of a line or in a block; a
	// URL in a string isn't a comment.
	for seed, want := range map[string]int{
		`label = "Idle"; // previously "Needs you"`: 0,
		`/* was cls = "help" */ label = "Idle";`:    0,
		`const u = "http://x"; const c = "help";`:   1, // the string isn't cut at its //
	} {
		if got := warmMarkupProblems(live, js+"\n"+seed+"\n"); len(got) != want {
			t.Errorf("%s with %s: %q, want %d problems", live, seed, got, want)
		}
	}
	// The needs-help branch may be reworded, comment and all.
	reworded := strings.Replace(js, `label = "Needs you"; cls = "help";`, `label = "Someone is needed"; /* reworded */ cls = "help";`, 1)
	if reworded == js {
		t.Fatal("live.js's needs-help branch moved: update this test")
	}
	if p := warmMarkupProblems(live, reworded); len(p) != 0 {
		t.Errorf("live.js reworded: %q", p)
	}
	// A warm class anywhere else in it is caught.
	if p := warmMarkupProblems(live, js+"\nwho.className = \"pill help\";\n"); len(p) != 1 {
		t.Errorf("live.js with another warm class: %q", p)
	}
}

// The live view's pill names the needs-help state with the design's word,
// the one every other page uses (DESIGN.md, Status vocabulary).
func TestLiveViewSaysNeedsYou(t *testing.T) {
	js := readMarkup(t, "../webrtc/live.js")
	m := regexp.MustCompile(`else if \(s === "needs_help"\) \{\s*label = "([^"]*)";`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("live.js's needs-help branch moved: update this test")
	}
	if m[1] != stHelp.Label {
		t.Errorf("the live view says %q while help is needed, want %q", m[1], stHelp.Label)
	}
}

// A missing handoff PIN waits for nobody: its pill is the plain grey one.
func TestMissingPINIsGrey(t *testing.T) {
	f := newFixture(t)
	for path, label := range map[string]string{"/agents": "No PIN", "/settings": "Not set"} {
		r := f.get(path)
		want(t, r, http.StatusOK, `<span class="pill">`+label+`</span>`)
		if strings.Contains(r.body, "pill help") {
			t.Errorf("%s is orange with nobody needed", path)
		}
	}
}
