// Command composehost runs host-side steps for Docker Compose projects as a
// Compose provider service.
package main

import (
	"os"
	"runtime"

	"github.com/ganeshphutane-g/composehost/internal/cli"
)

// The main goroutine stays on the process's first thread, which the
// parent-death signal (cli.dieWithParent) is set on.
func init() { runtime.LockOSThread() }

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
