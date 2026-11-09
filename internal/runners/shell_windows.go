//go:build windows

package runners

import "os/exec"

// windowsEssentials are inherited on Windows (development only), where programs
// fail to start without them. Production images run on Linux.
var windowsEssentials = []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP"}

// setProcessGroup is best effort on Windows: only the direct child is killed on
// cancellation. Clockwork's supported deployment is Linux.
func setProcessGroup(*exec.Cmd) {}
