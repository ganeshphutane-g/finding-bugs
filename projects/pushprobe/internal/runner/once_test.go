package runner

import (
	"context"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
)

// flaky fails its first n runs.
type flaky struct {
	n, runs int
	slow    bool
}

func (f *flaky) Run(ctx context.Context) check.Result {
	f.runs++
	if f.slow {
		<-ctx.Done()
		return check.Result{Message: "timed out"}
	}
	return check.Result{OK: f.runs > f.n}
}

func TestOnce(t *testing.T) {
	RetryPause = time.Millisecond
	ctx := context.Background()
	if r, n := Once(ctx, &flaky{n: 1}, time.Second, 1); !r.OK || n != 2 {
		t.Errorf("one retry: %v %d", r.OK, n)
	}
	if r, n := Once(ctx, &flaky{n: 5}, time.Second, 2); r.OK || n != 3 {
		t.Errorf("all failed: %v %d", r.OK, n)
	}
	if r, n := Once(ctx, &flaky{}, time.Second, 3); !r.OK || n != 1 {
		t.Errorf("first try: %v %d", r.OK, n)
	}
	start := time.Now()
	if r, n := Once(ctx, &flaky{slow: true}, 50*time.Millisecond, 0); r.OK || n != 1 || time.Since(start) > time.Second {
		t.Errorf("timeout: %v %d %s", r.OK, n, time.Since(start))
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, n := Once(cctx, &flaky{n: 5}, time.Second, 5); n != 1 {
		t.Errorf("cancelled: %d attempts", n)
	}
}
