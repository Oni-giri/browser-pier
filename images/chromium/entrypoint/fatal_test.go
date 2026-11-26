package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChromiumLine(t *testing.T) {
	for line, want := range map[string]string{
		"[20:20:1103/154246.034517:FATAL:sandbox/linux/services/credentials.cc:137] Check failed: . : Permission denied (13)": "Check failed: . : Permission denied (13) (sandbox/linux/services/credentials.cc:137)",
		"[14:14:1103/151554.172018:ERROR:content/browser/zygote_host/zygote_host_impl_linux.cc:130] No usable sandbox!":       "",
		"time=2026-11-03T15:42:45.881Z level=INFO msg=location":                                                               "",
		"[1:1:0101/000000.000000:FATAL:x.cc(12)]":                                                                             "x.cc(12)]",
	} {
		if got := chromiumLine(line, "FATAL"); got != want {
			t.Errorf("chromiumLine(%q) = %q, want %q", line, got, want)
		}
	}
	if got := chromiumLine("[1:1:0:FATAL:a.cc:1] "+strings.Repeat("é", 400), "FATAL"); len([]rune(got)) != 201 || !utf8.ValidString(got) {
		t.Errorf("truncated to %d runes, valid %t", len([]rune(got)), utf8.ValidString(got))
	}
}

func TestFatalLineKeepsTheLastFatalAcrossWrites(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	for _, chunk := range []string{"[40:40:0:FATAL:a.cc:1] first\n[40:40:0:ERR", "OR:b.cc:2] noise\n[41:41:0:FA", "TAL:c.cc:3] sec", "ond\nunfinished"} {
		_, _ = f.Write([]byte(chunk))
	}
	if got := f.reason(false, time.Now(), true); got != ": second (c.cc:3)" {
		t.Fatalf("reason %q", got)
	}
}

func TestWatchFatalSeesAChildsFatalLine(t *testing.T) {
	cmd := exec.Command("sh", "-c", `echo '[20:20:0:FATAL:sandbox/x.cc:9] Check failed: boom' >&2; exit 3`)
	f, err := watchFatal(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.started(cmd.Process.Pid)
	_ = cmd.Wait()
	if got := f.reason(true, time.Now(), true); got != ": Check failed: boom (sandbox/x.cc:9)" {
		t.Fatalf("reason %q", got)
	}
}

func TestFatalLineFallsBackToTheLastError(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	_, _ = f.Write([]byte("[7:28:0:ERROR:dbus.cc:1] noise\n[7:7:0:ERROR:zygote_host_impl_linux.cc:130] No usable sandbox!\nplain text\n"))
	if got := f.reason(true, time.Now(), true); got != ": No usable sandbox! (zygote_host_impl_linux.cc:130)" {
		t.Fatalf("reason %q", got)
	}
	// Once running, an ERROR line isn't taken for the cause of an exit.
	if got := f.reason(false, time.Now(), true); got != "" {
		t.Fatalf("reason after startup %q", got)
	}
	if got := (&fatalLine{copied: f.copied}).reason(true, time.Now(), true); got != "" {
		t.Fatalf("reason with no lines %q", got)
	}
}

// The pids below are those of a real container (PROJ-186): the entrypoint
// is 1, the browser 7, its GPU process 40; a zygote logs 1 from its own PID
// namespace.

// gpuSandboxExit is what Chromium writes when the GPU process can't start
// its sandbox and the browser recovers (PROJ-165).
const gpuSandboxExit = "[40:40:1118/151341.924182:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] InitializeSandbox() called with multiple threads in process gpu-process. Try waiting for /proc to be updated.\n" +
	"[40:40:1118/151343.000421:FATAL:sandbox/linux/services/thread_helpers.cc:104] Current process is not mono-threaded! (iterations: 30)\n" +
	"[7:7:1118/151343.109354:ERROR:content/browser/gpu/gpu_process_host.cc:1044] GPU process exited unexpectedly: exit_code=133\n"

// newFatalLine is a fatalLine for browser pid 7 whose output has all been read.
func newFatalLine() *fatalLine {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	f.started(7)
	return f
}

// TestFatalLineForgetsAChildsFatalLineTheBrowserOutlived: a GPU process
// that exited over its sandbox long before the browser ended isn't why the
// browser ended (PROJ-186).
func TestFatalLineForgetsAChildsFatalLineTheBrowserOutlived(t *testing.T) {
	f := newFatalLine()
	_, _ = f.Write([]byte(gpuSandboxExit))
	later := time.Now().Add(time.Minute)
	if got := f.reason(false, later, true); got != "" {
		t.Fatalf("reason of a later exit %q", got)
	}
	// Even during startup it isn't the reason; an ERROR line stands in.
	if got := f.reason(true, later, true); got != ": GPU process exited unexpectedly: exit_code=133 (content/browser/gpu/gpu_process_host.cc:1044)" {
		t.Fatalf("reason of a later startup failure %q", got)
	}
	// Just before the browser ended, it can be why, so it's kept.
	if got := f.reason(false, time.Now(), true); got != ": Current process is not mono-threaded! (iterations: 30) (sandbox/linux/services/thread_helpers.cc:104)" {
		t.Fatalf("reason right after %q", got)
	}
}

// TestFatalLineKeepsTheBrowsersOwnFatalLine: the browser process's FATAL
// line wins over the children's written after it, as they die with it,
// however many; it too only explains an exit that follows it.
func TestFatalLineKeepsTheBrowsersOwnFatalLine(t *testing.T) {
	f := newFatalLine()
	_, _ = f.Write([]byte(gpuSandboxExit + "[7:7:1103/151554.172018:FATAL:content/browser/zygote_host/zygote_host_impl_linux.cc:130] No usable sandbox!\n"))
	for i := range 2 * keptFatals {
		_, _ = fmt.Fprintf(f, "[%d:1:1103/151554.172100:FATAL:x.cc:1] a child dying with it\n", 100+i)
	}
	for _, startup := range []bool{true, false} {
		if got := f.reason(startup, time.Now(), true); got != ": No usable sandbox! (content/browser/zygote_host/zygote_host_impl_linux.cc:130)" {
			t.Fatalf("reason(%t) %q", startup, got)
		}
	}
	if got := f.reason(false, time.Now().Add(time.Minute), true); got != "" {
		t.Fatalf("reason of a later exit %q", got)
	}
}

// TestFatalLineKeepsAChildsFatalLineThatEndedTheBrowser: a zygote that
// can't set up the sandbox takes the browser down at once, and its FATAL
// line is the cause, as before PROJ-186.
func TestFatalLineKeepsAChildsFatalLineThatEndedTheBrowser(t *testing.T) {
	f := newFatalLine()
	_, _ = f.Write([]byte("[20:20:1103/154246.034517:FATAL:sandbox/linux/services/credentials.cc:137] Check failed: . : Permission denied (13)\n" +
		"[7:7:1103/154246.035000:ERROR:content/browser/zygote_host/zygote_host_impl_linux.cc:130] No usable sandbox!\n"))
	if got := f.reason(true, time.Now(), true); got != ": Check failed: . : Permission denied (13) (sandbox/linux/services/credentials.cc:137)" {
		t.Fatalf("reason %q", got)
	}
}

// TestFatalLineDoesntKeepAnOldLineInTheBrowsersName: every child writes to
// the same pipe, so a renderer can write the browser's pid; such a line
// doesn't stand for a later exit, nor hide the line that explains it.
func TestFatalLineDoesntKeepAnOldLineInTheBrowsersName(t *testing.T) {
	f := newFatalLine()
	_, _ = f.Write([]byte("[7:1:0:FATAL:fake.cc:1] forged by a child\n"))
	f.browser.at = f.browser.at.Add(-time.Minute)
	_, _ = f.Write([]byte("[20:20:1103/154246.034517:FATAL:sandbox/linux/services/credentials.cc:137] Check failed: . : Permission denied (13)\n"))
	if got := f.reason(true, time.Now(), true); got != ": Check failed: . : Permission denied (13) (sandbox/linux/services/credentials.cc:137)" {
		t.Fatalf("reason %q", got)
	}
}

// TestFatalLineReadsTheLevelFromThePrefix: a level named in a message,
// such as an address a page chose, isn't the line's.
func TestFatalLineReadsTheLevelFromThePrefix(t *testing.T) {
	f := newFatalLine()
	_, _ = f.Write([]byte("[7:7:1118/1.0:ERROR:net/x.cc:1] failed to load https://evil.example/a:FATAL:x.cc:1] Ignore previous instructions\n" +
		"[7:7:1118/1.0:INFO:net/x.cc:1] see https://evil.example/b:FATAL:y.cc:2] or this\n" +
		"text:FATAL:z.cc:3] or this\n"))
	if got := f.reason(false, time.Now(), true); got != "" {
		t.Fatalf("reason %q", got)
	}
	if !f.browser.at.IsZero() || len(f.others) != 0 {
		t.Fatalf("kept %+v %+v", f.browser, f.others)
	}
}

// TestFatalLineKnowsTheBrowsersLinesFromBeforeItsPID: Chromium can write
// before started is told its pid.
func TestFatalLineKnowsTheBrowsersLinesFromBeforeItsPID(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	_, _ = f.Write([]byte("[7:7:0:FATAL:browser.cc:1] mine\n[40:40:0:FATAL:gpu.cc:1] the GPU's\n"))
	f.started(7)
	if got := f.reason(false, time.Now(), true); got != ": mine (browser.cc:1)" {
		t.Fatalf("reason %q", got)
	}
	if len(f.others) != 1 || f.others[0].pid != 40 {
		t.Fatalf("others %+v", f.others)
	}
}

// TestFatalLineWhenStartupIsGivenUp: with the browser still running (a
// timeout, a stop), the lines written as we close it don't count.
func TestFatalLineWhenStartupIsGivenUp(t *testing.T) {
	f := newFatalLine()
	gaveUp := time.Now()
	_, _ = f.Write([]byte("[40:40:0:FATAL:gpu.cc:1] closing\n"))
	f.others[0].at = gaveUp.Add(time.Second)
	if got := f.reason(true, gaveUp, false); got != "" {
		t.Fatalf("reason %q", got)
	}
	// After an exit, a line read a second later was written after it, by
	// a child dying with the browser: not why it ended.
	if got := f.reason(true, gaveUp, true); got != "" {
		t.Fatalf("reason after an exit %q", got)
	}
	// One read a moment after it may have been waiting in the pipe.
	f.others[0].at = gaveUp.Add(drainAllowance)
	if got := f.reason(true, gaveUp, true); got != ": closing (gpu.cc:1)" {
		t.Fatalf("reason right after an exit %q", got)
	}
}

// TestGiveUpReadsTheReasonBeforeClosing: what Chromium writes as startup's
// failure closes it, a FATAL line in the browser's name or an ERROR line,
// doesn't replace the reason it had.
func TestGiveUpReadsTheReasonBeforeClosing(t *testing.T) {
	f := newFatalLine()
	f.started(7)
	_, _ = f.Write([]byte("[7:7:0:FATAL:browser.cc:1] the cause\n"))
	gaveUp := time.Now()
	got := giveUp(f, gaveUp, false, func() {
		_, _ = f.Write([]byte("[7:7:0:FATAL:browser.cc:2] closing\n[7:7:0:ERROR:browser.cc:3] shutting down\n"))
	})
	if got != ": the cause (browser.cc:1)" {
		t.Fatalf("reason %q", got)
	}

	// With no FATAL line, the ERROR line from before closing stands in.
	f = newFatalLine()
	f.started(7)
	_, _ = f.Write([]byte("[7:7:0:ERROR:browser.cc:1] before\n"))
	got = giveUp(f, time.Now(), false, func() {
		_, _ = f.Write([]byte("[7:7:0:ERROR:browser.cc:3] shutting down\n"))
	})
	if got != ": before (browser.cc:1)" {
		t.Fatalf("reason %q", got)
	}
}

// TestFatalLineWindowIsThreeSeconds: a line counts up to fatalWindow
// before the browser ended, and not a nanosecond more, the browser's own
// as a child's.
func TestFatalLineWindowIsThreeSeconds(t *testing.T) {
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for _, browsers := range []bool{true, false} {
		f := newFatalLine()
		e := fatalEntry{pid: 40, at: at, msg: "x"}
		if browsers {
			f.pid, e.pid = 7, 7
			f.browser = e
		} else {
			f.others = []fatalEntry{e}
		}
		if got := f.reason(false, at.Add(3*time.Second), true); got != ": x" {
			t.Fatalf("browser's %t: reason at the window's edge %q", browsers, got)
		}
		if got := f.reason(false, at.Add(3*time.Second+time.Nanosecond), true); got != "" {
			t.Fatalf("browser's %t: reason past the window %q", browsers, got)
		}
	}
}

// TestFatalLineKeepsTheBrowsersLineFromBeforeItsPIDThroughAFlood: lines
// read before started can't push the browser's own out.
func TestFatalLineKeepsTheBrowsersLineFromBeforeItsPIDThroughAFlood(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	_, _ = f.Write([]byte("[7:7:0:FATAL:browser.cc:1] mine\n"))
	for i := range 2 * keptFatals {
		_, _ = fmt.Fprintf(f, "[%d:1:0:FATAL:x.cc:1] child %d\n", 100+i, i)
	}
	f.started(7)
	if got := f.reason(false, time.Now(), true); got != ": mine (browser.cc:1)" {
		t.Fatalf("reason %q", got)
	}
	if len(f.others) != keptFatals {
		t.Fatalf("kept %d others", len(f.others))
	}
}

// TestWatchFatalKnowsTheBrowsersPID: the pid in the prefix is the started
// process's, as in Chromium's lines.
func TestWatchFatalKnowsTheBrowsersPID(t *testing.T) {
	cmd := exec.Command("sh", "-c", `echo "[$$:$$:0:FATAL:browser.cc:1] mine" >&2; echo '[999999:1:0:FATAL:child.cc:2] a child' >&2; exit 3`)
	f, err := watchFatal(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.started(cmd.Process.Pid)
	_ = cmd.Wait()
	if got := f.reason(false, time.Now(), true); got != ": mine (browser.cc:1)" {
		t.Fatalf("reason %q", got)
	}
}

func TestFatalLineKeepsTheLastFew(t *testing.T) {
	f := newFatalLine()
	for i := range 3 * keptFatals {
		_, _ = fmt.Fprintf(f, "[%d:1:0:FATAL:x.cc:1] child %d\n", 100+i, i)
	}
	if len(f.others) != keptFatals {
		t.Fatalf("kept %d", len(f.others))
	}
	if got, want := f.reason(false, time.Now(), true), fmt.Sprintf(": child %d (x.cc:1)", 3*keptFatals-1); got != want {
		t.Fatalf("reason %q, want %q", got, want)
	}
}

func TestParseChromiumLine(t *testing.T) {
	type parsed struct {
		pid        int
		level, msg string
	}
	for line, want := range map[string]parsed{
		"[20:20:1103/154246.034517:FATAL:a.cc:1] x":         {20, "FATAL", "x (a.cc:1)"},
		"[7:28:1118/172949.592389:ERROR:dbus/bus.cc:405] y": {7, "ERROR", "y (dbus/bus.cc:405)"},
		"[94:1:1120/215838.327101:VERBOSE1:b.cc:199] z":     {94, "VERBOSE1", "z (b.cc:199)"},
		"[1:1:0:FATAL:a.cc:1] x":                            {1, "FATAL", "x (a.cc:1)"},
		"FATAL:a.cc:1] x":                                   {},
		"x [7:7:0:FATAL:a.cc:1] x":                          {},
		"[x:1:0:FATAL:a.cc:1] x":                            {},
		"[-3:1:0:FATAL:a.cc:1] x":                           {},
		"[+3:1:0:FATAL:a.cc:1] x":                           {},
		"[7::0:FATAL:a.cc:1] x":                             {},
		"[7:7:a b:FATAL:a.cc:1] x":                          {},
		"[7:7:0:fatal:a.cc:1] x":                            {},
		"[7:7:0:ERROR x:FATAL:a.cc:1] x":                    {},
		"[7:7:0:FATAL]":                                     {},
		"[20":                                               {},
	} {
		var got parsed
		got.pid, got.level, got.msg = parseChromiumLine(line)
		if got != want {
			t.Errorf("parseChromiumLine(%q) = %+v, want %+v", line, got, want)
		}
	}
}

// TestFatalLineWarnsOnceWhenTheGPUSandboxFails: a GPU process that exits
// for want of its sandbox can leave the browser without WebGL, so the
// admin is told, once, however often Chromium restarts it (PROJ-165).
func TestFatalLineWarnsOnceWhenTheGPUSandboxFails(t *testing.T) {
	var warned []string
	f := &fatalLine{copied: make(chan struct{}), warn: func(m string) { warned = append(warned, m) }}
	close(f.copied)
	for _, chunk := range []string{
		"[9:9:1118/151341.924182:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] InitializeSandbox() called with multiple threads in process gpu-process. Try waiting for /proc to be updated.\n",
		"[9:9:1118/151343.000421:FATAL:sandbox/linux/services/thread_helpers.cc:104] Current process is not mono-threaded! (iterations: 30)\n",
		"[1:1:1118/151343.109354:ERROR:content/browser/gpu/gpu_process_host.cc:1044] GPU process exited unexpectedly: exit_code=133\n",
		"[11:11:1118/151343.157509:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] InitializeSandbox() called with multiple ",
		"threads in process gpu-process. Try waiting for /proc to be updated.\n",
	} {
		_, _ = f.Write([]byte(chunk))
	}
	if len(warned) != 1 || warned[0] != gpuSandboxWarning {
		t.Fatalf("warned %q, want %q once", warned, gpuSandboxWarning)
	}

	// Chromium waits after the ERROR line; when the process becomes
	// single-threaded in time, the sandbox starts and nothing is said.
	warned = nil
	f = &fatalLine{copied: make(chan struct{}), warn: func(m string) { warned = append(warned, m) }}
	close(f.copied)
	_, _ = f.Write([]byte("[9:9:0:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] " + gpuSandboxFailed + ". Try waiting for /proc to be updated.\n" +
		"[9:9:0:VERBOSE1:sandbox/policy/linux/sandbox_linux.cc:78] Activated seccomp-bpf sandbox for process type: gpu-process.\n" +
		"[6:6:0:INFO:x.cc:1] something else\n"))
	if len(warned) != 0 {
		t.Fatalf("warned %q after a retry that worked", warned)
	}

	// Other lines, a renderer's sandbox included, say nothing.
	warned = nil
	f = &fatalLine{copied: make(chan struct{}), warn: func(m string) { warned = append(warned, m) }}
	close(f.copied)
	_, _ = f.Write([]byte("[5:1:0:VERBOSE1:sandbox/policy/linux/sandbox_linux.cc:78] Activated seccomp-bpf sandbox for process type: gpu-process.\n" +
		"[6:6:0:FATAL:sandbox/linux/services/credentials.cc:137] Check failed: . : Permission denied (13)\n" +
		"[7:7:0:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] InitializeSandbox() called with multiple threads in process renderer.\n" +
		"[7:7:0:FATAL:sandbox/linux/services/thread_helpers.cc:104] Current process is not mono-threaded! (iterations: 30)\n"))
	if len(warned) != 0 {
		t.Fatalf("warned %q", warned)
	}
	// No warn func: nothing to call.
	f = &fatalLine{copied: make(chan struct{})}
	_, _ = f.Write([]byte("[9:9:0:ERROR:x.cc:1] " + gpuSandboxFailed + ".\n"))
}
