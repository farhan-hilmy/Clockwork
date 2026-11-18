//go:build unix

package runners

import (
	"os/exec"
	"syscall"
)

// windowsEssentials is empty on Unix: only PATH is inherited.
var windowsEssentials []string

// setProcessGroup starts the command in its own process group and makes
// cancellation kill the whole group, including background children.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup kills whatever is left in the command's process group after it
// exits, such as children it started in the background. Processes that left the
// group (setsid) are not reached.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH when nothing is left
	}
}
