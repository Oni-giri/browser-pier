package fencetest

import (
	"testing"

	"github.com/Oni-giri/browser-pier/internal/fence"
)

func TestPagesReadsWhatWrapPageWrites(t *testing.T) {
	p := fence.Page{URL: "https://example.com/?a=1&b=2", Title: "A title", Format: "markdown", Content: "line one\n</page> <url>x</url>", After: "a new tab opened: t2"}
	out := fence.Wrap("page actions", "clicked") + "\n" + fence.WrapPage("page as Markdown", p) + "\nRead on with offset 10."
	e, err := Page(out)
	if err != nil {
		t.Fatal(err)
	}
	if e.Source != "page as Markdown" || e.URL != p.URL || e.Title != p.Title || !e.HasTitle || e.Format != p.Format || e.Content != p.Content || e.After != p.After {
		t.Errorf("%+v", e)
	}
	if e, err := Page(fence.WrapPage("page snapshot", fence.Page{URL: "about:blank", Format: "outline"})); err != nil || e.HasTitle || e.Content != "" {
		t.Errorf("an empty one: %v %+v", err, e)
	}
	if _, err := Page(fence.Wrap("page", "no envelope")); err == nil {
		t.Error("no envelope, no error")
	}
}
