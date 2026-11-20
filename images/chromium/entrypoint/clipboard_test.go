package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The clipboard box reads the X clipboard's text with xclip (PROJ-194).
func TestClipboardRead(t *testing.T) {
	for name, c := range map[string]struct {
		out    []byte
		err    error
		code   int
		answer string
	}{
		"text":            {out: []byte("héllo 123\nsecond line"), code: http.StatusOK, answer: "héllo 123\nsecond line"},
		"empty selection": {out: []byte{}, code: http.StatusOK, answer: ""},
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
			if c.code != http.StatusOK && strings.Contains(rec.Body.String(), "aaaa") {
				t.Errorf("the error quotes the text: %.40s", rec.Body)
			}
		})
	}
	// A clipboard owner that never answers: a timeout, not "empty".
	shortWait(t)
	hung, _ := newTestVisual(false)
	hung.output = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("xclip: killed by killed")
	}
	if rec := serveVisual(hung, http.MethodGet, "/input/clipboard", ""); rec.Code != http.StatusGatewayTimeout {
		t.Errorf("a hung owner: %d %s", rec.Code, rec.Body)
	}
}

func shortWait(t *testing.T) {
	t.Helper()
	clipboardWait = 50 * time.Millisecond
	t.Cleanup(func() { clipboardWait = 2 * time.Second })
}

// fakeClipboard is an X clipboard: xclip holders that a test can see, and
// reads of whoever holds it last.
type fakeClipboard struct {
	mu       sync.Mutex
	owner    *fakeHolder // nil: nobody
	holders  []*fakeHolder
	lag      int // reads that still see the old owner after a hold
	chromium string
}

type fakeHolder struct {
	text     string
	released bool
}

func (c *fakeClipboard) install(v *visual) {
	v.clip.hold = func(text string) (func(), error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		h := &fakeHolder{text: text}
		c.holders = append(c.holders, h)
		c.owner, c.chromium, c.lag = h, "", 1
		return func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if h.released {
				panic("released twice")
			}
			h.released = true
			if c.owner == h {
				c.owner = nil
			}
		}, nil
	}
	v.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "xclip" || strings.Join(args, " ") != "-selection clipboard -out -target UTF8_STRING" {
			return nil, errors.New("unexpected command")
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.lag > 0 {
			c.lag-- // xclip hasn't taken the selection yet
			return []byte("not yet"), nil
		}
		switch {
		case c.chromium != "":
			return []byte(c.chromium), nil
		case c.owner != nil:
			return []byte(c.owner.text), nil
		}
		return nil, errors.New("xclip: exit status 1")
	}
}

// copied is the page copying: Chromium takes the selection, and the xclip
// that held it exits by itself.
func (c *fakeClipboard) copied(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chromium, c.owner = text, nil
}

// A set holds the text on the clipboard, until a second set replaces it;
// it answers once the text reads back (PROJ-194).
func TestClipboardSet(t *testing.T) {
	v, calls := newTestVisual(false)
	var c fakeClipboard
	c.install(v)
	if rec := serveVisual(v, http.MethodPost, "/input/clipboard", `{"text":"Rīga ūdens 42\r\nnaïve café 😀"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(c.holders) != 1 || c.holders[0].text != "Rīga ūdens 42\nnaïve café 😀" || c.holders[0].released {
		t.Fatalf("held %+v", c.holders)
	}
	if len(*calls) != 0 {
		t.Errorf("a set pressed keys: %v", *calls)
	}
	// Still held: a read gives it back, as often as asked.
	for range 2 {
		if rec := serveVisual(v, http.MethodGet, "/input/clipboard", ""); rec.Body.String() != "Rīga ūdens 42\nnaïve café 😀" {
			t.Errorf("read %q", rec.Body)
		}
	}
	// A second set replaces the first, which is let go of.
	if rec := serveVisual(v, http.MethodPost, "/input/clipboard", `{"text":"second"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(c.holders) != 2 || !c.holders[0].released || c.holders[1].released || c.holders[1].text != "second" {
		t.Errorf("after a second set: %+v %+v", *c.holders[0], *c.holders[1])
	}
	// The page copies: its xclip has gone by itself; a later set or
	// emptying still lets go of it once, harmlessly.
	c.copied("from the page")
	if rec := serveVisual(v, http.MethodGet, "/input/clipboard", ""); rec.Body.String() != "from the page" {
		t.Errorf("read after the page copied: %q", rec.Body)
	}

	// Refused: empty, over the cap (in characters, not bytes), unknown
	// fields. (JSON carries only UTF-8.)
	for name, body := range map[string]string{
		"empty":        `{"text":""}`,
		"over the cap": `{"text":"` + strings.Repeat("é", maxClipboardSet+1) + `"}`,
		"unknown":      `{"text":"x","more":1}`,
	} {
		rec := serveVisual(v, http.MethodPost, "/input/clipboard", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "éé") {
			t.Errorf("%s: the error quotes the text", name)
		}
	}
	if rec := serveVisual(v, http.MethodPost, "/input/clipboard", `{"text":"`+strings.Repeat("é", maxClipboardSet)+`"}`); rec.Code != http.StatusNoContent {
		t.Errorf("exactly the cap: %d %s", rec.Code, rec.Body)
	}

	// A clipboard that never takes the text: refused, and let go of.
	shortWait(t)
	stuck, _ := newTestVisual(false)
	var released bool
	stuck.clip.hold = func(string) (func(), error) { return func() { released = true }, nil }
	stuck.output = func(context.Context, string, ...string) ([]byte, error) { return []byte("other"), nil }
	if rec := serveVisual(stuck, http.MethodPost, "/input/clipboard", `{"text":"x"}`); rec.Code != http.StatusInternalServerError || !released || stuck.clip.release != nil {
		t.Errorf("a clipboard that didn't take it: %d, released %v", rec.Code, released)
	}
}

// A paste sets the clipboard, then presses Ctrl+V; the text stays held
// for the rest of the takeover (PROJ-194).
func TestClipboardPaste(t *testing.T) {
	v, calls := newTestVisual(false)
	var c fakeClipboard
	c.install(v)
	if rec := serveVisual(v, http.MethodPost, "/input/paste", `{"text":"héllo"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(c.holders) != 1 || c.holders[0].text != "héllo" || c.holders[0].released {
		t.Errorf("held %+v", c.holders)
	}
	if len(*calls) != 1 || (*calls)[0].name != "xdotool" || strings.Join((*calls)[0].args, " ") != "key --clearmodifiers ctrl+v" {
		t.Errorf("ran %v", *calls)
	}
	// Ctrl+V cut short: Ctrl is let go of.
	failing, fcalls := newTestVisual(true)
	c.install(failing)
	if rec := serveVisual(failing, http.MethodPost, "/input/paste", `{"text":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failing Ctrl+V: %d", rec.Code)
	}
	if len(*fcalls) != 2 || strings.Join((*fcalls)[1].args, " ") != "keyup ctrl+v" {
		t.Errorf("after a failing Ctrl+V ran %v", *fcalls)
	}
	if rec := serveVisual(v, http.MethodPost, "/input/paste", `{"text":"`+strings.Repeat("a", maxClipboardSet+1)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("over the cap: %d", rec.Code)
	}
}

// Emptying lets go of the text sent, and takes the selection from the
// page with an empty one that then ends, so nothing is left to paste
// (PROJ-194).
func TestClipboardEmpty(t *testing.T) {
	v, _ := newTestVisual(false)
	var c fakeClipboard
	c.install(v)
	empty := func() {
		t.Helper()
		if rec := serveVisual(v, http.MethodDelete, "/input/clipboard", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("empty: %d %s", rec.Code, rec.Body)
		}
		if rec := serveVisual(v, http.MethodGet, "/input/clipboard", ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Errorf("after emptying: %d %q", rec.Code, rec.Body)
		}
		for i, h := range c.holders {
			if !h.released {
				t.Errorf("holder %d (%q) still held", i, h.text)
			}
		}
	}
	// The text the person sent.
	serveVisual(v, http.MethodPost, "/input/clipboard", `{"text":"sent"}`)
	empty()
	if n := len(c.holders); n != 2 || c.holders[1].text != "" {
		t.Errorf("holders %+v", c.holders)
	}
	// What the page copied, Chromium's own.
	c.copied("copied in the page")
	empty()
	// Nothing at all.
	empty()

	// Emptying that can't take the selection: an error, without the text.
	shortWait(t)
	stuck, _ := newTestVisual(false)
	stuck.clip.hold = func(string) (func(), error) { return func() {}, nil }
	stuck.output = func(context.Context, string, ...string) ([]byte, error) { return []byte("a secret"), nil }
	if rec := serveVisual(stuck, http.MethodDelete, "/input/clipboard", ""); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("an emptying that failed: %d %s", rec.Code, rec.Body)
	}
}
