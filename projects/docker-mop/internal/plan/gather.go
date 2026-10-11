package plan

import (
	"context"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

// Gather reads the daemon state a plan for kind needs.
func Gather(ctx context.Context, c *engine.Client, kind Kind) (State, error) {
	var st State
	du, err := c.DiskUsage(ctx)
	if err != nil {
		return st, err
	}
	st.Containers, st.Volumes = du.Containers, du.Volumes
	info, err := c.Info(ctx)
	if err != nil {
		return st, err
	}
	st.ContainerdStore = info.ContainerdStore
	st.DaemonNow = info.SystemTime
	if st.APIVersion, err = c.APIVersion(ctx); err != nil {
		return st, err
	}
	if kind == System || kind == Images {
		if st.Images, err = c.Images(ctx); err != nil {
			return st, err
		}
		if st.Dangling, err = c.DanglingImageIDs(ctx); err != nil {
			return st, err
		}
	}
	if kind == System || kind == Networks {
		if st.Networks, err = c.Networks(ctx); err != nil {
			return st, err
		}
	}
	return st, nil
}
