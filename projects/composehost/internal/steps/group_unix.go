//go:build unix

package steps

import (
	"os/exec"
	"syscall"
)

// inGroup runs cmd in its own process group, and kills the group when its
// context ends.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
