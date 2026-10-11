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
