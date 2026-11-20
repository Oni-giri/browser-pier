package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fatalLine passes Chromium's output through to ours and remembers its last
// FATAL lines (and its last ERROR line, for want of one), so a browser that
// dies can say why ("Check failed: ... Permission denied", "No usable
// sandbox!") and not only how ("killed by aborted"). Only the container's
// last log line outlives it.
//
// Not every FATAL line ended the browser: a child process's ends only that
// child, and Chromium may recover (a GPU process that can't start its
// sandbox exits, and Chromium runs another without GL, PROJ-165). So the
// reason is a FATAL line from just before the browser ended: the browser
// process's own if there is one, or else a child's (PROJ-186).
//
// The pid in a line's prefix isn't proof of where it came from: every
// child writes to the same pipe, and a compromised renderer can write any
// prefix. The window keeps such a line from standing for a later exit.
type fatalLine struct {
	w       *os.File // the pipe's write end, Chromium's stdout and stderr
	copied  chan struct{}
	mu      sync.Mutex
	buf     []byte       // the line being written
	pid     int          // the browser process's, once started
	browser fatalEntry   // its last FATAL line, if any
	others  []fatalEntry // the last few FATAL lines of other pids, oldest first
	error   string       // ERROR
	// warn, if set, is told once when a GPU process exits because it
	// couldn't start its sandbox (gpuSandboxFailed, then gpuNotMonoThreaded).
	warn       func(msg string)
	gpuWaiting bool // gpuSandboxFailed was the last sandbox line
	gpuWarned  bool
}

// gpuSandboxFailed is how Chromium says a GPU process found other threads
// when it went to start its seccomp sandbox. With
// --gpu-sandbox-failures-fatal (PROJ-165) it's an ERROR, and Chromium then
// waits for the process to become single-threaded: if it does, the sandbox
// starts and nothing is lost; if not, the FATAL gpuNotMonoThreaded follows
// and the process exits. That FATAL line doesn't name the process; this
// one does.
const (
	gpuSandboxFailed   = "InitializeSandbox() called with multiple threads in process gpu-process"
	gpuNotMonoThreaded = "Current process is not mono-threaded"
)

// gpuSandboxWarning is what the admin reads in the browser's log when a
// GPU process did exit for it: Chromium restarts it, and if that keeps
// failing, runs it without GL, so pages get no WebGL and can't say why.
const gpuSandboxWarning = "a GPU process exited because it couldn't start its sandbox; Chromium restarts it, and if that keeps failing, pages get no WebGL"

// fatalEntry is one FATAL line: the pid in its prefix, when we read it,
// and its message. A zero at means none.
type fatalEntry struct {
	pid int
	at  time.Time
	msg string
}

// keptFatals is how many FATAL lines of other processes are kept; only
// the latest within fatalWindow is used.
const keptFatals = 16

// fatalWindow is how long before the browser ended a FATAL line can still
// explain it. A FATAL line aborts its process at once, and a zygote that
// can't set up the sandbox ("Check failed: ... Permission denied") takes
// the browser down within milliseconds.
const fatalWindow = 3 * time.Second

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
// Chromium and its children have exited, and records the browser process's
// pid (0 if it didn't start): Chromium writes it in each line's prefix.
// The browser is never pid 1 (the entrypoint is), the pid a zygote logs
// from its own PID namespace; other children log their pid in ours.
func (f *fatalLine) started(pid int) {
	f.mu.Lock()
	f.pid = pid
	// Lines read before we knew the pid.
	kept := f.others[:0]
	for _, e := range f.others {
		if pid != 0 && e.pid == pid {
			f.browser = e
		} else {
			kept = append(kept, e)
		}
	}
	f.others = kept
	f.mu.Unlock()
	_ = f.w.Close()
}

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
		switch {
		case strings.Contains(line, gpuSandboxFailed):
			f.gpuWaiting = true
		case strings.Contains(line, "InitializeSandbox() called with multiple threads"):
			f.gpuWaiting = false // another process type's
		case f.gpuWaiting && strings.Contains(line, gpuNotMonoThreaded) && chromiumLine(line, "FATAL") != "":
			f.gpuWaiting = false
			if !f.gpuWarned && f.warn != nil {
				f.gpuWarned = true
				f.warn(gpuSandboxWarning)
			}
		}
		switch pid, level, m := parseChromiumLine(line); level {
		case "FATAL":
			e := fatalEntry{pid: pid, at: time.Now(), msg: m}
			if f.pid != 0 && pid == f.pid {
				f.browser = e
				break
			}
			if len(f.others) == keptFatals {
				f.others = append(f.others[:0], f.others[1:]...)
			}
			f.others = append(f.others, e)
		case "ERROR":
			f.error = m
		}
		f.buf = f.buf[i+1:]
	}
	if len(f.buf) > 64<<10 {
		f.buf = f.buf[:0] // a runaway line can't be a FATAL one worth keeping
	}
	return len(p), nil
}

// reason is ": " and the FATAL line that can explain why the browser
// ended, or "" if there was none. ended is when it exited (exited true),
// or when startup was given up on, with the browser still running (a
// timeout, or a stop): then lines read later, as we close it, don't count.
// Only lines read within fatalWindow before ended count, the browser
// process's own first. During startup (startup true) the last ERROR line
// stands in for a missing FATAL one: later on, ERROR lines are mostly ones
// Chromium recovered from. It waits briefly for the output still in the
// pipe.
func (f *fatalLine) reason(startup bool, ended time.Time, exited bool) string {
	select {
	case <-f.copied:
	case <-time.After(time.Second):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	explains := func(e fatalEntry) bool {
		return !e.at.IsZero() && ended.Sub(e.at) <= fatalWindow && (exited || !e.at.After(ended))
	}
	if explains(f.browser) {
		return ": " + f.browser.msg
	}
	for i := len(f.others) - 1; i >= 0; i-- {
		if explains(f.others[i]) {
			return ": " + f.others[i].msg
		}
	}
	if startup && f.error != "" {
		return ": " + f.error
	}
	return ""
}

// parseChromiumLine splits a Chromium log line,
// "[pid:tid:MMDD/HHMMSS.micros:LEVEL:sandbox/linux/services/credentials.cc:137] message",
// into its pid, its level, and its message then where it came from. level
// is "" for any other line: the prefix is checked field by field, so a
// level named in a message doesn't count.
func parseChromiumLine(line string) (pid int, level, msg string) {
	s, ok := strings.CutPrefix(line, "[")
	if !ok {
		return 0, "", ""
	}
	fields := strings.SplitN(s, ":", 5)
	if len(fields) < 5 || !allOf(fields[0], "0123456789") || !allOf(fields[1], "0123456789") ||
		!allOf(fields[2], "0123456789/.") || !allOf(fields[3], "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
		return 0, "", ""
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", ""
	}
	where, m, ok := strings.Cut(fields[4], "] ")
	if !ok {
		return pid, fields[3], truncate(strings.TrimSpace(fields[4]))
	}
	return pid, fields[3], truncate(strings.TrimSpace(m) + " (" + where + ")")
}

// chromiumLine returns the message of a Chromium log line at level, then
// where it came from, or "" for any other line.
func chromiumLine(line, level string) string {
	if _, l, m := parseChromiumLine(line); l == level {
		return m
	}
	return ""
}

// allOf reports whether s is non-empty and made only of chars.
func allOf(s, chars string) bool {
	return s != "" && strings.Trim(s, chars) == ""
}

// truncate keeps the reason short enough to survive slog's quoting within
// the orchestrator's log line cap.
func truncate(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
