package chromium

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestSwitchListMatchesFlags ties the Dockerfile's build-time switch list to
// chromiumArgs. Chromium ignores a switch it doesn't know without a word, so
// the build checks each listed one is in the binary; a switch passed but not
// listed, or misspelt, would go unchecked.
func TestSwitchListMatchesFlags(t *testing.T) {
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	flags, err := os.ReadFile("entrypoint/flags.go")
	if err != nil {
		t.Fatal(err)
	}

	// The list: `for sw in a b \` and its continuation lines, up to `; do`.
	m := regexp.MustCompile(`(?s)for sw in (.*?); do`).FindSubmatch(dockerfile)
	if m == nil {
		t.Fatal("no `for sw in …; do` switch list in the Dockerfile")
	}
	listed := strings.Fields(strings.ReplaceAll(string(m[1]), `\`, " "))

	// Every "--switch" literal in chromiumArgs.
	body := regexp.MustCompile(`(?s)func chromiumArgs\(.*?\n}\n`).Find(flags)
	if body == nil {
		t.Fatal("no chromiumArgs in flags.go")
	}
	var passed []string
	for _, sw := range regexp.MustCompile(`"--([a-z0-9-]+)`).FindAllSubmatch(body, -1) {
		if s := string(sw[1]); !slices.Contains(passed, s) {
			passed = append(passed, s)
		}
	}
	if len(passed) < 10 {
		t.Fatalf("found only %v in chromiumArgs; is the pattern stale?", passed)
	}

	for _, sw := range passed {
		if !slices.Contains(listed, sw) {
			t.Errorf("chromiumArgs passes --%s, which the Dockerfile's switch list doesn't check", sw)
		}
	}
	for _, sw := range listed {
		if !slices.Contains(passed, sw) {
			t.Errorf("the Dockerfile's switch list checks --%s, which chromiumArgs no longer passes", sw)
		}
	}
}
