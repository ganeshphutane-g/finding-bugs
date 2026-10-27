// Command composehost runs host-side steps for Docker Compose projects as a
// Compose provider service.
package main

import (
	"os"

	"github.com/ganeshphutane-g/composehost/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
