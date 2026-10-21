// pullwhy shows why a docker pull is slow or fails.
package main

import (
	"os"

	"github.com/ganeshphutane-g/pullwhy/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
