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

// A paste holds the text on the clipboard, waits until it reads back,
// presses Ctrl+V, then lets the clipboard go (PROJ-194).
func TestPaste(t *testing.T) {
	pasteHold = 0
	t.Cleanup(func() { pasteHold = 500 * time.Millisecond })
	v, calls := newTestVisual(false)
	var held []string
	released := 0
	v.hold = func(text string) (func(), error) {
		held = append(held, text)
		return func() { released++ }, nil
	}
	reads := 0
	v.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
		reads++
		if reads == 1 {
			return []byte("not yet"), nil // xclip hasn't taken it yet
		}
		return []byte(held[len(held)-1]), nil
	}
	const text = "Rīga ūdens 42\nnaïve café 😀"
	rec := serveVisual(v, http.MethodPost, "/input/paste", `{"text":"Rīga ūdens 42\nnaïve café 😀"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(held) != 1 || held[0] != text || released != 1 || reads < 2 {
		t.Errorf("held %q, released %d, read %d times", held, released, reads)
	}
	if len(*calls) != 1 || (*calls)[0].name != "xdotool" || strings.Join((*calls)[0].args, " ") != "key --clearmodifiers ctrl+v" {
		t.Errorf("ran %v", *calls)
	}
	for _, body := range []string{`{"text":""}`, `{"text":"` + strings.Repeat("a", maxPaste+1) + `"}`} {
		if rec := serveVisual(v, http.MethodPost, "/input/paste", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%.30s: %d", body, rec.Code)
		}
	}
	// A clipboard that never takes the text: refused, and let go.
	clipboardWait = 50 * time.Millisecond
	t.Cleanup(func() { clipboardWait = 2 * time.Second })
	v.output = func(context.Context, string, ...string) ([]byte, error) { return []byte("other"), nil }
	if rec := serveVisual(v, http.MethodPost, "/input/paste", `{"text":"x"}`); rec.Code != http.StatusInternalServerError || released != 2 {
		t.Errorf("a clipboard that didn't take it: %d, released %d", rec.Code, released)
	}
}
