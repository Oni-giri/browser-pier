package ui

import (
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
// handoff page's reason and the live view's "Needs help". These tests list
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
		"#who.help::before",
		".log li.attention", ".log li.attention::before",
	}

	// The classes those rules hang on.
	warmClasses = []string{"help", "emph", "attention", "has-help", "chip-help"}

	// Where the markup may use a warm class, word for word. Paths are
	// relative to this package; templates/ and static/ are embedded.
	warmMarkup = map[string][]string{
		"templates/layout.html": {
			`<body{{if .Help.Count}} class="has-help"{{end}}>`,
			`<a class="chip-help" href="{{.Link}}">{{.Count}} {{plural .Count "needs" "need"}} you</a>`,
		},
		"templates/overview.html": {
			`<section class="cell emph span-8" aria-label="{{.Name}} needs you">`,
			`<span class="pill help">Needs you</span>`,
			`<li{{if eq .Status.Key "help"}} class="help"{{end}}>`,
		},
		"templates/parts.html": {
			`<article class="card{{if eq .Status.Key "help"}} help{{if .Emph}} emph{{end}}{{end}}" id="card-{{.Name}}">`,
			`<tr{{if eq .Status.Key "help"}} class="help"{{end}}>`,
		},
		"templates/profile.html": {
			`<li{{if .Attention}} class="attention"{{end}}>`,
		},
		"../handoff/handoff.html": {
			`<span class="pill help plain">{{.Reason}}</span>`,
		},
		"static/app.js": {
			`document.body.classList.toggle("has-help", !!$(".chip-help", e.target))`,
		},
		"../webrtc/live.js": {
			`label = "Needs help"; cls = "help";`,
		},
	}
)

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

func TestOrangeCSSOnlyWhereSomeoneIsNeeded(t *testing.T) {
	b, err := embedded.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	rules := cssRules(string(b))
	if len(rules) < 100 {
		t.Fatalf("app.css: read only %d rules", len(rules))
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if strings.HasPrefix(r.sel, ":root") {
			for _, d := range strings.Split(r.decl, ";") {
				name, value, ok := strings.Cut(strings.TrimSpace(d), ":")
				if ok && strings.HasPrefix(name, "--") && orangeMark.MatchString(value) && !slices.Contains(orangeTokens, name) {
					t.Errorf("app.css: %s{%s}: a new orange token; orange is only for a person needed", r.sel, strings.TrimSpace(d))
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
		t.Errorf("app.css: %s %s{%s}: orange is only for a person needed; a note, caution or setting is neutral", r.at, r.sel, r.decl)
	}
	for _, sel := range orangeRules {
		if !seen[sel] {
			t.Errorf("app.css: %s is no longer orange: update orangeRules", sel)
		}
	}
}

// classes returns the literal class names of every class attribute, leaving
// out template actions but keeping the words they switch on.
func classes(src string) [][]string {
	var all [][]string
	for rest := src; ; {
		i := strings.Index(rest, `class="`)
		if i < 0 {
			return all
		}
		rest = rest[i+len(`class="`):]
		var v strings.Builder
		for len(rest) > 0 && rest[0] != '"' {
			if strings.HasPrefix(rest, "{{") {
				j := strings.Index(rest, "}}")
				if j < 0 {
					return all
				}
				rest = rest[j+2:]
				v.WriteByte(' ')
				continue
			}
			v.WriteByte(rest[0])
			rest = rest[1:]
		}
		all = append(all, strings.Fields(v.String()))
	}
}

var (
	jsString  = regexp.MustCompile("\"([^\"\\\\\n]*)\"|'([^'\\\\\n]*)'|`([^`]*)`")
	selectorS = regexp.MustCompile(`[\s.#,>+~:()\[\]]+`)
	styleAttr = regexp.MustCompile(`style="[^"]*"`)
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

func TestWarmMarkupOnlyWhereSomeoneIsNeeded(t *testing.T) {
	templates, err := fs.Glob(embedded, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	files := append(templates, "../handoff/handoff.html", "static/app.js", "../webrtc/live.js", "../handoff/handoff.js")
	for _, name := range files {
		src := readMarkup(t, name)
		for _, ok := range warmMarkup[name] {
			if !strings.Contains(src, ok) {
				t.Errorf("%s: no longer has %s: update warmMarkup", name, ok)
			}
			src = strings.ReplaceAll(src, ok, "")
		}
		if strings.HasSuffix(name, ".js") {
			for _, m := range jsString.FindAllStringSubmatch(src, -1) {
				s := m[1] + m[2] + m[3]
				for _, w := range selectorS.Split(s, -1) {
					if slices.Contains(warmClasses, w) {
						t.Errorf("%s: %q: a warm class outside a Needs-you place", name, s)
					}
				}
				if orangeMark.MatchString(s) {
					t.Errorf("%s: %q: orange is only for a person needed", name, s)
				}
			}
			continue
		}
		for _, cs := range classes(src) {
			for _, c := range cs {
				if slices.Contains(warmClasses, c) {
					t.Errorf("%s: class %q: %q is warm; a note, caution or setting is neutral", name, strings.Join(cs, " "), c)
				}
			}
		}
		for _, s := range styleAttr.FindAllString(src, -1) {
			if orangeMark.MatchString(s) {
				t.Errorf("%s: %s: orange is only for a person needed", name, s)
			}
		}
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
