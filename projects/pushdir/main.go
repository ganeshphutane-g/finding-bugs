// Command pushdir watches a folder and copies every change to an rclone
// remote within seconds.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/pushdir/internal/cli"
	"github.com/ganeshphutane-g/pushdir/internal/rclone"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel() // let a running rclone finish cleanly
		<-sigs
		fmt.Fprintln(os.Stderr, "pushdir: stopping at once")
		rclone.KillAll()
		os.Exit(130)
	}()
	os.Exit(cli.Run(ctx, cli.DefaultEnv(), os.Args[1:]))
}
