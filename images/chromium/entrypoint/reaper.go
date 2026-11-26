package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

// procs is the one reaper for this process. There must be only one: each
// reaper waits on every child, so two would steal each other's exit statuses.
var procs = newReaper()

// reaper waits for every child process, including orphans re-parented to
// PID 1, and hands each exit status to whoever started that process.
// Processes started through it must not be waited on with cmd.Wait.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]waiter
	// starts counts the processes started, for tests that must start none.
	starts atomic.Int64
}

type waiter struct {
	proc *os.Process
	ch   chan syscall.WaitStatus
}

func newReaper() *reaper {
	r := &reaper{waiters: map[int]waiter{}}
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go func() {
		for range sigchld {
			r.reap()
		}
	}()
	return r
}

// start starts cmd and returns a channel that receives its exit status.
// cmd's Stdout and Stderr must be nil or *os.File, since nothing calls Wait
// to finish copying from a pipe.
func (r *reaper) start(cmd *exec.Cmd) (<-chan syscall.WaitStatus, error) {
	ch, _, err := r.startPid(cmd)
	return ch, err
}

// startPid is start, also returning the child's pid: read here, under the
// lock, since reap releases the process (and its Pid) once it exits.
func (r *reaper) startPid(cmd *exec.Cmd) (<-chan syscall.WaitStatus, int, error) {
	for _, out := range []io.Writer{cmd.Stdout, cmd.Stderr} {
		if _, ok := out.(*os.File); out != nil && !ok {
			return nil, 0, errors.New("reaper: a command's stdout and stderr must be nil or *os.File")
		}
	}
	// Holding the lock across Start means reap can't collect the child
	// before it is registered, even if it exits at once.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts.Add(1)
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	pid := cmd.Process.Pid
	r.waiters[pid] = waiter{proc: cmd.Process, ch: ch}
	return ch, pid, nil
}

func (r *reaper) reap() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		if w, ok := r.waiters[pid]; ok {
			// Nothing calls Wait, so release the handle (a pidfd) here
			// rather than leave it open until the garbage collector runs.
			_ = w.proc.Release()
			w.ch <- ws
			delete(r.waiters, pid)
		}
	}
}

// setSubreaper makes orphaned descendants re-parent to this process even
// when it isn't PID 1, for example under `docker run --init`.
func setSubreaper() error {
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return errno
	}
	return nil
}
