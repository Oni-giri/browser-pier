package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckMounts(t *testing.T) {
	profile, control := t.TempDir(), t.TempDir()
	c := config{ProfileDir: profile, ControlSocket: filepath.Join(control, "control.sock"), MountCheck: "s1"}
	write := func(dir, v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, mountMarker), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := checkMounts(c); err == nil {
		t.Fatal("want an error with no markers")
	}
	write(profile, "s1")
	if err := checkMounts(c); err == nil || !strings.Contains(err.Error(), control) {
		t.Fatalf("want an error naming the control folder, got %v", err)
	}
	write(control, "s0") // left by an earlier start
	if err := checkMounts(c); err == nil {
		t.Fatal("want an error for a stale marker")
	}
	write(control, "s1")
	if err := checkMounts(c); err != nil {
		t.Fatal(err)
	}

	// Run by hand, without an orchestrator, there's nothing to check.
	if err := checkMounts(config{ProfileDir: t.TempDir(), ControlSocket: "/nowhere/control.sock"}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	if err := prepareHome(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		t.Fatalf("HOME not created: %v", err)
	}
}
