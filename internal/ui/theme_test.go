package ui

import (
	"regexp"
	"strings"
	"testing"
)

// The blue, sky and orange lights and the icon hues are the same in both
// themes; every other colour is a token, so dark mode can't miss one.
var (
	colourLiteral = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|(?:rgba?|hsla?)\([^)]*\)`)
	accentLight   = regexp.MustCompile(`^rgba\((47,107,255|56,189,248|255,106,61),[.0-9]+\)$`)
	iconHue       = regexp.MustCompile(`--hue:#[0-9A-F]{6}\}`)
)

func TestCSSColoursAreTokens(t *testing.T) {
	b, err := embedded.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, "*{box-sizing:border-box}")
	if i < 0 {
		t.Fatal("app.css: can't find where the tokens end")
	}
	line := strings.Count(css[:i], "\n") + 1
	for _, l := range strings.Split(css[i:], "\n") {
		for _, loc := range colourLiteral.FindAllStringIndex(l, -1) {
			c := l[loc[0]:loc[1]]
			if accentLight.MatchString(c) || iconHue.MatchString(l[max(loc[0]-6, 0):min(loc[1]+1, len(l))]) {
				continue
			}
			t.Errorf("app.css:%d: %s isn't a token: add it to :root as light-dark(light, dark)", line, c)
		}
		line++
	}
}
