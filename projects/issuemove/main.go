// Command issuemove moves issues between Gitea or Forgejo repositories.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/issuemove/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
