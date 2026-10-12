// Command bucketmove moves S3 buckets off MinIO (or any S3 server) without
// losing object versions or settings by accident: it shows what is in use,
// checks what the target supports, copies every version in order and
// verifies the result.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ganeshphutane-g/bucketmove/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
