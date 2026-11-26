package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeProc is a /proc with the processes given: pid -> parent, start.
func fakeProc(t *testing.T, procs map[int][2]int) procTree {
	t.Helper()
	root := t.TempDir()
	for pid, ps := range procs {
		setProc(t, root, pid, ps[0], ps[1])
	}
	if err := os.MkdirAll(filepath.Join(root, "self", "fdinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return procTree{root: root}
}

func setProc(t *testing.T, root string, pid, ppid, start int) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A name with a space and a parenthesis, as Chromium's helpers can have.
	stat := fmt.Sprintf("%d (chromium (x)) S %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, ppid, start)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setPidfd(t *testing.T, tree procTree, fd, pid int) {
	t.Helper()
	info := fmt.Sprintf("pos:\t0\nflags:\t02000002\nmnt_id:\t15\nino:\t1\nPid:\t%d\nNSpid:\t%d\n", pid, pid)
	if err := os.WriteFile(filepath.Join(tree.root, "self", "fdinfo", strconv.Itoa(fd)), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCallerCheck(t *testing.T) {
	// 1 the entrypoint, 40 Chromium, 50 its zygote, 51 a renderer,
	// 60 a stranger under the entrypoint.
	tree := fakeProc(t, map[int][2]int{1: {0, 10}, 40: {1, 100}, 50: {40, 110}, 51: {50, 120}, 60: {1, 130}})
	b := &browserCheck{proc: tree}
	if err := b.allow(callerCreds{pid: 40, pidfd: -1}); err == nil {
		t.Fatal("allowed before the browser started")
	}
	if err := b.setBrowser(40); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		c    callerCreds
		ok   bool
	}{
		{"the browser by pid", callerCreds{pid: 40, pidfd: -1}, true},
		{"a descendant by pid", callerCreds{pid: 51, pidfd: -1}, true},
		{"a stranger by pid", callerCreds{pid: 60, pidfd: -1}, false},
		{"the entrypoint", callerCreds{pid: 1, pidfd: -1}, false},
		{"no pid at all", callerCreds{pidfd: -1}, false},
		{"a missing process", callerCreds{pid: 99, pidfd: -1}, false},
	} {
		if err := b.allow(tc.c); (err == nil) != tc.ok {
			t.Errorf("%s: allow = %v, want ok %v", tc.name, err, tc.ok)
		}
	}

	setPidfd(t, tree, 7, 51)
	if err := b.allow(callerCreds{pid: 51, pidfd: 7}); err != nil {
		t.Errorf("a descendant by pidfd: %v", err)
	}
	setPidfd(t, tree, 8, 60)
	if err := b.allow(callerCreds{pid: 60, pidfd: 8}); err == nil {
		t.Error("a stranger by pidfd was allowed")
	}
	// The pidfd names the stranger though the bus says 51: refused.
	if err := b.allow(callerCreds{pid: 51, pidfd: 8}); err == nil {
		t.Error("a pidfd naming another process was allowed")
	}
	setPidfd(t, tree, 9, -1)
	if err := b.allow(callerCreds{pid: 51, pidfd: 9}); err == nil {
		t.Error("a caller whose pidfd says it exited was allowed")
	}

	// The browser's pid reused by another process: nothing is allowed.
	setProc(t, tree.root, 40, 1, 999)
	if err := b.allow(callerCreds{pid: 40, pidfd: -1}); err == nil {
		t.Error("allowed a new process with the browser's pid")
	}
	setProc(t, tree.root, 40, 1, 100)
	b.browserExited()
	if err := b.allow(callerCreds{pid: 40, pidfd: -1}); err == nil {
		t.Error("allowed after the browser exited")
	}
}

func TestCallerCheckReadsTheRealProc(t *testing.T) {
	b := &browserCheck{proc: procTree{root: "/proc"}}
	if err := b.setBrowser(os.Getppid()); err != nil {
		t.Fatal(err)
	}
	if err := b.allow(callerCreds{pid: os.Getpid(), pidfd: -1}); err != nil {
		t.Fatalf("this test, under its parent: %v", err)
	}
	if err := b.setBrowser(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := b.allow(callerCreds{pid: os.Getppid(), pidfd: -1}); err == nil {
		t.Fatal("the parent passed as the browser's descendant")
	}
}
