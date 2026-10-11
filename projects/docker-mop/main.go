// Command docker-mop is a Docker CLI plugin that previews prunes before they
// delete anything and renames or copies volumes.
//
// Install it as ~/.docker/cli-plugins/docker-mop and run `docker mop`.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/docker-mop/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
