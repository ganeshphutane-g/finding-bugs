// Command docker-edit changes an existing container (ports, environment,
// mounts, restart policy, labels, image, networks) by making it again
// exactly, with the changes, and swapping it in safely.
//
// Install it as ~/.docker/cli-plugins/docker-edit and run `docker edit`, or
// run docker-edit directly.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/docker-edit/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
