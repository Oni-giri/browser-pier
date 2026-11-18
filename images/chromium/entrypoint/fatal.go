package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// fatalLine passes Chromium's output through to ours and remembers its last
// FATAL line (and its last ERROR line, for want of one), so a browser that
// dies can say why ("Check failed: ... Permission denied", "No usable
// sandbox!") and not only how ("killed by aborted"). Only the container's
// last log line outlives it.
type fatalLine struct {
	w      *os.File // the pipe's write end, Chromium's stdout and stderr
	copied chan struct{}
	mu     sync.Mutex
	buf    []byte // the line being written
	last   string // FATAL
	error  string // ERROR
	// warn, if set, is told once when the GPU process can't start its
	// sandbox (gpuSandboxFailed).
	warn      func(msg string)
	gpuWarned bool
}

// gpuSandboxFailed is how Chromium says the GPU process can't start its
// seccomp sandbox: in a WARNING without --gpu-sandbox-failures-fatal, in
// an ERROR just before the FATAL "Current process is not mono-threaded!"
// with it (PROJ-165). The FATAL line doesn't name the process; this does.
const gpuSandboxFailed = "InitializeSandbox() called with multiple threads in process gpu-process"

// gpuSandboxWarning is what the admin reads in the browser's log then:
// the browser keeps running, but without WebGL, and the page can't say so.
const gpuSandboxWarning = "the GPU process couldn't start its sandbox, so it exits; Chromium will run it without GL, and pages get no WebGL"

// watchFatal points cmd's output at a pipe read by a fatalLine, which
// tells warn (if not nil) what an admin should know. Call started once cmd
// has started.
func watchFatal(cmd *exec.Cmd, warn func(msg string)) (*fatalLine, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	f := &fatalLine{w: w, copied: make(chan struct{}), warn: warn}
	cmd.Stdout, cmd.Stderr = w, w
	go func() {
		_, _ = io.Copy(io.MultiWriter(os.Stderr, f), r)
		_ = r.Close()
		close(f.copied)
	}()
	return f, nil
}

// started closes our copy of the write end, so the reader sees EOF once
// Chromium and its children have exited.
func (f *fatalLine) started() { _ = f.w.Close() }

func (f *fatalLine) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := string(f.buf[:i])
		if !f.gpuWarned && f.warn != nil && strings.Contains(line, gpuSandboxFailed) {
			f.gpuWarned = true
			f.warn(gpuSandboxWarning)
		}
		if m := chromiumLine(line, "FATAL"); m != "" {
			f.last = m
		} else if m := chromiumLine(line, "ERROR"); m != "" {
			f.error = m
		}
		f.buf = f.buf[i+1:]
	}
	if len(f.buf) > 64<<10 {
		f.buf = f.buf[:0] // a runaway line can't be a FATAL one worth keeping
	}
	return len(p), nil
}

// reason is ": " and the last FATAL line, or "" if there was none. During
// startup (startup true) the last ERROR line stands in for a missing FATAL
// one: later on, ERROR lines are mostly ones Chromium recovered from. It
// waits briefly for the output still in the pipe.
func (f *fatalLine) reason(startup bool) string {
	select {
	case <-f.copied:
	case <-time.After(time.Second):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.last != "":
		return ": " + f.last
	case startup && f.error != "":
		return ": " + f.error
	}
	return ""
}

// chromiumLine returns the message of a Chromium log line at level, then
// where it came from, or "" for any other line. Chromium writes
// "[pid:tid:MMDD/HHMMSS.micros:FATAL:sandbox/linux/services/credentials.cc:137] message".
func chromiumLine(line, level string) string {
	_, rest, ok := strings.Cut(line, ":"+level+":")
	if !ok {
		return ""
	}
	where, msg, ok := strings.Cut(rest, "] ")
	if !ok {
		return truncate(strings.TrimSpace(rest))
	}
	return truncate(strings.TrimSpace(msg) + " (" + where + ")")
}

// truncate keeps the reason short enough to survive slog's quoting within
// the orchestrator's log line cap.
func truncate(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
