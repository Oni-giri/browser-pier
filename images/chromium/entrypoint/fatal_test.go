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
	f, err := watchFatal(cmd)
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
