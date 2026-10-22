// Command wgresolve keeps WireGuard peers with DNS-name endpoints connected.
package main

import (
	"os"

	"github.com/ganeshphutane-g/wgresolve/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, cli.Deps{}))
}
