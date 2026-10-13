// Command pushdir watches a folder and copies every change to an rclone
// remote within seconds.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/pushdir/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // a second Ctrl-C ends pushdir at once
	}()
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
