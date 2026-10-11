package engine

import (
	"context"
	"net/http"
)

// ServerVersion is the subset of GET /version that docker-mop shows.
type ServerVersion struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
}

// Version returns the daemon's version information.
func (c *Client) Version(ctx context.Context) (ServerVersion, error) {
	var v ServerVersion
	err := c.Call(ctx, Request{Method: http.MethodGet, Path: "/version"}, &v)
	return v, err
}

// UsesContainerdStore reports whether the daemon keeps images in the
// containerd image store
// rather than the classic graph driver store. The two prune images
// differently.
func (c *Client) UsesContainerdStore(ctx context.Context) (bool, error) {
	var info struct {
		DriverStatus [][2]string `json:"DriverStatus"`
	}
	if err := c.Call(ctx, Request{Method: http.MethodGet, Path: "/info"}, &info); err != nil {
		return false, err
	}
	for _, kv := range info.DriverStatus {
		if kv[0] == "driver-type" && kv[1] == "io.containerd.snapshotter.v1" {
			return true, nil
		}
	}
	return false, nil
}
