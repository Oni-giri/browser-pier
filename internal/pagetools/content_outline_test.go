package pagetools

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var (
	markerRE = regexp.MustCompile(`[0-9a-f]{16}`)
	refRE2   = regexp.MustCompile(`\b[a-z][a-z0-9]e([0-9]+)\b`)
)

// normal is out with the fence's marker and the refs' epoch made the
// same each time.
func normal(out string) string {
	return refRE2.ReplaceAllString(markerRE.ReplaceAllString(out, "MARKER"), "REFe$1")
}

func (f *fixture) content(in ContentIn) (string, error) {
	f.t.Helper()
	out, err := f.tools.Content(context.Background(), f.session(), f.who, in)
	f.closedAfter()
	return out, err
}

// page_content's outline is snapshot (PROJ-167): on two identical pages
// with fresh state, they answer the same once the marker and ref epoch
// are set aside.
func TestContentOutlineIsSnapshot(t *testing.T) {
	type step struct {
		max  *int
		diff bool
		// change opens the menu before the read.
		change bool
	}
	for name, steps := range map[string][]step{
		"plain":            {{}},
		"max_chars":        {{max: new(300)}},
		"max_chars 0":      {{max: new(0)}},
		"diff, no earlier": {{diff: true}},
		"diff, changed":    {{max: new(2000)}, {diff: true, max: new(2000), change: true}},
	} {
		t.Run(name, func(t *testing.T) {
			var outs [2][]string
			for i, viaContent := range []bool{true, false} {
				f := newFixture(t)
				p := newMenuPage(f.fb)
				f.fb.addTab("https://example.com/menu", p.doc, true)
				for _, st := range steps {
					if st.change {
						f.fb.mu.Lock()
						f.fb.behave[p.menu].onClick()
						f.fb.mu.Unlock()
					}
					var out string
					var err error
					if viaContent {
						out, err = f.content(ContentIn{Format: "outline", MaxChars: st.max, Diff: st.diff})
					} else {
						out, err = f.tools.Snapshot(context.Background(), f.session(), f.who, SnapshotIn{MaxChars: st.max, Diff: st.diff})
						f.closedAfter()
					}
					if err != nil {
						t.Fatal(err)
					}
					outs[i] = append(outs[i], normal(out))
				}
				if acts := f.m.Activity(f.sess.ID); len(acts) == 0 || acts[0].What != "Read the page" {
					t.Errorf("activity: %+v", acts)
				}
			}
			if last := outs[0][len(outs[0])-1]; steps[len(steps)-1].change && !strings.Contains(last, `format="outline-diff"`) {
				t.Fatalf("not a diff:\n%s", last)
			}
			for k := range outs[0] {
				if outs[0][k] != outs[1][k] {
					t.Errorf("read %d: outline\n%s\nsnapshot\n%s", k, outs[0][k], outs[1][k])
				}
			}
		})
	}
}

// Reading the outline counts as a snapshot: act's changes and a diff
// start from it.
func TestContentOutlineCountsAsASnapshot(t *testing.T) {
	f := newFixture(t)
	p := newMenuPage(f.fb)
	f.fb.addTab("https://example.com/menu", p.doc, true)
	out, err := f.content(ContentIn{Format: "outline"})
	if err != nil {
		t.Fatal(err)
	}
	menu := regexp.MustCompile(`([a-z][a-z0-9]e[0-9]+) button "Menu"`).FindStringSubmatch(out)
	if menu == nil {
		t.Fatalf("no Menu ref:\n%s", out)
	}
	out, err = f.act(Step{Action: "click", Ref: menu[1], Pace: paceInstant})
	if err != nil {
		t.Fatal(err)
	}
	if e := envelope(t, changesOf(out)); e.format != "outline-diff" || !strings.Contains(e.content, `button "Menu" [expanded]`) {
		t.Errorf("act's changes don't start from the outline:\n%s", out)
	}
	if out, err := f.content(ContentIn{Format: "outline", Diff: true}); err != nil || out != diffNoChanges {
		t.Errorf("a diff after act's changes: %v %q", err, out)
	}
}

func TestContentParameters(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/dir/article", articlePage(), true)
	for _, in := range []ContentIn{
		{Format: "markdown", Diff: true},
		{Format: "readability", Diff: true},
		{Format: "outline", Offset: 10},
		{Format: "outline", Diff: true, MaxChars: new(0)},
		{Format: "outline", MaxChars: new(MaxMaxChars + 1)},
		{Format: "markdown", MaxChars: new(MaxContentChars + 1)},
	} {
		if _, err := f.content(in); !isStatus(err, 400) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if _, err := f.content(ContentIn{Format: "html"}); !isStatus(err, 400) || !strings.Contains(err.Error(), "outline, markdown, readability, screenshot or pdf") {
		t.Errorf("format html: %v", err)
	}
	// max_chars 0: an empty content tag for outline, the default for markdown.
	out, err := f.content(ContentIn{Format: "outline", MaxChars: new(0)})
	if err != nil || envelope(t, out).content != "" {
		t.Errorf("outline with max_chars 0: %v\n%s", err, out)
	}
	out, err = f.content(ContentIn{Format: "markdown", MaxChars: new(0)})
	if err != nil || !strings.Contains(envelope(t, out).content, "# Library opens") {
		t.Errorf("markdown with max_chars 0: %v\n%s", err, out)
	}
}

// In Visual mode, outline is refused as the page tools are, with their
// words, not the content path's.
func TestContentOutlineInVisualMode(t *testing.T) {
	f := newFixture(t)
	s, _, err := f.m.Start(context.Background(), "shot", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tools.Content(context.Background(), s, f.who, ContentIn{Format: "outline"})
	_, want := f.tools.Snapshot(context.Background(), s, f.who, SnapshotIn{})
	if !isStatus(err, http.StatusConflict) || want == nil || err.Error() != want.Error() {
		t.Errorf("outline in Visual mode: %v, want %v", err, want)
	}
	if f.fb.Dials() != 0 {
		t.Error("outline connected to a Visual-mode browser")
	}
}
