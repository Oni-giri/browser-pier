package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The live view's copy reads the X clipboard's text with xclip (PROJ-194).
func TestClipboard(t *testing.T) {
	for name, c := range map[string]struct {
		out    []byte
		err    error
		code   int
		answer string
	}{
		"text":            {out: []byte("héllo 123\nsecond line"), code: http.StatusOK, answer: "héllo 123\nsecond line"},
		"nothing held":    {err: errors.New("xclip: exit status 1"), code: http.StatusOK, answer: ""},
		"xclip missing":   {err: errors.New(`exec: "xclip": executable file not found in $PATH`), code: http.StatusInternalServerError},
		"killed":          {err: errors.New("xclip: killed by killed"), code: http.StatusInternalServerError},
		"not UTF-8":       {out: []byte{0xff, 0xfe, 'a'}, code: http.StatusUnprocessableEntity},
		"over the cap":    {out: []byte(strings.Repeat("a", maxClipboard+1)), code: http.StatusRequestEntityTooLarge},
		"exactly the cap": {out: []byte(strings.Repeat("a", maxClipboard)), code: http.StatusOK, answer: strings.Repeat("a", maxClipboard)},
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := newTestVisual(false)
			var got []string
			v.output = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("the read has no deadline")
				}
				got = append([]string{name}, args...)
				return c.out, c.err
			}
			rec := serveVisual(v, http.MethodGet, "/input/clipboard", "")
			if rec.Code != c.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if strings.Join(got, " ") != "xclip -selection clipboard -out -target UTF8_STRING" {
				t.Errorf("ran %q", got)
			}
			if c.code == http.StatusOK && (rec.Body.String() != c.answer || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain")) {
				t.Errorf("answer %q, %s", rec.Body.String(), rec.Header().Get("Content-Type"))
			}
		})
	}
	// A clipboard owner that never answers: a timeout, not "empty".
	clipboardWait = 50 * time.Millisecond
	t.Cleanup(func() { clipboardWait = 2 * time.Second })
	hung, _ := newTestVisual(false)
	hung.output = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("xclip: killed by killed")
	}
	if rec := serveVisual(hung, http.MethodGet, "/input/clipboard", ""); rec.Code != http.StatusGatewayTimeout {
		t.Errorf("a hung owner: %d %s", rec.Code, rec.Body)
	}
	// Only GET: nothing writes the remote clipboard.
	v, _ := newTestVisual(false)
	if rec := serveVisual(v, http.MethodPost, "/input/clipboard", "x"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}
