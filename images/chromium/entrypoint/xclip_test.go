package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeXclip puts an xclip on PATH that acts as $FAKE_XCLIP says, so the
// real readSelection and holdClipboard run against it: how xclip's exit
// status and words are read is tested without X.
func fakeXclip(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# Called as: xclip -selection SEL -out -target TARGET
#        or: xclip -quiet -selection SEL [-target TARGET] -in
in=; target=; sel=
while [ $# -gt 0 ]; do
	case "$1" in
	-in) in=1 ;;
	-target) shift; target=$1 ;;
	-selection) shift; sel=$1 ;;
	esac
	shift
done
na() { echo "Error: target $target not available" >&2; exit 1; }
case "$FAKE_XCLIP" in
none) [ -n "$in" ] && exit 0; na ;;
text) printf 'h\303\251llo\nworld' ;;
image) [ "$target" = TARGETS ] && { printf 'TARGETS\nimage/png\n'; exit 0; }; na ;;
latin1) printf '\377\376a' ;;
big) exec head -c 40000 /dev/zero ;; # one process, as xclip is
display) echo "Error: Can't open display: :99" >&2; exit 1 ;;
crash) exit 2 ;;
owner)
	# An emptying xclip that takes its selection and holds it until
	# killed; the selection is owned only while it runs.
	f="$FAKE_XCLIP_DIR/holder-$sel"
	[ -n "$in" ] && { cat >/dev/null; echo $$ >"$f"; exec sleep 30; }
	[ -f "$f" ] && kill -0 "$(cat "$f")" 2>/dev/null || na
	[ "$target" = TARGETS ] && { printf 'TARGETS\nBROWSERPIER_EMPTIED\n'; exit 0; }
	exit 0 ;;
stolen)
	# The page takes the selection back at once: xclip ends by itself.
	[ -n "$in" ] && { cat >/dev/null; exit 0; }
	[ "$target" = TARGETS ] && { printf 'TARGETS\nUTF8_STRING\n'; exit 0; }
	printf 'copied late' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "xclip"), []byte(script), 0o700); err != nil { //nolint:gosec // a test's own script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_XCLIP", mode)
	t.Setenv("FAKE_XCLIP_DIR", dir)
}

// readSelection reads xclip's answer: text, nothing ("not available"), or
// an error, which is never taken for nothing (PROJ-194).
func TestReadSelection(t *testing.T) {
	for _, c := range []struct {
		mode, target string
		ok           bool
		out, err     string
	}{
		{"none", "UTF8_STRING", false, "", ""},
		{"none", "TARGETS", false, "", ""},
		{"text", "UTF8_STRING", true, "héllo\nworld", ""},
		{"image", "UTF8_STRING", false, "", ""},
		{"image", "TARGETS", true, "TARGETS\nimage/png\n", ""},
		{"display", "UTF8_STRING", false, "", "xclip: Error: Can't open display: :99"},
		{"crash", "UTF8_STRING", false, "", "xclip: exit status 2"},
	} {
		t.Run(c.mode+" "+c.target, func(t *testing.T) {
			fakeXclip(t, c.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, ok, err := readSelection(ctx, selClipboard, c.target)
			if string(out) != c.out || ok != c.ok || (err == nil) != (c.err == "") || (err != nil && err.Error() != c.err) {
				t.Errorf("= %q, %t, %v", out, ok, err)
			}
		})
	}
}

// The clipboard box's read, through the real readSelection.
func TestClipboardReadThroughXclip(t *testing.T) {
	for _, c := range []struct {
		mode   string
		code   int
		answer string
	}{
		{"none", http.StatusOK, ""},
		{"text", http.StatusOK, "héllo\nworld"},
		{"image", http.StatusUnprocessableEntity, "the clipboard holds something that isn't text\n"},
		{"latin1", http.StatusUnprocessableEntity, "the clipboard holds something that isn't UTF-8 text\n"},
		{"big", http.StatusRequestEntityTooLarge, "the clipboard holds more than 32 kB of text\n"},
		{"display", http.StatusInternalServerError, "couldn't read the clipboard: xclip: Error: Can't open display: :99\n"},
		{"crash", http.StatusInternalServerError, "couldn't read the clipboard: xclip: exit status 2\n"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			fakeXclip(t, c.mode)
			v := newVisual(1280, 800)
			rec := serveVisual(v, http.MethodGet, "/input/clipboard", "")
			if rec.Code != c.code || rec.Body.String() != c.answer {
				t.Errorf("%d %q", rec.Code, rec.Body)
			}
		})
	}
}

// Emptying through the real holdClipboard and readSelection: the xclip is
// seen to own each selection, then ended; a page that keeps taking the
// selection back is refused, saying so; no display is said too.
func TestClipboardEmptyThroughXclip(t *testing.T) {
	for _, c := range []struct {
		mode string
		code int
		says string
	}{
		{"owner", http.StatusNoContent, ""},
		{"stolen", http.StatusInternalServerError, "clipboard: the page took the selection back 3 times; primary: the page took the selection back 3 times"},
		{"display", http.StatusInternalServerError, "clipboard: xclip: Error: Can't open display: :99"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			fakeXclip(t, c.mode)
			v := newVisual(1280, 800)
			start := time.Now()
			rec := serveVisual(v, http.MethodDelete, "/input/clipboard", "")
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.says) {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
			if took := time.Since(start); took > 2*emptyWait+time.Second {
				t.Errorf("took %v", took)
			}
			// Every xclip that held a selection was ended: a holder left
			// running would still sleep, and own its selection.
			for _, sel := range []string{selClipboard, selPrimary} {
				pid, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_XCLIP_DIR"), "holder-"+sel))
				if err != nil {
					continue
				}
				if err := exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run(); err == nil {
					t.Errorf("the %s holder still runs", sel)
				}
			}
		})
	}
}
