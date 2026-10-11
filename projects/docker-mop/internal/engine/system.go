package engine

import (
	"context"
	"net/http"
	"time"
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

// Info is the part of GET /info docker-mop uses.
type Info struct {
	// ContainerdStore is true when images live in the containerd image store
	// rather than the classic graph driver store. The two prune images
	// differently.
	ContainerdStore bool
	// SystemTime is the daemon's clock, in the daemon's time zone. Docker
	// evaluates "until" filters against it.
	SystemTime time.Time
}

// Info returns facts about the daemon that change how prunes behave.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var raw struct {
		DriverStatus [][2]string `json:"DriverStatus"`
		SystemTime   string      `json:"SystemTime"`
	}
	if err := c.Call(ctx, Request{Method: http.MethodGet, Path: "/info"}, &raw); err != nil {
		return Info{}, err
	}
	var info Info
	for _, kv := range raw.DriverStatus {
		if kv[0] == "driver-type" && kv[1] == "io.containerd.snapshotter.v1" {
			info.ContainerdStore = true
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.SystemTime); err == nil {
		info.SystemTime = t
	}
	return info, nil
}
