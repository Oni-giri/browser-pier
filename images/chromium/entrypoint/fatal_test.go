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
	for _, chunk := range []string{"[1:1:0:FATAL:a.cc:1] first\n[1:1:0:ERR", "OR:b.cc:2] noise\n[1:1:0:FA", "TAL:c.cc:3] sec", "ond\nunfinished"} {
		_, _ = f.Write([]byte(chunk))
	}
	if got := f.reason(false, time.Now()); got != ": second (c.cc:3)" {
		t.Fatalf("reason %q", got)
	}
}

func TestWatchFatalSeesAChildsFatalLine(t *testing.T) {
	cmd := exec.Command("sh", "-c", `echo '[7:7:0:FATAL:sandbox/x.cc:9] Check failed: boom' >&2; exit 3`)
	f, err := watchFatal(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.started(cmd.Process.Pid)
	_ = cmd.Wait()
	if got := f.reason(true, time.Now()); got != ": Check failed: boom (sandbox/x.cc:9)" {
		t.Fatalf("reason %q", got)
	}
}

func TestFatalLineFallsBackToTheLastError(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	_, _ = f.Write([]byte("[1:1:0:ERROR:dbus.cc:1] noise\n[14:14:0:ERROR:zygote_host_impl_linux.cc:130] No usable sandbox!\nplain text\n"))
	if got := f.reason(true, time.Now()); got != ": No usable sandbox! (zygote_host_impl_linux.cc:130)" {
		t.Fatalf("reason %q", got)
	}
	// Once running, an ERROR line isn't taken for the cause of an exit.
	if got := f.reason(false, time.Now()); got != "" {
		t.Fatalf("reason after startup %q", got)
	}
	if got := (&fatalLine{copied: f.copied}).reason(true, time.Now()); got != "" {
		t.Fatalf("reason with no lines %q", got)
	}
}

// gpuSandboxExit is what Chromium writes when the GPU process (pid 9) can't
// start its sandbox and the browser (pid 1) recovers (PROJ-165).
const gpuSandboxExit = "[9:9:1118/151341.924182:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] InitializeSandbox() called with multiple threads in process gpu-process. Try waiting for /proc to be updated.\n" +
	"[9:9:1118/151343.000421:FATAL:sandbox/linux/services/thread_helpers.cc:104] Current process is not mono-threaded! (iterations: 30)\n" +
	"[1:1:1118/151343.109354:ERROR:content/browser/gpu/gpu_process_host.cc:1044] GPU process exited unexpectedly: exit_code=133\n"

// TestFatalLineForgetsAChildsFatalLineTheBrowserOutlived: a GPU process
// that exited over its sandbox long before the browser ended isn't why the
// browser ended (PROJ-186).
func TestFatalLineForgetsAChildsFatalLineTheBrowserOutlived(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{}), pid: 1}
	close(f.copied)
	_, _ = f.Write([]byte(gpuSandboxExit))
	later := time.Now().Add(time.Minute)
	if got := f.reason(false, later); got != "" {
		t.Fatalf("reason of a later exit %q", got)
	}
	// Even during startup it isn't the reason; an ERROR line stands in.
	if got := f.reason(true, later); got != ": GPU process exited unexpectedly: exit_code=133 (content/browser/gpu/gpu_process_host.cc:1044)" {
		t.Fatalf("reason of a later startup failure %q", got)
	}
	// Just before the browser ended, it can be why, so it's kept.
	if got := f.reason(false, time.Now()); got != ": Current process is not mono-threaded! (iterations: 30) (sandbox/linux/services/thread_helpers.cc:104)" {
		t.Fatalf("reason right after %q", got)
	}
}

// TestFatalLineKeepsTheBrowsersOwnFatalLine: the browser process's FATAL
// line is the reason however long ago it came, and wins over a child's
// written after it.
func TestFatalLineKeepsTheBrowsersOwnFatalLine(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{}), pid: 14}
	close(f.copied)
	_, _ = f.Write([]byte(gpuSandboxExit +
		"[14:14:1103/151554.172018:FATAL:content/browser/zygote_host/zygote_host_impl_linux.cc:130] No usable sandbox!\n" +
		"[20:20:1103/151554.172100:FATAL:x.cc:1] a child dying with it\n"))
	for _, ended := range []time.Time{time.Now(), time.Now().Add(time.Minute)} {
		for _, startup := range []bool{true, false} {
			if got := f.reason(startup, ended); got != ": No usable sandbox! (content/browser/zygote_host/zygote_host_impl_linux.cc:130)" {
				t.Fatalf("reason(%t, %s) %q", startup, ended, got)
			}
		}
	}
}

// TestFatalLineKeepsAChildsFatalLineThatEndedTheBrowser: a zygote that
// can't set up the sandbox takes the browser down at once, and its FATAL
// line is the cause, as before PROJ-186.
func TestFatalLineKeepsAChildsFatalLineThatEndedTheBrowser(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{}), pid: 14}
	close(f.copied)
	_, _ = f.Write([]byte("[20:20:1103/154246.034517:FATAL:sandbox/linux/services/credentials.cc:137] Check failed: . : Permission denied (13)\n" +
		"[14:14:1103/154246.035000:ERROR:content/browser/zygote_host/zygote_host_impl_linux.cc:130] No usable sandbox!\n"))
	if got := f.reason(true, time.Now()); got != ": Check failed: . : Permission denied (13) (sandbox/linux/services/credentials.cc:137)" {
		t.Fatalf("reason %q", got)
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
	if got := f.reason(false, time.Now().Add(time.Minute)); got != ": mine (browser.cc:1)" {
		t.Fatalf("reason %q", got)
	}
}

func TestFatalLineKeepsTheLastFew(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{}), pid: 1}
	close(f.copied)
	for i := range 3 * keptFatals {
		_, _ = fmt.Fprintf(f, "[%d:1:0:FATAL:x.cc:1] child %d\n", 100+i, i)
	}
	if len(f.fatals) != keptFatals {
		t.Fatalf("kept %d", len(f.fatals))
	}
	if got, want := f.reason(false, time.Now()), fmt.Sprintf(": child %d (x.cc:1)", 3*keptFatals-1); got != want {
		t.Fatalf("reason %q, want %q", got, want)
	}
}

func TestLinePID(t *testing.T) {
	for line, want := range map[string]int{
		"[20:20:1103/154246.034517:FATAL:a.cc:1] x": 20,
		"[1:1:0:FATAL:a.cc:1] x":                    1,
		"FATAL:a.cc:1] x":                           0,
		"[x:1:0:FATAL:a.cc:1] x":                    0,
		"[0:1:0:FATAL:a.cc:1] x":                    0,
		"[-3:1:0:FATAL:a.cc:1] x":                   0,
		"[20":                                       0,
	} {
		if got := linePID(line); got != want {
			t.Errorf("linePID(%q) = %d, want %d", line, got, want)
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
