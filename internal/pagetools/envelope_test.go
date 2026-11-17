package pagetools

import (
	"testing"

	"github.com/Oni-giri/browser-pier/internal/fence/fencetest"
)

// env is a page envelope (PROJ-167), as an answer has it.
type env struct {
	marker, source, url, title, format, content, after string
	hasTitle                                           bool
}

func envOf(e fencetest.Envelope) env {
	return env{marker: e.Marker, source: e.Source, url: e.URL, title: e.Title, format: e.Format, content: e.Content, after: e.After, hasTitle: e.HasTitle}
}

// envelope is out's one page envelope.
func envelope(t *testing.T, out string) env {
	t.Helper()
	e, err := fencetest.Page(out)
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	return envOf(e)
}
