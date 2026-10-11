// Package plan works out what a Docker prune would remove, without removing
// anything. It follows the rules in Docker's own prune code so the plan
// matches what `docker ... prune` would delete on the same state.
package plan

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

// Kind is what to prune.
type Kind string

const (
	System     Kind = "system"
	Containers Kind = "containers"
	Images     Kind = "images"
	Volumes    Kind = "volumes"
	Networks   Kind = "networks"
)

// ParseKind accepts the plural names and the singular forms used by docker's
// own commands (`docker container prune`, `docker image prune`, ...).
func ParseKind(s string) (Kind, error) {
	switch strings.ToLower(s) {
	case "system", "all":
		return System, nil
	case "containers", "container":
		return Containers, nil
	case "images", "image":
		return Images, nil
	case "volumes", "volume":
		return Volumes, nil
	case "networks", "network":
		return Networks, nil
	}
	return "", fmt.Errorf("unknown prune target %q (use system, containers, images, volumes or networks)", s)
}

// anonymousVolumeLabel marks volumes Docker created without a name.
const anonymousVolumeLabel = "com.docker.volume.anonymous"

// Options are the prune flags.
type Options struct {
	Kind Kind
	// All: images prune removes every unused image, not just dangling ones;
	// volumes prune removes named volumes too. For system it only affects
	// images, as in Docker.
	All bool
	// Volumes: system prune also removes anonymous volumes.
	Volumes bool
	Filters Filters
	Now     time.Time
}

// State is a snapshot of the daemon's objects.
type State struct {
	Containers []engine.Container
	Images     []engine.Image
	Dangling   map[string]bool
	Volumes    []engine.Volume
	Networks   []engine.Network
	// ContainerdStore is true when the daemon uses the containerd image
	// store, which prunes each image on its own and ignores parent images.
	ContainerdStore bool
	// DaemonNow is the daemon's clock and time zone when the state was read;
	// "until" filters are evaluated against it, as Docker does.
	DaemonNow time.Time
	// APIVersion is the daemon's Engine API version. Before 1.42, volume
	// prune also removes named volumes.
	APIVersion string
}

// Item is one object the prune would remove.
type Item struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"` // bytes freed; -1 when unknown
	Created time.Time `json:"created"`
	Note    string    `json:"note,omitempty"`
	// Tags and Digests are set for images; prune deletes images through them.
	Tags    []string `json:"tags,omitempty"`
	Digests []string `json:"digests,omitempty"`
}

// Notes on image items that change how they are removed.
const (
	noteUntagOnly   = "only untagged: other images are built on it"
	noteParentLayer = "untagged parent layer"
)

// Plan is the result: objects in the order Docker would remove them.
type Plan struct {
	Kind       Kind     `json:"kind"`
	Containers []Item   `json:"containers"`
	Networks   []Item   `json:"networks"`
	Volumes    []Item   `json:"volumes"`
	Images     []Item   `json:"images"`
	Notes      []string `json:"notes,omitempty"`
}

// Count is the number of objects in the plan.
func (p Plan) Count() int {
	return len(p.Containers) + len(p.Networks) + len(p.Volumes) + len(p.Images)
}

// Total is the space the plan frees. AtLeast is true when some sizes are
// lower bounds (shared image layers) or unknown.
func (p Plan) Total() (bytes int64, atLeast bool) {
	for _, group := range [][]Item{p.Containers, p.Volumes, p.Images} {
		for _, it := range group {
			if it.Size < 0 {
				atLeast = true
				continue
			}
			bytes += it.Size
		}
	}
	if len(p.Images) > 0 {
		atLeast = true
	}
	return bytes, atLeast
}

// Compute works out the plan for opts on state.
func Compute(state State, opts Options) (Plan, error) {
	if opts.Filters == nil {
		opts.Filters = Filters{}
	}
	if opts.Now.IsZero() {
		opts.Now = state.DaemonNow
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	p := Plan{Kind: opts.Kind}
	f := opts.Filters

	until, err := f.Until(opts.Now)
	if err != nil {
		return p, err
	}

	var remaining []engine.Container
	switch opts.Kind {
	case Containers:
		if err := f.Validate("until", "label", "label!"); err != nil {
			return p, err
		}
		remove, _ := planContainers(state.Containers, f, until)
		p.Containers = containerItems(remove)
	case Networks:
		if err := f.Validate("until", "label", "label!"); err != nil {
			return p, err
		}
		p.Networks, p.Notes = planNetworks(state.Networks, state.Containers, f, until)
	case Volumes:
		if err := f.Validate("label", "label!", "all"); err != nil {
			return p, err
		}
		all, err := boolFilter(f, "all", opts.All || namedVolumesPruned(state))
		if err != nil {
			return p, err
		}
		p.Volumes = planVolumes(state.Volumes, state.Containers, f, all)
	case Images:
		if err := f.Validate("until", "label", "label!", "dangling"); err != nil {
			return p, err
		}
		all := opts.All
		if f.Contains("dangling") {
			dangling, err := boolFilter(f, "dangling", true)
			if err != nil {
				return p, err
			}
			all = all || !dangling
		}
		p.Images = planImages(state, state.Containers, f, until, all)
	case System:
		if err := f.Validate("until", "label", "label!"); err != nil {
			return p, err
		}
		if opts.Volumes && f.Contains("until") {
			return p, fmt.Errorf(`the "until" filter is not supported with "--volumes"`)
		}
		var remove []engine.Container
		remove, remaining = planContainers(state.Containers, f, until)
		p.Containers = containerItems(remove)
		var notes []string
		p.Networks, notes = planNetworks(state.Networks, remaining, f, until)
		p.Notes = append(p.Notes, notes...)
		if opts.Volumes {
			p.Volumes = planVolumes(state.Volumes, remaining, f, namedVolumesPruned(state))
		}
		p.Images = planImages(state, remaining, f, until, opts.All)
		p.Notes = append(p.Notes, "Build cache is not included. Docker cannot preview it; use 'docker builder prune' for that.")
	default:
		return p, fmt.Errorf("unknown prune target %q", opts.Kind)
	}
	return p, nil
}

// namedVolumesPruned is true for daemons older than API 1.42 (Docker 23),
// whose volume prune removes named volumes too.
func namedVolumesPruned(state State) bool {
	return state.APIVersion != "" && engine.CompareVersions(state.APIVersion, "1.42") < 0
}

// boolFilter reads a true/false filter such as dangling=false or all=true.
func boolFilter(f Filters, key string, def bool) (bool, error) {
	values := f.Get(key)
	if len(values) == 0 {
		return def, nil
	}
	if len(values) > 1 {
		return false, fmt.Errorf("invalid filter '%s': only one value allowed", key)
	}
	switch strings.ToLower(values[0]) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("invalid filter '%s=%s'", key, values[0])
}

// planContainers returns stopped containers to remove and the containers
// that remain afterwards.
func planContainers(all []engine.Container, f Filters, until time.Time) (remove, remaining []engine.Container) {
	for _, c := range all {
		if removableState(c.State) &&
			(until.IsZero() || !time.Unix(c.Created, 0).After(until)) &&
			f.MatchLabels(c.Labels) {
			remove = append(remove, c)
		} else {
			remaining = append(remaining, c)
		}
	}
	return remove, remaining
}

// removableState is Docker's "not running" check: running, paused and
// restarting containers are kept.
func removableState(state string) bool {
	switch state {
	case "created", "exited", "dead":
		return true
	}
	return false
}

func containerItems(cs []engine.Container) []Item {
	items := make([]Item, 0, len(cs))
	for _, c := range cs {
		items = append(items, Item{
			ID:      c.ID,
			Name:    c.Name(),
			Size:    c.SizeRw,
			Created: time.Unix(c.Created, 0),
			Note:    c.State,
		})
	}
	return items
}

var predefinedNetworks = map[string]bool{"bridge": true, "host": true, "none": true}

func planNetworks(all []engine.Network, containers []engine.Container, f Filters, until time.Time) ([]Item, []string) {
	// Stopped containers still refer to networks, but Docker removes those
	// networks anyway. Point that out so it is not a surprise.
	stoppedUsers := map[string][]string{}
	for _, c := range containers {
		if c.State == "running" || c.State == "paused" || c.State == "restarting" {
			continue
		}
		for name, n := range c.NetworkSettings.Networks {
			key := n.NetworkID
			if key == "" {
				key = "name:" + name
			}
			stoppedUsers[key] = append(stoppedUsers[key], c.Name())
		}
	}

	var items []Item
	var notes []string
	for _, n := range all {
		// Docker's prune keeps config-only networks (templates for
		// --config-from) and leaves swarm networks to the swarm.
		if n.ConfigOnly || n.Scope == "swarm" || predefinedNetworks[n.Name] || len(n.Containers) > 0 {
			continue
		}
		if !until.IsZero() && n.Created.After(until) {
			continue
		}
		if !f.MatchLabels(n.Labels) {
			continue
		}
		it := Item{ID: n.ID, Name: n.Name, Size: 0, Created: n.Created}
		users := append(stoppedUsers[n.ID], stoppedUsers["name:"+n.Name]...)
		if len(users) > 0 {
			sort.Strings(users)
			it.Note = "used by stopped " + plural(len(users), "container") + ": " + strings.Join(users, ", ")
			notes = append(notes, fmt.Sprintf("Network %q is used by stopped %s (%s). Docker removes it anyway, and those containers will not start until it is recreated.",
				n.Name, plural(len(users), "container"), strings.Join(users, ", ")))
		}
		items = append(items, it)
	}
	return items, notes
}

func planVolumes(all []engine.Volume, containers []engine.Container, f Filters, includeNamed bool) []Item {
	inUse := map[string]bool{}
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				inUse[m.Name] = true
			}
		}
	}

	var items []Item
	for _, v := range all {
		if inUse[v.Name] {
			continue
		}
		_, anonymous := v.Labels[anonymousVolumeLabel]
		if !includeNamed && !anonymous {
			continue
		}
		if !f.MatchLabels(v.Labels) {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, v.CreatedAt)
		note := "named"
		if anonymous {
			note = "anonymous"
		}
		items = append(items, Item{ID: v.Name, Name: v.Name, Size: v.Size(), Created: created, Note: note})
	}
	return items
}

func planImages(state State, containers []engine.Container, f Filters, until time.Time, all bool) []Item {
	used := map[string]bool{}
	for _, c := range containers {
		used[c.ImageID] = true
	}
	// The containerd image store ignores parent links, so only the classic
	// store needs them.
	children := map[string][]string{}
	if !state.ContainerdStore {
		for _, img := range state.Images {
			if img.ParentID != "" {
				children[img.ParentID] = append(children[img.ParentID], img.ID)
			}
		}
	}

	candidate := map[string]bool{}
	for _, img := range state.Images {
		if !all && !state.Dangling[img.ID] {
			continue
		}
		// Untagged intermediate images (classic builder) are only removed
		// together with the image built on top of them.
		if len(img.Tags()) == 0 && len(children[img.ID]) > 0 {
			continue
		}
		// Docker skips images without a creation time when "until" is set.
		if !until.IsZero() && (img.Created == 0 || time.Unix(img.Created, 0).After(until)) {
			continue
		}
		if !f.MatchLabels(img.Labels) || used[img.ID] {
			continue
		}
		candidate[img.ID] = true
	}

	// An image is really deleted only when every image built on it is deleted
	// too; a tagged candidate with a surviving child is just untagged. Docker
	// also deletes untagged, unused parents once nothing else needs them,
	// whatever the filters say. Start from everything that could go and drop
	// images with a surviving child until nothing changes.
	gone := map[string]bool{}
	for _, img := range state.Images {
		intermediate := len(img.Tags()) == 0 && len(img.Digests()) == 0 && len(children[img.ID]) > 0
		if candidate[img.ID] || (intermediate && !used[img.ID]) {
			gone[img.ID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id := range gone {
			for _, c := range children[id] {
				if !gone[c] {
					delete(gone, id)
					changed = true
					break
				}
			}
		}
	}

	var items []Item
	for _, img := range state.Images {
		switch {
		case candidate[img.ID] && gone[img.ID]:
			items = append(items, imageItem(img))
		case candidate[img.ID]:
			it := imageItem(img)
			it.Size = 0
			it.Note = noteUntagOnly
			items = append(items, it)
		case gone[img.ID] && feedsRemovedCandidate(img.ID, children, gone, candidate):
			it := imageItem(img)
			it.Note = noteParentLayer
			items = append(items, it)
		}
	}
	return items
}

// feedsRemovedCandidate reports whether id is a parent (directly or through
// other removed parents) of a removed candidate. That is how Docker reaches
// untagged parent layers.
func feedsRemovedCandidate(id string, children map[string][]string, gone, candidate map[string]bool) bool {
	for _, c := range children[id] {
		if gone[c] && (candidate[c] || feedsRemovedCandidate(c, children, gone, candidate)) {
			return true
		}
	}
	return false
}

func imageItem(img engine.Image) Item {
	name := "<none>"
	if tags := img.Tags(); len(tags) > 0 {
		name = strings.Join(tags, ", ")
	}
	size := img.Size
	if img.SharedSize > 0 {
		size -= img.SharedSize
	}
	return Item{ID: img.ID, Name: name, Size: size, Created: time.Unix(img.Created, 0), Tags: img.Tags(), Digests: img.Digests()}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
