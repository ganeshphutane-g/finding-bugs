// Command pushprobe runs checks where your monitoring cannot reach and pushes
// the results to Uptime Kuma and Gatus.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/pushprobe/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
