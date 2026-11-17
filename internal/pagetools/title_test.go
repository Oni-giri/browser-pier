package pagetools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// titled sets a tab's title, as the page would.
func (f *fixture) titled(tb *ftab, title string) {
	f.fb.mu.Lock()
	defer f.fb.mu.Unlock()
	tb.title = title
}

// The page tools note the active tab's title they read for their answer,
// in an entry of its own (PROJ-158), and the browser is asked nothing
// more for it.
func TestPageToolsNoteTheTitle(t *testing.T) {
	const title = "Inbox (3) – Example Mail"
	cases := []struct {
		tool string
		run  func(f *fixture) error
	}{
		{"snapshot", func(f *fixture) error { _, err := f.snapshot(nil); return err }},
		{"snapshot diff", func(f *fixture) error {
			_, err := f.tools.Snapshot(context.Background(), f.session(), f.who, SnapshotIn{Diff: true})
			return err
		}},
		{"navigate", func(f *fixture) error {
			_, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{Action: "reload"})
			return err
		}},
		{"tabs", func(f *fixture) error { _, err := f.tabs(TabsIn{Action: "switch", ID: "t1"}); return err }},
		{"page_content", func(f *fixture) error { _, err := f.text(TextIn{Format: "markdown"}); return err }},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			f := newFixture(t)
			tb := f.fb.addTab("https://mail.example/inbox", loginPage(), true)
			f.titled(tb, title)
			if c.tool == "navigate" {
				// A reload keeps the address; the fake names the page
				// after it.
				f.fb.mu.Lock()
				f.fb.scripted = nil
				f.fb.mu.Unlock()
			}
			if err := c.run(f); err != nil {
				t.Fatal(err)
			}
			want := title
			if c.tool == "navigate" {
				want = "Page https://mail.example/inbox"
			}
			var titles []sessions.Activity
			for _, a := range f.m.Activity(f.sess.ID) {
				if a.Kind == sessions.KindPage {
					titles = append(titles, a)
					continue
				}
				if strings.Contains(a.What, want) || a.Title != "" {
					t.Errorf("an action entry holds the title: %+v", a)
				}
			}
			if len(titles) != 1 || titles[0].Title != want || titles[0].What != "Page title" || titles[0].Agent != "" {
				t.Fatalf("title entries: %+v", titles)
			}
			// Again, same title: no new entry.
			if err := c.run(f); err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, a := range f.m.Activity(f.sess.ID) {
				if a.Kind == sessions.KindPage {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d title entries after the same title again", n)
			}
		})
	}
}

// methodsFor runs the page tools that note titles on a fresh fixture and
// lists the CDP methods the browser was sent, in order.
func methodsFor(t *testing.T) []string {
	f := newFixture(t)
	f.fb.addTab("https://mail.example/inbox", loginPage(), true)
	if _, err := f.snapshot(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tabs(TabsIn{Action: "switch", ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.text(TextIn{Format: "markdown"}); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range f.fb.Received() {
		out = append(out, m.Method)
	}
	return out
}

// TestTitlesAskTheBrowserNothing prints the methods the title-noting
// tools send (go test -v -run TestTitlesAskTheBrowserNothing), to compare
// with a build without PROJ-158; the same run twice must send the same.
func TestTitlesAskTheBrowserNothing(t *testing.T) {
	a, b := methodsFor(t), methodsFor(t)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("two runs sent different methods:\n%v\n%v", a, b)
	}
	t.Logf("methods: %s", strings.Join(a, " "))
}
