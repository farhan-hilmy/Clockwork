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
