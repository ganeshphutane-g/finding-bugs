// Command packparity keeps repair data for restic repositories.
package main

import (
	"os"

	"github.com/ganeshphutane-g/packparity/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
