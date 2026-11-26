package main

import (
	"bytes"
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
// and empties both selections before agents get the browser back:
//
//	GET    /input/clipboard   its text (text/plain), "" when nothing holds it
//	POST   /input/clipboard   {"text"}: hold the text there until replaced or emptied
//	POST   /input/paste       {"text"}: the same, then Ctrl+V
//	DELETE /input/clipboard   empty CLIPBOARD and PRIMARY, whoever holds them (Chromium too)
//
// Nothing here logs the text, nor puts it in an error.

// maxClipboard caps what a read answers: JSON-escaped, up to six times as
// long, it still fits one data channel message (256 KiB in Chrome).
const maxClipboard = 32 << 10

// maxClipboardSet caps the text the person sends, in characters.
const maxClipboardSet = 10000

// clipboardWait bounds a read, and the wait for a set to take hold: a
// clipboard owner that doesn't answer (a hung page) doesn't hold the live
// view up. Tests shorten it.
var clipboardWait = 2 * time.Second

// emptyWait bounds the emptying of one selection, and emptyRead each look
// at it then. Both selections' worst case, 3 s, stays well inside the 5 s
// the orchestrator gives the whole call (sessions.Manager.control), so a
// slow emptying answers with its own reason, not the caller's timeout.
// emptyTakes is how many times one emptying takes its selection again
// from a page that keeps taking it back. Tests shorten the waits.
var (
	emptyWait  = 1500 * time.Millisecond
	emptyRead  = 500 * time.Millisecond
	emptyTakes = 3
)

// The X selections: CLIPBOARD (Ctrl+C, Ctrl+V) and PRIMARY (what is
// selected, pasted with the middle button).
const (
	selClipboard = "clipboard"
	selPrimary   = "primary"
)

// emptyTarget is the only target an emptying xclip offers. It holds no
// text, so nothing pastes from it, and seeing it among the selection's
// targets proves that xclip, not the page, owns the selection.
const emptyTarget = "BROWSERPIER_EMPTIED"

// clipState is the xclip that holds the text the person sent, and how to
// reach X's selections.
type clipState struct {
	mu sync.Mutex
	// release lets go of the text held, nil when nothing is.
	release func()
	// hold runs xclip owning selection, offering text as target (""
	// for xclip's own, UTF8_STRING), until release is called or another
	// client (Chromium copying) takes the selection, which closes gone;
	// tests replace it.
	hold func(selection, target, text string) (release func(), gone <-chan struct{}, err error)
	// read reads selection as target. ok is false, with no error, when
	// the selection has nothing as that target: no owner, or an owner
	// without it. Tests replace it.
	read func(ctx context.Context, selection, target string) (out []byte, ok bool, err error)
}

func (v *visual) registerClipboard(mux *http.ServeMux) {
	mux.HandleFunc("GET /input/clipboard", v.readClipboard)
	mux.HandleFunc("POST /input/clipboard", v.setClipboard)
	mux.HandleFunc("POST /input/paste", v.paste)
	mux.HandleFunc("DELETE /input/clipboard", v.emptyClipboard)
}

// readClipboardText reads the clipboard's text, within clipboardWait.
// Empty when nothing holds the clipboard; an error when what holds it
// isn't text (an image copied, say).
func (v *visual) readClipboardText(ctx context.Context) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, clipboardWait)
	defer cancel()
	timedOut := func(err error) (int, error) {
		if ctx.Err() != nil {
			return http.StatusGatewayTimeout, errors.New("the browser's clipboard didn't answer")
		}
		return http.StatusInternalServerError, fmt.Errorf("couldn't read the clipboard: %w", err)
	}
	out, ok, err := v.clip.read(ctx, selClipboard, "UTF8_STRING")
	if err != nil {
		code, err := timedOut(err)
		return nil, code, err
	}
	if !ok {
		// No text: nothing holds the clipboard, or what does isn't text.
		targets, held, err := v.clip.read(ctx, selClipboard, "TARGETS")
		switch {
		case err != nil:
			code, err := timedOut(err)
			return nil, code, err
		case held && !hasTarget(targets, emptyTarget):
			return nil, http.StatusUnprocessableEntity, errors.New("the clipboard holds something that isn't text")
		}
		return nil, http.StatusOK, nil
	}
	switch {
	case len(out) > maxClipboard:
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("the clipboard holds more than %d kB of text", maxClipboard>>10)
	case !utf8.Valid(out):
		return nil, http.StatusUnprocessableEntity, errors.New("the clipboard holds something that isn't UTF-8 text")
	}
	return out, http.StatusOK, nil
}

// hasTarget reports whether a TARGETS answer, one name a line, names
// target.
func hasTarget(targets []byte, target string) bool {
	for line := range strings.SplitSeq(string(targets), "\n") {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}

// readClipboard answers the clipboard's text, for the box's Refresh: read
// only then, never polled, so a page can't put text in the person's
// clipboard on its own.
func (v *visual) readClipboard(w http.ResponseWriter, r *http.Request) {
	gen, person, err := v.fence.takeover(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	done, ok := v.fence.admit(gen, person)
	if !ok {
		refuseFenced(w)
		return
	}
	defer done()
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
	gen, person, err := v.fence.takeover(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text, ok := clipboardText(w, r)
	if !ok {
		return
	}
	// Right before xclip starts: a takeover that ended while this waited
	// touches nothing, not even the text it held before.
	done, ok := v.fence.admit(gen, person)
	if !ok {
		refuseFenced(w)
		return
	}
	defer done()
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
	gen, person, err := v.fence.takeover(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text, ok := clipboardText(w, r)
	if !ok {
		return
	}
	done, ok := v.fence.admit(gen, person)
	if !ok {
		refuseFenced(w)
		return
	}
	defer done()
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
	release, _, err := v.clip.hold(selClipboard, "", text)
	if err != nil {
		return fmt.Errorf("couldn't set the clipboard: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, clipboardWait)
	defer cancel()
	if err := v.await(ctx, func() bool {
		out, ok, err := v.clip.read(ctx, selClipboard, "UTF8_STRING")
		return err == nil && ok && string(out) == text
	}); err != nil {
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

// await polls done until it says so, or ctx ends.
func (*visual) await(ctx context.Context, done func() bool) error {
	for !done() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// emptyClipboard empties both selections, when the person's control ends:
// the text they sent, what the page copied (Chromium's CLIPBOARD) and
// what is selected in it (Chromium's PRIMARY, which a middle-click
// pastes). Each is taken by an xclip that offers only emptyTarget, which
// proves it owns the selection, and which then ends, leaving no owner, so
// a later paste of either pastes nothing.
//
// It names, in fenceHeader, the takeover whose end it is for: if a later
// takeover has begun meanwhile (this was held up past its time), the
// clipboard is that one's, and is left alone (fence.go's cleanup).
func (v *visual) emptyClipboard(w http.ResponseWriter, r *http.Request) {
	n, _, err := fenceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var failed []string
	_, _ = v.fence.cleanup(n, func(stale bool) error {
		if stale {
			return nil
		}
		v.clip.mu.Lock()
		defer v.clip.mu.Unlock()
		v.letGoLocked()
		for _, sel := range []string{selClipboard, selPrimary} {
			if err := v.emptySelection(r.Context(), sel); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", sel, err))
			}
		}
		return nil
	})
	if len(failed) > 0 {
		http.Error(w, "couldn't empty the clipboard: "+strings.Join(failed, "; "), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// emptySelection takes selection with an empty xclip, waits until it is
// seen to own it, then ends it. If the page takes the selection back
// meanwhile (a copy landing late, on a busy page), which ends that xclip,
// it takes it again, emptyTakes times at most.
func (v *visual) emptySelection(ctx context.Context, selection string) error {
	ctx, cancel := context.WithTimeout(ctx, emptyWait)
	defer cancel()
	var release func()
	var gone <-chan struct{}
	defer func() {
		if release != nil {
			release() // the owner goes, and the selection has none
		}
	}()
	for takes := 0; ; {
		if release == nil {
			if takes == emptyTakes {
				return fmt.Errorf("the page took the selection back %d times", takes)
			}
			var err error
			if release, gone, err = v.clip.hold(selection, emptyTarget, ""); err != nil {
				return err
			}
			takes++
		}
		rctx, rcancel := context.WithTimeout(ctx, emptyRead)
		targets, ok, err := v.clip.read(rctx, selection, "TARGETS")
		timedOut := rctx.Err() != nil
		rcancel()
		switch {
		case err == nil && ok && hasTarget(targets, emptyTarget):
			// Ours: end it, then check it left no owner. A page taking the
			// selection back between that look and the end would own it
			// now, and the xclip it ended can't tell us so.
			release()
			release = nil
			vctx, vcancel := context.WithTimeout(ctx, emptyRead)
			_, owned, verr := v.clip.read(vctx, selection, "TARGETS")
			vtimedOut := vctx.Err() != nil
			vcancel()
			switch {
			case verr == nil && !owned:
				return nil
			case verr != nil && !vtimedOut:
				return verr
			}
			continue // owned again, or unseen: take it again
		case err != nil && !timedOut:
			return err // no display, say: looking again won't help
		}
		select {
		case <-gone: // another client took it, and that xclip ended
			release()
			release = nil
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("couldn't take the selection within %v", emptyWait)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// holdClipboard runs xclip in the foreground owning selection, offering
// text as target. It runs until release kills it, or until another client
// takes the selection (Chromium, when the page is copied from): then it
// exits by itself and the reaper collects it.
func holdClipboard(selection, target, text string) (func(), <-chan struct{}, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	args := []string{"-quiet", "-selection", selection}
	if target != "" {
		args = append(args, "-target", target)
	}
	cmd := exec.Command("xclip", append(args, "-in")...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.Stdin = pr
	exited, err := procs.start(cmd)
	_ = pr.Close()
	if err != nil {
		_ = pw.Close()
		return nil, nil, err
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
		return nil, nil, werr
	}
	return release, gone, nil
}

// readSelection runs xclip -out for selection as target. xclip says
// "target … not available" when the selection has no owner, or one
// without that target: ok is false then. Any other failure (no display,
// say) is an error, with xclip's own words, which never hold the content.
func readSelection(ctx context.Context, selection, target string) ([]byte, bool, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = outR.Close() }()
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outW.Close()
		return nil, false, err
	}
	defer func() { _ = errR.Close() }()
	cmd := exec.Command("xclip", "-selection", selection, "-out", "-target", target)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.Stdout, cmd.Stderr = outW, errW
	exited, err := procs.start(cmd)
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		return nil, false, err
	}
	defer context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })()
	var stderr bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderr, io.LimitReader(errR, 1024))
		_, _ = io.Copy(io.Discard, errR)
		close(stderrDone)
	}()
	out, readErr := io.ReadAll(io.LimitReader(outR, maxClipboard+1))
	if len(out) > maxClipboard {
		// Over the cap: the caller refuses it, by its length.
		_ = cmd.Process.Kill()
	}
	ws := <-exited
	<-stderrDone
	switch {
	case len(out) > maxClipboard:
		return out, true, nil
	case ws.Exited() && ws.ExitStatus() == 0:
		return out, true, readErr
	case ws.Exited() && ws.ExitStatus() == 1 && strings.Contains(stderr.String(), "not available"):
		return nil, false, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = describe(ws)
	}
	return nil, false, fmt.Errorf("xclip: %s", msg)
}
