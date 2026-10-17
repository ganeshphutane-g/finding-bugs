package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Doc is a JSON object as the daemon sends it, numbers kept exact.
type Doc = map[string]any

// callDoc decodes a JSON answer with exact numbers.
func (c *Client) callDoc(ctx context.Context, r Request, out any) error {
	resp, err := c.Do(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", r.Method, r.Path, err)
	}
	return nil
}

// InspectContainer reads a container's full description.
func (c *Client) InspectContainer(ctx context.Context, id string) (Doc, error) {
	var out Doc
	err := c.callDoc(ctx, Request{Method: http.MethodGet, Path: "/containers/" + url.PathEscape(id) + "/json"}, &out)
	return out, err
}

// InspectImage reads an image's description (by ID or reference).
func (c *Client) InspectImage(ctx context.Context, ref string) (Doc, error) {
	var out Doc
	err := c.callDoc(ctx, Request{Method: http.MethodGet, Path: "/images/" + url.PathEscape(ref) + "/json"}, &out)
	return out, err
}

// CreateContainer creates a container and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, body Doc) (string, error) {
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	err := c.Call(ctx, Request{Method: http.MethodPost, Path: "/containers/create", Query: q, JSON: body}, &out)
	return out.ID, err
}

// StartContainer starts a container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.Call(ctx, Request{Method: http.MethodPost, Path: "/containers/" + url.PathEscape(id) + "/start"}, nil)
}

// StopContainer stops a container, waiting up to timeout seconds (-1: the
// container's own stop timeout).
func (c *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	q := url.Values{}
	if timeout >= 0 {
		q.Set("t", strconv.Itoa(timeout))
	}
	err := c.Call(ctx, Request{Method: http.MethodPost, Path: "/containers/" + url.PathEscape(id) + "/stop", Query: q}, nil)
	var apiErr *APIError
	if asAPIError(err, &apiErr) && apiErr.StatusCode == http.StatusNotModified {
		return nil // already stopped
	}
	return err
}

// RenameContainer gives a container a new name.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.Call(ctx, Request{Method: http.MethodPost, Path: "/containers/" + url.PathEscape(id) + "/rename", Query: url.Values{"name": {name}}}, nil)
}

// RemoveContainer removes a container (never its volumes).
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.Call(ctx, Request{Method: http.MethodDelete, Path: "/containers/" + url.PathEscape(id), Query: q}, nil)
}

// ConnectNetwork attaches a container to a network.
func (c *Client) ConnectNetwork(ctx context.Context, network, id string, endpoint Doc) error {
	return c.Call(ctx, Request{Method: http.MethodPost, Path: "/networks/" + url.PathEscape(network) + "/connect",
		JSON: Doc{"Container": id, "EndpointConfig": endpoint}}, nil)
}

// Changes lists the paths changed in a container's writable layer.
func (c *Client) Changes(ctx context.Context, id string) ([]Doc, error) {
	var out []Doc
	err := c.callDoc(ctx, Request{Method: http.MethodGet, Path: "/containers/" + url.PathEscape(id) + "/changes"}, &out)
	return out, err
}

// ListContainers lists all containers (running or not).
func (c *Client) ListContainers(ctx context.Context) ([]Doc, error) {
	var out []Doc
	err := c.callDoc(ctx, Request{Method: http.MethodGet, Path: "/containers/json", Query: url.Values{"all": {"1"}}}, &out)
	return out, err
}

func asAPIError(err error, target **APIError) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}
