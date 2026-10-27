package steps

import (
	"os/exec"
	"syscall"
)

// inGroup runs cmd in its own process group, and kills the group when its
// context ends. If composehost itself is killed (Compose does that on
// Ctrl-C), the kernel kills the command's shell too.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
