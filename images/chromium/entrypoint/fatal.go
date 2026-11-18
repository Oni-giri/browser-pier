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
// reason is the browser process's own FATAL line, or else a child's from
// just before the browser ended (PROJ-186).
type fatalLine struct {
	w      *os.File // the pipe's write end, Chromium's stdout and stderr
	copied chan struct{}
	mu     sync.Mutex
	buf    []byte       // the line being written
	pid    int          // the browser process's, once started
	fatals []fatalEntry // the last few FATAL lines, oldest first
	error  string       // ERROR
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

// fatalEntry is one FATAL line: the pid in its prefix (0 if none), when we
// read it, and its message.
type fatalEntry struct {
	pid int
	at  time.Time
	msg string
}

// keptFatals is how many FATAL lines are kept. The browser's own is the
// last it writes, and only children dying with it can write after it.
const keptFatals = 16

// childFatalWindow is how long before the browser ended a child's FATAL
// line can still explain it: a zygote that can't set up the sandbox
// ("Check failed: ... Permission denied") takes the browser down within
// milliseconds.
const childFatalWindow = 3 * time.Second

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
func (f *fatalLine) started(pid int) {
	f.mu.Lock()
	f.pid = pid
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
		if m := chromiumLine(line, "FATAL"); m != "" {
			if len(f.fatals) == keptFatals {
				f.fatals = append(f.fatals[:0], f.fatals[1:]...)
			}
			f.fatals = append(f.fatals, fatalEntry{pid: linePID(line), at: time.Now(), msg: m})
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

// reason is ": " and the FATAL line that can explain why the browser
// ended (at ended): the browser process's last one, or else the last one
// written within childFatalWindow before ended. "" if there was none.
// During startup (startup true) the last ERROR line stands in for a missing
// FATAL one: later on, ERROR lines are mostly ones Chromium recovered from.
// It waits briefly for the output still in the pipe.
func (f *fatalLine) reason(startup bool, ended time.Time) string {
	select {
	case <-f.copied:
	case <-time.After(time.Second):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pid != 0 {
		for i := len(f.fatals) - 1; i >= 0; i-- {
			if f.fatals[i].pid == f.pid {
				return ": " + f.fatals[i].msg
			}
		}
	}
	if n := len(f.fatals); n > 0 && ended.Sub(f.fatals[n-1].at) <= childFatalWindow {
		return ": " + f.fatals[n-1].msg
	}
	if startup && f.error != "" {
		return ": " + f.error
	}
	return ""
}

// linePID returns the pid in a Chromium log line's prefix ("[pid:tid:..."),
// or 0 if it has none.
func linePID(line string) int {
	rest, ok := strings.CutPrefix(line, "[")
	if !ok {
		return 0
	}
	digits, _, ok := strings.Cut(rest, ":")
	if !ok {
		return 0
	}
	pid, err := strconv.Atoi(digits)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
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
