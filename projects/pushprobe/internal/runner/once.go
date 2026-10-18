// Package runner runs checks with their timeouts and retries.
package runner

import (
	"context"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
)

// RetryPause is the pause between a failed attempt and the next.
var RetryPause = 2 * time.Second

// Once runs a check, each attempt within timeout, retrying a failure up to
// retries times. The result is the first success or the last failure. It
// returns early, with the last result so far, when ctx ends.
func Once(ctx context.Context, c check.Checker, timeout time.Duration, retries int) (r check.Result, attempts int) {
	for {
		attempts++
		actx, cancel := context.WithTimeout(ctx, timeout)
		r = c.Run(actx)
		cancel()
		if r.OK || attempts > retries || ctx.Err() != nil {
			return r, attempts
		}
		select {
		case <-ctx.Done():
			return r, attempts
		case <-time.After(RetryPause):
		}
	}
}
