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

func shortWait(t *testing.T) {
	t.Helper()
	clipboardWait, emptyWait, emptyRead = 50*time.Millisecond, 100*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() {
		clipboardWait, emptyWait, emptyRead = 2*time.Second, 1500*time.Millisecond, 500*time.Millisecond
	})
}

// fakeX is X's two selections as xclip sees them: who owns each, what
// it offers, and the xclip holders started.
type fakeX struct {
	mu      sync.Mutex
	owners  map[string]*fakeOwner
	pending map[string]*fakeOwner // an xclip that takes its selection after lag reads
	lag     int
	holders []*fakeHolder
	// stuck: a holder never takes its selection. readErr: every read fails.
	stuck   bool
	readErr error
	// steal: how many times the page takes a selection back just after an
	// emptying xclip took it (a copy landing late), which ends that xclip.
	steal int
}

type fakeOwner struct {
	kind   string // "xclip", "chromium" or "image"
	target string // an xclip's: "" for text, or emptyTarget
	text   string
	holder *fakeHolder
}

type fakeHolder struct {
	selection, target, text string
	released                bool
	gone                    chan struct{} // closed when it exits by itself
}

func newFakeX(v *visual) *fakeX {
	x := &fakeX{owners: map[string]*fakeOwner{}, pending: map[string]*fakeOwner{}}
	v.clip.hold = func(selection, target, text string) (func(), <-chan struct{}, error) {
		x.mu.Lock()
		defer x.mu.Unlock()
		h := &fakeHolder{selection: selection, target: target, text: text, gone: make(chan struct{})}
		x.holders = append(x.holders, h)
		o := &fakeOwner{kind: "xclip", target: target, text: text, holder: h}
		if !x.stuck {
			x.pending[selection], x.lag = o, 1
		}
		return func() {
			x.mu.Lock()
			defer x.mu.Unlock()
			if h.released {
				panic("released twice")
			}
			h.released = true
			if cur := x.owners[selection]; cur != nil && cur.holder == h {
				delete(x.owners, selection) // X: an owner that goes leaves none
			}
			if p := x.pending[selection]; p != nil && p.holder == h {
				delete(x.pending, selection)
			}
		}, h.gone, nil
	}
	v.clip.read = func(ctx context.Context, selection, target string) ([]byte, bool, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, false, errors.New("a read without a deadline")
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		if x.readErr != nil {
			return nil, false, x.readErr
		}
		if x.lag > 0 {
			x.lag-- // xclip hasn't taken it yet
		} else {
			for sel, o := range x.pending {
				if o.target == emptyTarget && x.steal > 0 {
					// The page's late copy: it owns the selection again,
					// and the xclip that took it exits.
					x.steal--
					x.owners[sel] = &fakeOwner{kind: "chromium", text: "copied late"}
					close(o.holder.gone)
					delete(x.pending, sel)
					continue
				}
				x.owners[sel] = o
				delete(x.pending, sel)
			}
		}
		o := x.owners[selection]
		if o == nil {
			return nil, false, nil
		}
		switch {
		case target == "TARGETS":
			switch {
			case o.kind == "image":
				return []byte("TARGETS\nimage/png\n"), true, nil
			case o.kind == "xclip" && o.target != "":
				return []byte("TARGETS\n" + o.target + "\n"), true, nil
			}
			return []byte("TARGETS\nUTF8_STRING\n"), true, nil
		case o.kind == "image":
			return nil, false, nil
		case o.kind == "xclip" && o.target != "":
			return nil, true, nil // xclip answers any target, with nothing
		}
		return []byte(o.text), true, nil
	}
	return x
}

// page makes Chromium the owner of selection, holding text ("" for an
// image): the page copied, or text is selected in it. An xclip that held
// the selection exits by itself.
func (x *fakeX) page(selection, text string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	o := &fakeOwner{kind: "chromium", text: text}
	if text == "" {
		o.kind = "image"
	}
	x.owners[selection] = o
}

func (x *fakeX) owner(selection string) *fakeOwner {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.owners[selection]
}

// The clipboard box reads the X clipboard's text with xclip (PROJ-194).
func TestClipboardRead(t *testing.T) {
	read := func(v *visual) (int, string) {
		rec := serveVisual(v, http.MethodGet, "/input/clipboard", "")
		if rec.Code == http.StatusOK && !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("content type %s", rec.Header().Get("Content-Type"))
		}
		return rec.Code, rec.Body.String()
	}
	for name, c := range map[string]struct {
		text   string // Chromium's; "" for nothing
		image  bool
		code   int
		answer string
	}{
		"text":            {text: "héllo 123\nsecond line", code: http.StatusOK, answer: "héllo 123\nsecond line"},
		"nothing held":    {code: http.StatusOK, answer: ""},
		"an image":        {image: true, code: http.StatusUnprocessableEntity},
		"not UTF-8":       {text: "\xff\xfea", code: http.StatusUnprocessableEntity},
		"over the cap":    {text: strings.Repeat("a", maxClipboard+1), code: http.StatusRequestEntityTooLarge},
		"exactly the cap": {text: strings.Repeat("a", maxClipboard), code: http.StatusOK, answer: strings.Repeat("a", maxClipboard)},
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := newTestVisual(false)
			x := newFakeX(v)
			if c.image {
				x.page(selClipboard, "")
			} else if c.text != "" {
				x.page(selClipboard, c.text)
			}
			code, body := read(v)
			if code != c.code {
				t.Fatalf("%d %.60s", code, body)
			}
			if code == http.StatusOK && body != c.answer {
				t.Errorf("answer %.60q", body)
			}
			if code != http.StatusOK && strings.Contains(body, "aaaa") {
				t.Errorf("the error quotes the text: %.40s", body)
			}
		})
	}
	// xclip failing for another reason (no display): an error, not empty.
	v, _ := newTestVisual(false)
	x := newFakeX(v)
	x.readErr = errors.New("xclip: Error: Can't open display: :99")
	if code, body := read(v); code != http.StatusInternalServerError || !strings.Contains(body, "display") {
		t.Errorf("no display: %d %s", code, body)
	}
	// A clipboard owner that never answers: a timeout, not "empty".
	shortWait(t)
	hung, _ := newTestVisual(false)
	hung.clip.read = func(ctx context.Context, _, _ string) ([]byte, bool, error) {
		<-ctx.Done()
		return nil, false, errors.New("xclip: killed by killed")
	}
	if code, body := read(hung); code != http.StatusGatewayTimeout {
		t.Errorf("a hung owner: %d %s", code, body)
	}
}

// A set holds the text on the clipboard, until a second set replaces it;
// it answers once the text reads back (PROJ-194).
func TestClipboardSet(t *testing.T) {
	v, calls := newTestVisual(false)
	x := newFakeX(v)
	if rec := serveVisual(v, http.MethodPost, "/input/clipboard", `{"text":"Rīga ūdens 42\r\nnaïve café 😀"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(x.holders) != 1 || x.holders[0].text != "Rīga ūdens 42\nnaïve café 😀" || x.holders[0].released || x.holders[0].selection != selClipboard || x.holders[0].target != "" {
		t.Fatalf("held %+v", x.holders)
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
	if len(x.holders) != 2 || !x.holders[0].released || x.holders[1].released || x.holders[1].text != "second" {
		t.Errorf("after a second set: %+v %+v", *x.holders[0], *x.holders[1])
	}
	// The page copies: its xclip has gone by itself; a later set or
	// emptying still lets go of it once, harmlessly.
	x.page(selClipboard, "from the page")
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
	sx := newFakeX(stuck)
	sx.stuck = true
	if rec := serveVisual(stuck, http.MethodPost, "/input/clipboard", `{"text":"x"}`); rec.Code != http.StatusInternalServerError || !sx.holders[0].released || stuck.clip.release != nil {
		t.Errorf("a clipboard that didn't take it: %d, %+v", rec.Code, sx.holders)
	}
}

// A paste sets the clipboard, then presses Ctrl+V; the text stays held
// for the rest of the takeover (PROJ-194).
func TestClipboardPaste(t *testing.T) {
	v, calls := newTestVisual(false)
	x := newFakeX(v)
	if rec := serveVisual(v, http.MethodPost, "/input/paste", `{"text":"héllo"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(x.holders) != 1 || x.holders[0].text != "héllo" || x.holders[0].released {
		t.Errorf("held %+v", x.holders)
	}
	if len(*calls) != 1 || (*calls)[0].name != "xdotool" || strings.Join((*calls)[0].args, " ") != "key --clearmodifiers ctrl+v" {
		t.Errorf("ran %v", *calls)
	}
	// Ctrl+V cut short: Ctrl is let go of.
	failing, fcalls := newTestVisual(true)
	newFakeX(failing)
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

// Emptying lets go of the text sent, and takes both selections, CLIPBOARD
// and PRIMARY, from whoever holds them, the page included: an xclip that
// offers no text, seen to own the selection before it ends, so nothing
// is left to paste (PROJ-194).
func TestClipboardEmpty(t *testing.T) {
	v, _ := newTestVisual(false)
	x := newFakeX(v)
	empty := func(what string) {
		t.Helper()
		before := len(x.holders)
		if rec := serveVisual(v, http.MethodDelete, "/input/clipboard", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: empty: %d %s", what, rec.Code, rec.Body)
		}
		for _, sel := range []string{selClipboard, selPrimary} {
			if o := x.owner(sel); o != nil {
				t.Errorf("%s: %s still owned by %+v", what, sel, *o)
			}
		}
		if rec := serveVisual(v, http.MethodGet, "/input/clipboard", ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Errorf("%s: after emptying: %d %q", what, rec.Code, rec.Body)
		}
		var took []string
		for i, h := range x.holders {
			if !h.released {
				t.Errorf("%s: holder %d (%+v) still held", what, i, *h)
			}
			if i >= before {
				if h.target != emptyTarget || h.text != "" {
					t.Errorf("%s: emptied with %+v", what, *h)
				}
				took = append(took, h.selection)
			}
		}
		if strings.Join(took, " ") != "clipboard primary" {
			t.Errorf("%s: took %v", what, took)
		}
	}
	// The text the person sent.
	serveVisual(v, http.MethodPost, "/input/clipboard", `{"text":"sent"}`)
	empty("sent")
	// What the page copied, and what is selected in it: Chromium's own.
	x.page(selClipboard, "copied in the page")
	x.page(selPrimary, "selected in the page")
	empty("the page's")
	// An image copied: no text to read even before, so "reads empty" would
	// prove nothing; the xclip's own target does.
	x.page(selClipboard, "")
	empty("an image")
	// Nothing at all.
	empty("nothing")

	// An xclip that never takes the selection: an error, without the text,
	// and it is let go of.
	shortWait(t)
	stuck, _ := newTestVisual(false)
	sx := newFakeX(stuck)
	sx.page(selClipboard, "a secret")
	sx.stuck = true
	if rec := serveVisual(stuck, http.MethodDelete, "/input/clipboard", ""); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") ||
		!strings.Contains(rec.Body.String(), "clipboard: couldn't take the selection within") {
		t.Errorf("an emptying that failed: %d %s", rec.Code, rec.Body)
	}
	for _, h := range sx.holders {
		if !h.released {
			t.Errorf("left %+v", *h)
		}
	}
	// No display: an error.
	nd, _ := newTestVisual(false)
	ndx := newFakeX(nd)
	ndx.readErr = errors.New("xclip: Error: Can't open display: :99")
	if rec := serveVisual(nd, http.MethodDelete, "/input/clipboard", ""); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "display") {
		t.Errorf("no display: %d %s", rec.Code, rec.Body)
	}
}

func TestHasTarget(t *testing.T) {
	if !hasTarget([]byte("TARGETS\nBROWSERPIER_EMPTIED\n"), emptyTarget) || hasTarget([]byte("TARGETS\nBROWSERPIER_EMPTIED_NOT\n"), emptyTarget) || hasTarget(nil, emptyTarget) {
		t.Error("hasTarget")
	}
}

// The page taking CLIPBOARD back just after the emptying xclip took it (a
// copy landing late on a busy page): the emptying takes it again, a few
// times at most, and lets go of every xclip it started (PROJ-194).
func TestClipboardEmptyRetakes(t *testing.T) {
	v, _ := newTestVisual(false)
	x := newFakeX(v)
	x.page(selClipboard, "copied")
	x.steal = 1
	if rec := serveVisual(v, http.MethodDelete, "/input/clipboard", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	var took []string
	for _, h := range x.holders {
		took = append(took, h.selection)
		if !h.released {
			t.Errorf("left %+v", *h)
		}
	}
	if strings.Join(took, " ") != "clipboard clipboard primary" {
		t.Errorf("took %v", took)
	}
	if o := x.owner(selClipboard); o != nil {
		t.Errorf("still owned by %+v", *o)
	}

	// A page that keeps taking it back: refused, saying so, after
	// emptyTakes tries, and well within the time it has.
	v, _ = newTestVisual(false)
	x = newFakeX(v)
	x.page(selClipboard, "copied")
	x.steal = 100
	start := time.Now()
	rec := serveVisual(v, http.MethodDelete, "/input/clipboard", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "clipboard: the page took the selection back 3 times") || strings.Contains(rec.Body.String(), "copied") {
		t.Errorf("a page that keeps it: %d %s", rec.Code, rec.Body)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v", took)
	}
	n := 0
	for _, h := range x.holders {
		if h.selection == selClipboard {
			n++
		}
		if !h.released {
			t.Errorf("left %+v", *h)
		}
	}
	if n != emptyTakes {
		t.Errorf("took CLIPBOARD %d times", n)
	}
}
