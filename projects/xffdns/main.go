// xffdns passes DNS queries on to AdGuard Home's DoH endpoint, naming the
// real client in X-Forwarded-For.
package main

import (
	"os"

	"github.com/ganeshphutane-g/xffdns/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
