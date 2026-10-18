//go:build !(linux || darwin || freebsd)

package check

import (
	"context"
	"runtime"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type diskCheck struct{ c config.Disk }

func (d diskCheck) Run(ctx context.Context) Result {
	return fail(time.Now(), "disk checks are not supported on %s yet", runtime.GOOS)
}
