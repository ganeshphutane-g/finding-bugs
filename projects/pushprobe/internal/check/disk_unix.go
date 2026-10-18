//go:build linux || darwin || freebsd

package check

import (
	"context"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type diskCheck struct{ c config.Disk }

func (d diskCheck) Run(ctx context.Context) Result {
	start := time.Now()
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.c.Path, &st); err != nil {
		return fail(start, "%s: %v", d.c.Path, err)
	}
	size := uint64(st.Blocks) * uint64(st.Bsize)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	// Used as df counts it: of what an ordinary user can use.
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * uint64(st.Bsize)
	if used+free == 0 {
		return fail(start, "%s: the file system reports no size", d.c.Path)
	}
	percent := float64(used) * 100 / float64(used+free)
	return usage(start, d.c.Path, percent, d.c.MaxUsedPercent, free, size)
}
