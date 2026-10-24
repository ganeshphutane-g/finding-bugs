// Command tsroam turns a Tailscale exit node on whenever the machine is not
// on one of its trusted networks, with an optional kill switch.
package main

import (
	"os"

	"github.com/ganeshphutane-g/tsroam/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, cli.Deps{}))
}
