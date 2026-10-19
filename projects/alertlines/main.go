// Command alertlines adds the matching log lines to the alerts Loki's ruler
// sends to Alertmanager.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/alertlines/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
