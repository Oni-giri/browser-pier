package main

import (
	"os/exec"
	"strings"
	"testing"
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
	if got := f.reason(false); got != ": second (c.cc:3)" {
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
	f.started()
	_ = cmd.Wait()
	if got := f.reason(true); got != ": Check failed: boom (sandbox/x.cc:9)" {
		t.Fatalf("reason %q", got)
	}
}

func TestFatalLineFallsBackToTheLastError(t *testing.T) {
	f := &fatalLine{copied: make(chan struct{})}
	close(f.copied)
	_, _ = f.Write([]byte("[1:1:0:ERROR:dbus.cc:1] noise\n[14:14:0:ERROR:zygote_host_impl_linux.cc:130] No usable sandbox!\nplain text\n"))
	if got := f.reason(true); got != ": No usable sandbox! (zygote_host_impl_linux.cc:130)" {
		t.Fatalf("reason %q", got)
	}
	// Once running, an ERROR line isn't taken for the cause of an exit.
	if got := f.reason(false); got != "" {
		t.Fatalf("reason after startup %q", got)
	}
	if got := (&fatalLine{copied: f.copied}).reason(true); got != "" {
		t.Fatalf("reason with no lines %q", got)
	}
}

// TestFatalLineWarnsOnceWhenTheGPUSandboxFails: the browser lives on
// without WebGL, so the admin is told, once, however often Chromium
// restarts the GPU process (PROJ-165).
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

	// Other lines, a renderer's sandbox included, say nothing.
	warned = nil
	f = &fatalLine{copied: make(chan struct{}), warn: func(m string) { warned = append(warned, m) }}
	close(f.copied)
	_, _ = f.Write([]byte("[5:1:0:VERBOSE1:sandbox/policy/linux/sandbox_linux.cc:78] Activated seccomp-bpf sandbox for process type: gpu-process.\n" +
		"[6:6:0:FATAL:sandbox/linux/services/credentials.cc:137] Check failed: . : Permission denied (13)\n" +
		"[7:7:0:ERROR:sandbox/policy/linux/sandbox_linux.cc:394] InitializeSandbox() called with multiple threads in process renderer.\n"))
	if len(warned) != 0 {
		t.Fatalf("warned %q", warned)
	}
	// No warn func: nothing to call.
	f = &fatalLine{copied: make(chan struct{})}
	_, _ = f.Write([]byte("[9:9:0:ERROR:x.cc:1] " + gpuSandboxFailed + ".\n"))
}
