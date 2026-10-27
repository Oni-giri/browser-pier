package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitStatus(t *testing.T, ch <-chan syscall.WaitStatus) syscall.WaitStatus {
	t.Helper()
	select {
	case ws := <-ch:
		return ws
	case <-time.After(5 * time.Second):
		t.Fatal("no exit status after 5s")
		return 0
	}
}

func TestReaperReportsExitStatus(t *testing.T) {
	ch, err := procs.start(exec.Command("sh", "-c", "exit 3"))
	if err != nil {
		t.Fatal(err)
	}
	if ws := waitStatus(t, ch); ws.ExitStatus() != 3 {
		t.Fatalf("exit status = %d, want 3", ws.ExitStatus())
	}
}

func TestReaperReportsSignal(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	ch, err := procs.start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if ws := waitStatus(t, ch); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("status = %v, want killed by SIGTERM", ws)
	}
}

func TestReaperProcessExitingImmediately(t *testing.T) {
	// The child may exit before start registers it; the status must still arrive.
	for range 50 {
		ch, err := procs.start(exec.Command("true"))
		if err != nil {
			t.Fatal(err)
		}
		waitStatus(t, ch)
	}
}

func TestReaperCollectsOrphans(t *testing.T) {
	if err := setSubreaper(); err != nil {
		t.Fatal(err)
	}
	// The shell exits at once; its background sleep is re-parented to us.
	ch, err := procs.start(exec.Command("sh", "-c", "sleep 0.2 & exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, ch)

	deadline := time.Now().Add(5 * time.Second)
	for {
		kids, zombies := children(t)
		if kids == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d children left after 5s, %d of them zombies", kids, zombies)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// children counts this process's live children and how many are zombies.
func children(t *testing.T) (n, zombies int) {
	t.Helper()
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		b, err := os.ReadFile("/proc/self/task/" + task.Name() + "/children")
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(b)) {
			n++
			stat, err := os.ReadFile("/proc/" + pid + "/stat")
			if err == nil && strings.Contains(string(stat), ") Z ") {
				zombies++
			}
		}
	}
	return n, zombies
}
