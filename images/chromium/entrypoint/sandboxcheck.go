package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// sandboxCheckArg runs the sandbox check instead of the browser. The
// orchestrator starts a short-lived container with it, under the browser
// containers' exact settings, to find an AppArmor profile Chromium's
// sandbox works under (see internal/docker).
const sandboxCheckArg = "sandbox-check"

// sandboxCheck does what Chromium's sandbox needs from the kernel: a child
// in new user, PID and network namespaces that then chroots, which takes
// a capability inside its namespace. Creating the namespace isn't enough:
// under Ubuntu's user namespace restriction an unconfined process gets one
// without capabilities ("No usable sandbox!"), while Docker's AppArmor
// profile under AppArmor 4 refuses it outright (EACCES).
func sandboxCheck() error {
	cmd := exec.Command("/bin/true")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		Chroot:      "/",
	}
	exited, err := procs.start(cmd)
	if err != nil {
		return fmt.Errorf("can't create the sandbox's namespaces: %w", err)
	}
	if ws := <-exited; ws.ExitStatus() != 0 {
		return fmt.Errorf("the sandbox's test process failed: %s", describe(ws))
	}
	return nil
}

// runSandboxCheck prints the check's result as the container's last log
// line, and exits non-zero on failure.
func runSandboxCheck() int {
	if err := sandboxCheck(); err != nil {
		fmt.Println("sandbox-check: failed:", err)
		return 1
	}
	fmt.Println("sandbox-check: ok")
	return 0
}
