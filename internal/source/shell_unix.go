//go:build !windows

package source

import (
	"os"
	"os/exec"
	"syscall"
)

// shellInvocation returns the shell and its command flag for running a command
// line on Unix. Honours $SHELL if set, else falls back to /bin/sh.
func shellInvocation() (shell, flag string) {
	shell = os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return shell, "-c"
}

// setupProcGroup starts the child in its own process group so we can later
// signal the whole group (child + any grandchildren it spawns).
func setupProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup sends SIGTERM to the child's entire process group. A negative
// pid targets the group whose id equals the child's pid (set via Setpgid).
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
