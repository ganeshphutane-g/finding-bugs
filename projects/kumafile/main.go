// Command kumafile keeps an Uptime Kuma configuration in a file.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/kumafile/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
