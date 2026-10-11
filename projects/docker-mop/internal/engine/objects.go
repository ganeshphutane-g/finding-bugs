package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Container is the part of a container summary docker-mop uses.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	SizeRw  int64             `json:"SizeRw"`
	Mounts  []struct {
		Type string `json:"Type"`
		Name string `json:"Name"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			NetworkID string `json:"NetworkID"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Name returns the container's primary name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return ""
	}
	n := c.Names[0]
	if len(n) > 0 && n[0] == '/' {
		n = n[1:]
	}
	return n
}

// Image is the part of an image summary docker-mop uses.
type Image struct {
	ID         string            `json:"Id"`
	RepoTags   []string          `json:"RepoTags"`
	ParentID   string            `json:"ParentId"`
	Created    int64             `json:"Created"`
	Size       int64             `json:"Size"`
	SharedSize int64             `json:"SharedSize"`
	Labels     map[string]string `json:"Labels"`
	Containers int64             `json:"Containers"`
}

// Tags returns the image's real tags, dropping the "<none>:<none>" placeholder.
func (i Image) Tags() []string {
	var out []string
	for _, t := range i.RepoTags {
		if t != "<none>:<none>" {
			out = append(out, t)
		}
	}
	return out
}

// Volume is the part of a volume docker-mop uses.
type Volume struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	CreatedAt string            `json:"CreatedAt"`
	Labels    map[string]string `json:"Labels"`
	Options   map[string]string `json:"Options"`
	Scope     string            `json:"Scope"`
	UsageData *struct {
		Size     int64 `json:"Size"`
		RefCount int64 `json:"RefCount"`
	} `json:"UsageData"`
}

// Size returns the volume's disk usage, or -1 when the daemon did not report it.
func (v Volume) Size() int64 {
	if v.UsageData == nil {
		return -1
	}
	return v.UsageData.Size
}

// Network is the part of a network docker-mop uses.
type Network struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Created    time.Time         `json:"Created"`
	Labels     map[string]string `json:"Labels"`
	ConfigOnly bool              `json:"ConfigOnly"`
	// Containers holds the network's active endpoints (running containers).
	// The list endpoint leaves it empty; InspectNetwork fills it.
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
}

// DiskUsage is the daemon's view of containers, images and volumes, with sizes.
type DiskUsage struct {
	Containers []Container
	Images     []Image
	Volumes    []Volume
}

// DiskUsage calls GET /system/df. It reads both the classic response lists and
// the *Usage.Items form that newer daemons also return.
func (c *Client) DiskUsage(ctx context.Context) (DiskUsage, error) {
	var raw struct {
		Containers     []Container `json:"Containers"`
		Images         []Image     `json:"Images"`
		Volumes        []Volume    `json:"Volumes"`
		ContainerUsage *struct {
			Items []Container `json:"Items"`
		} `json:"ContainerUsage"`
		ImageUsage *struct {
			Items []Image `json:"Items"`
		} `json:"ImageUsage"`
		VolumeUsage *struct {
			Items []Volume `json:"Items"`
		} `json:"VolumeUsage"`
	}
	if err := c.Call(ctx, Request{Method: http.MethodGet, Path: "/system/df"}, &raw); err != nil {
		return DiskUsage{}, err
	}
	du := DiskUsage{Containers: raw.Containers, Images: raw.Images, Volumes: raw.Volumes}
	if du.Containers == nil && raw.ContainerUsage != nil {
		du.Containers = raw.ContainerUsage.Items
	}
	if du.Images == nil && raw.ImageUsage != nil {
		du.Images = raw.ImageUsage.Items
	}
	if du.Volumes == nil && raw.VolumeUsage != nil {
		du.Volumes = raw.VolumeUsage.Items
	}
	return du, nil
}

// Images lists every image, including the untagged parent images the classic
// builder leaves behind (which /system/df leaves out), with shared sizes.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var imgs []Image
	err := c.Call(ctx, Request{
		Method: http.MethodGet,
		Path:   "/images/json",
		Query:  url.Values{"all": {"1"}, "shared-size": {"1"}},
	}, &imgs)
	return imgs, err
}

// DanglingImageIDs asks the daemon which images it considers dangling, so
// docker-mop uses exactly the daemon's own definition.
func (c *Client) DanglingImageIDs(ctx context.Context) (map[string]bool, error) {
	filters, _ := json.Marshal(map[string][]string{"dangling": {"true"}})
	var imgs []Image
	err := c.Call(ctx, Request{
		Method: http.MethodGet,
		Path:   "/images/json",
		Query:  url.Values{"filters": {string(filters)}},
	}, &imgs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(imgs))
	for _, img := range imgs {
		out[img.ID] = true
	}
	return out, nil
}

// Networks lists networks and inspects each one to learn its active endpoints.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var list []Network
	if err := c.Call(ctx, Request{Method: http.MethodGet, Path: "/networks"}, &list); err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(list))
	for _, n := range list {
		var full Network
		err := c.Call(ctx, Request{Method: http.MethodGet, Path: "/networks/" + url.PathEscape(n.ID)}, &full)
		if IsNotFound(err) {
			continue // removed while we were looking
		}
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}
