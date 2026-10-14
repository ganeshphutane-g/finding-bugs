// Command pgcarry upgrades the PostgreSQL service of a Docker Compose
// project to a new major version, safely.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/pgcarry/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Stdin, os.Stdout, os.Stderr, os.Args[1:])
	stop()
	os.Exit(code)
}
