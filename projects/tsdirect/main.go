// Command tsdirect keeps LAN traffic on the LAN when Tailscale's subnet
// routes cover it, while the machine really is on that LAN.
package main

import (
	"os"

	"github.com/ganeshphutane-g/tsdirect/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, cli.Deps{}))
}
