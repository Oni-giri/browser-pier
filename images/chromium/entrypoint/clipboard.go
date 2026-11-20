package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The live view's clipboard box (PROJ-194) reads and sets the browser's
// clipboard, X's CLIPBOARD selection, with xclip: no CDP, as xdotool's
// input. The orchestrator calls these only for the person with control,
// and empties the clipboard before agents get the browser back:
//
//	GET    /input/clipboard   its text (text/plain), "" when nothing holds text
//	POST   /input/clipboard   {"text"}: hold the text there until replaced or emptied
//	POST   /input/paste       {"text"}: the same, then Ctrl+V
//	DELETE /input/clipboard   empty it, whoever holds it (Chromium too)
//
// Nothing here logs the text, nor puts it in an error.

// maxClipboard caps what a read answers: JSON-escaped, up to six times as
// long, it still fits one data channel message (256 KiB in Chrome).
const maxClipboard = 32 << 10

// maxClipboardSet caps the text the person sends, in characters.
const maxClipboardSet = 10000

// clipboardWait bounds a read, and the wait for a set or an emptying to
// read back: a clipboard owner that doesn't answer (a hung page) doesn't
// hold the live view up. Tests shorten it.
var clipboardWait = 2 * time.Second

// clipState is the xclip that holds the text the person sent.
type clipState struct {
	mu sync.Mutex
	// release lets go of the text held, nil when nothing is.
	release func()
	// hold runs xclip with text as the clipboard until release is called,
	// or until another client (Chromium copying) takes the selection;
	// tests replace it.
	hold func(text string) (release func(), err error)
}

func (v *visual) registerClipboard(mux *http.ServeMux) {
	mux.HandleFunc("GET /input/clipboard", v.readClipboard)
	mux.HandleFunc("POST /input/clipboard", v.setClipboard)
	mux.HandleFunc("POST /input/paste", v.paste)
	mux.HandleFunc("DELETE /input/clipboard", v.emptyClipboard)
}

// readClipboardText reads the clipboard's text, within clipboardWait.
// Empty when nothing holds the clipboard, or not as text.
func (v *visual) readClipboardText(ctx context.Context) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, clipboardWait)
	defer cancel()
	out, err := v.output(ctx, "xclip", "-selection", "clipboard", "-out", "-target", "UTF8_STRING")
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, http.StatusGatewayTimeout, errors.New("the browser's clipboard didn't answer")
	case strings.HasSuffix(err.Error(), "exit status 1"):
		out = nil // nothing holds the clipboard, or not as text
	default:
		return nil, http.StatusInternalServerError, fmt.Errorf("couldn't read the clipboard: %w", err)
	}
	switch {
	case len(out) > maxClipboard:
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("the clipboard holds more than %d kB of text", maxClipboard>>10)
	case !utf8.Valid(out):
		return nil, http.StatusUnprocessableEntity, errors.New("the clipboard holds something that isn't UTF-8 text")
	}
	return out, http.StatusOK, nil
}

// readClipboard answers the clipboard's text, for the box's Refresh: read
// only then, never polled, so a page can't put text in the person's
// clipboard on its own.
func (v *visual) readClipboard(w http.ResponseWriter, r *http.Request) {
	out, code, err := v.readClipboardText(r.Context())
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(out)
}

// clipboardText reads a set's text, with line endings made \n, or
// refuses it.
func clipboardText(w http.ResponseWriter, r *http.Request) (string, bool) {
	var t struct{ Text string }
	if !decode(w, r, &t) {
		return "", false
	}
	text := strings.ReplaceAll(strings.ReplaceAll(t.Text, "\r\n", "\n"), "\r", "\n")
	if err := checkText(text, maxClipboardSet); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return text, true
}

func (v *visual) setClipboard(w http.ResponseWriter, r *http.Request) {
	text, ok := clipboardText(w, r)
	if !ok {
		return
	}
	v.clip.mu.Lock()
	defer v.clip.mu.Unlock()
	if err := v.holdText(r.Context(), text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// paste sets the clipboard, then presses Ctrl+V. Pasting, not typing, puts
// the text in exactly: xdotool types a character the keyboard map lacks
// (an accent, sometimes) by mapping it to a spare key on the fly, and
// Chromium misses the change now and then. A key press cut short lets go
// of Ctrl, as an agent's input does (perform).
func (v *visual) paste(w http.ResponseWriter, r *http.Request) {
	text, ok := clipboardText(w, r)
	if !ok {
		return
	}
	v.clip.mu.Lock()
	defer v.clip.mu.Unlock()
	if err := v.holdText(r.Context(), text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v.perform(w, r, func(func() hand) []xcmd {
		return []xcmd{{[]string{"key", "--clearmodifiers", "ctrl+v"}, []string{"keyup", "ctrl+v"}}}
	})
}

// holdText puts text on the clipboard, held until replaced or emptied,
// and waits until it reads back. v.clip.mu must be held.
func (v *visual) holdText(ctx context.Context, text string) error {
	v.letGoLocked()
	release, err := v.clip.hold(text)
	if err != nil {
		return fmt.Errorf("couldn't set the clipboard: %w", err)
	}
	if err := v.awaitClipboard(ctx, text); err != nil {
		release()
		return errors.New("couldn't set the clipboard: it didn't take the text")
	}
	v.clip.release = release
	return nil
}

// letGoLocked ends the xclip holding the person's text, if one is.
// v.clip.mu must be held.
func (v *visual) letGoLocked() {
	if v.clip.release != nil {
		v.clip.release()
		v.clip.release = nil
	}
}

// awaitClipboard waits, within clipboardWait, until the clipboard reads
// text ("" also when nothing holds it).
func (v *visual) awaitClipboard(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, clipboardWait)
	defer cancel()
	for {
		out, err := v.output(ctx, "xclip", "-selection", "clipboard", "-out", "-target", "UTF8_STRING")
		if (err == nil && string(out) == text) || (text == "" && err != nil && strings.HasSuffix(err.Error(), "exit status 1")) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// emptyClipboard empties the clipboard, when the person's control ends:
// it lets go of the text they sent, and of what the page copied, which
// Chromium holds itself. An empty xclip takes the selection from
// whoever holds it and then exits, which leaves it with no owner, so a
// later Ctrl+V pastes nothing.
func (v *visual) emptyClipboard(w http.ResponseWriter, r *http.Request) {
	v.clip.mu.Lock()
	defer v.clip.mu.Unlock()
	v.letGoLocked()
	release, err := v.clip.hold("")
	if err != nil {
		http.Error(w, "couldn't empty the clipboard: "+err.Error(), http.StatusInternalServerError)
		return
	}
	err = v.awaitClipboard(r.Context(), "")
	release()
	if err == nil {
		// With no owner left, nothing reads back.
		err = v.awaitClipboard(r.Context(), "")
	}
	if err != nil {
		http.Error(w, "couldn't empty the clipboard: it kept its text", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// holdClipboard runs xclip in the foreground with text as the clipboard.
// It runs until release kills it, or until another client takes the
// selection (Chromium, when the page is copied from): then it exits by
// itself and the reaper collects it.
func holdClipboard(text string) (func(), error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("xclip", "-quiet", "-selection", "clipboard", "-in")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.Stdin = pr
	exited, err := procs.start(cmd)
	_ = pr.Close()
	if err != nil {
		_ = pw.Close()
		return nil, err
	}
	gone := make(chan struct{})
	go func() {
		<-exited
		close(gone)
	}()
	release := sync.OnceFunc(func() {
		select {
		case <-gone: // it let go already
		default:
			_ = cmd.Process.Kill()
			<-gone
		}
	})
	_, werr := io.WriteString(pw, text)
	_ = pw.Close()
	if werr != nil {
		release()
		return nil, werr
	}
	return release, nil
}
