//go:build windows

package source

import (
	"os"
	"os/exec"
)

// shellInvocation returns the shell and its command flag for running a command
// line on Windows. Uses %COMSPEC% (cmd.exe) with /c.
func shellInvocation() (shell, flag string) {
	shell = os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd"
	}
	return shell, "/c"
}

// setupProcGroup is a no-op on Windows (no POSIX process groups); killProcGroup
// falls back to killing the direct child.
func setupProcGroup(cmd *exec.Cmd) {}

// killProcGroup terminates the child process. Windows doesn't have POSIX
// process groups; killing the direct child is the pragmatic behaviour.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
