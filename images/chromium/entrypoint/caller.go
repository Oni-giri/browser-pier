package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// The portal's caller check (PROJ-152): it answers only the Chromium the
// entrypoint started and that Chromium's descendants. The bus already
// limits callers to the browser's uid and no agent code runs in the
// container, so this is defence in depth: a process of that uid that
// isn't the browser (a stray helper) can't ask for a file.
//
// The bus names the caller by a pidfd (ProcessFD in
// GetConnectionCredentials) when the kernel gives one (SO_PEERPIDFD,
// Linux 6.5 on): a pidfd can't come to name another process, so after the
// walk up the parents the check reads it again, and a caller that exited
// meanwhile (its pid possibly reused) is refused. On older kernels there
// is only the ProcessID; then the walk is bracketed by the process's start
// time instead. That fallback has a known limit: the bus records the pid
// when the connection is made, so a process of the browser's uid that
// connects, hands the socket to a child and exits leaves a connection
// whose recorded pid a later Chromium descendant may reuse, and the check
// would pass. The pidfd path refuses it (the pidfd says the process
// exited). Accepted as defence in depth: the bus already admits only the
// browser's uid, and no agent code runs in the container.

// callerCreds is what the bus says about a caller.
type callerCreds struct {
	pid   int // ProcessID, 0 if none
	pidfd int // ProcessFD, -1 if none; the caller closes it
}

// procTree reads processes from a /proc.
type procTree struct{ root string }

// stat is a process's parent and start time (fields 4 and 22 of
// /proc/<pid>/stat).
func (t procTree) stat(pid int) (ppid int, start uint64, err error) {
	b, err := os.ReadFile(filepath.Join(t.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the name may hold spaces and parentheses
	if i < 0 || i+2 > len(s) {
		return 0, 0, fmt.Errorf("process %d: unreadable stat", pid)
	}
	f := strings.Fields(s[i+2:]) // from field 3, the state
	if len(f) < 20 {
		return 0, 0, fmt.Errorf("process %d: unreadable stat", pid)
	}
	if ppid, err = strconv.Atoi(f[1]); err != nil {
		return 0, 0, fmt.Errorf("process %d: unreadable stat", pid)
	}
	if start, err = strconv.ParseUint(f[19], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("process %d: unreadable stat", pid)
	}
	return ppid, start, nil
}

// pidfdPid is the pid a pidfd of ours names, or -1 once that process has
// exited (the fdinfo's Pid line).
func (t procTree) pidfdPid(fd int) (int, error) {
	b, err := os.ReadFile(filepath.Join(t.root, "self", "fdinfo", strconv.Itoa(fd)))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Pid:"); ok {
			return strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return 0, errors.New("not a pidfd")
}

// maxDepth bounds the walk up the parents: Chromium's helpers are a few
// levels below it.
const maxDepth = 32

// descends reports whether pid is ancestor or one of its descendants.
func (t procTree) descends(pid, ancestor int) (bool, error) {
	for range maxDepth {
		if pid == ancestor {
			return true, nil
		}
		if pid <= 1 {
			return false, nil
		}
		ppid, _, err := t.stat(pid)
		if err != nil {
			return false, err
		}
		pid = ppid
	}
	return false, nil
}

// browserCheck is the caller check for the browser started at pid.
type browserCheck struct {
	proc procTree
	mu   sync.Mutex
	pid  int    // the browser's pid, 0 before it starts and once it has exited
	when uint64 // its start time
}

// setBrowser names the browser once it has started.
func (b *browserCheck) setBrowser(pid int) error {
	_, start, err := b.proc.stat(pid)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pid, b.when = pid, start
	return nil
}

// browserExited forgets the browser: nothing is answered after it exits.
func (b *browserCheck) browserExited() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pid = 0
}

// allow checks a caller: the browser, or a process under it.
func (b *browserCheck) allow(c callerCreds) error {
	b.mu.Lock()
	browser, when := b.pid, b.when
	b.mu.Unlock()
	if browser == 0 {
		return errors.New("the browser isn't running")
	}
	if _, start, err := b.proc.stat(browser); err != nil || start != when {
		return errors.New("the browser isn't running")
	}
	pid := c.pid
	var start uint64
	if c.pidfd >= 0 {
		p, err := b.proc.pidfdPid(c.pidfd)
		switch {
		case err != nil:
			return fmt.Errorf("caller's pidfd: %w", err)
		case p <= 0:
			return errors.New("the caller has exited")
		case c.pid != 0 && p != c.pid:
			return fmt.Errorf("the caller's pidfd names process %d, not %d", p, c.pid)
		}
		pid = p
	} else {
		if pid <= 0 {
			return errors.New("the bus doesn't say which process called")
		}
		var err error
		if _, start, err = b.proc.stat(pid); err != nil {
			return fmt.Errorf("caller: %w", err)
		}
	}
	ok, err := b.proc.descends(pid, browser)
	if err != nil {
		return fmt.Errorf("caller: %w", err)
	}
	if !ok {
		return fmt.Errorf("process %d isn't the browser or under it", pid)
	}
	// The walk took a moment: the caller must still be the process it
	// was, not a new one with its pid.
	if c.pidfd >= 0 {
		if p, err := b.proc.pidfdPid(c.pidfd); err != nil || p != pid {
			return errors.New("the caller exited during the check")
		}
		return nil
	}
	if _, again, err := b.proc.stat(pid); err != nil || again != start {
		return errors.New("the caller exited during the check")
	}
	return nil
}
