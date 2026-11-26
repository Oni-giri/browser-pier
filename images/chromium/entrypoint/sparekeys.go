package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Oni-giri/browser-pier/internal/keyplan"
)

// spareKeysArg runs the spare-key check instead of the browser: it starts
// an Xvfb of its own, counts the keycodes typing can bind characters to
// (keyboardMap.spare), prints the count, and fails if it is under
// keyplan.SpareKeys, which the orchestrator's time estimates assume
// (PROJ-195). The image's build runs it, so a Debian update whose layout
// data leaves fewer fails the build, not the first long type.
const spareKeysArg = "spare-keys"

// spareKeysDisplay is the check's own display, apart from the browser's.
const spareKeysDisplay = ":98"

func runSpareKeys(out io.Writer) int {
	n, err := countSpareKeys(context.Background(), spareKeysDisplay)
	if err != nil {
		_, _ = fmt.Fprintf(out, "spare keys: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "spare keys: %d (at least %d needed)\n", n, keyplan.SpareKeys)
	if n < keyplan.SpareKeys {
		return 1
	}
	return 0
}

// countSpareKeys starts Xvfb on display and counts its spare keys.
func countSpareKeys(ctx context.Context, disp string) (int, error) {
	x, err := spawn(exec.Command("Xvfb", disp, "-screen", "0", "640x480x24", "-nolisten", "tcp"))
	if err != nil {
		return 0, fmt.Errorf("start Xvfb: %w", err)
	}
	defer x.stop(syscall.SIGTERM, 5*time.Second)
	sock := "/tmp/.X11-unix/X" + disp[1:]
	if err := waitFor(ctx, x, 10*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}); err != nil {
		return 0, fmt.Errorf("xvfb: %w", err)
	}
	conn, err := dialKeyboardAt(ctx, sock)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	m, err := readKeyboardMap(conn)
	if err != nil {
		return 0, err
	}
	return len(m.spare()), nil
}
