package cli

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// dieWithParent asks for SIGTERM when Compose (the parent) dies, killed
// or not: that cancels the steps, and kills the commands they started.
func dieWithParent() {
	ppid := os.Getppid()
	unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0)
	if os.Getppid() != ppid {
		// It died before the request.
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}
}
