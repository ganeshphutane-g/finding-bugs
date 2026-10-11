package plan

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

// Outcome is what happened to one planned item.
type Outcome struct {
	Type   string `json:"type"` // container, network, volume, image
	Item   Item   `json:"item"`
	Done   bool   `json:"done"`
	Reason string `json:"reason,omitempty"` // why it was skipped
}

// Result is the outcome of applying a plan.
type Result struct {
	Outcomes []Outcome `json:"outcomes"`
}

// Removed counts the items that were removed (or untagged, for "only
// untagged" images) by type.
func (r Result) Removed(typ string) int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Type == typ && o.Done {
			n++
		}
	}
	return n
}

// Skipped returns the items that were left alone.
func (r Result) Skipped() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if !o.Done {
			out = append(out, o)
		}
	}
	return out
}

// Freed is the space freed by removed items.
func (r Result) Freed() int64 {
	var n int64
	for _, o := range r.Outcomes {
		if o.Done && o.Item.Size > 0 {
			n += o.Item.Size
		}
	}
	return n
}

// errSkip marks a per-item problem: the item is reported and left alone, and
// the run carries on. Any other error (such as losing the daemon) stops it.
type errSkip struct{ reason string }

func (e errSkip) Error() string { return e.reason }

func skip(format string, args ...any) error {
	return errSkip{reason: fmt.Sprintf(format, args...)}
}

// Apply removes exactly the items in p, in Docker's order, and nothing else.
// Nothing is ever forced: an item that is now in use, or that changed since
// the plan was made, is skipped and reported.
func Apply(ctx context.Context, c *engine.Client, p Plan) (Result, error) {
	var res Result
	run := func(typ string, items []Item, remove func(Item) error) error {
		for _, it := range items {
			err := remove(it)
			var s errSkip
			switch {
			case err == nil:
				res.Outcomes = append(res.Outcomes, Outcome{Type: typ, Item: it, Done: true})
			case errors.As(err, &s):
				res.Outcomes = append(res.Outcomes, Outcome{Type: typ, Item: it, Reason: s.reason})
			default:
				return err
			}
		}
		return nil
	}

	if err := run("container", p.Containers, func(it Item) error { return removeContainer(ctx, c, it) }); err != nil {
		return res, err
	}
	if err := run("network", p.Networks, func(it Item) error { return removeNetwork(ctx, c, it) }); err != nil {
		return res, err
	}
	if err := run("volume", p.Volumes, func(it Item) error { return removeVolume(ctx, c, it) }); err != nil {
		return res, err
	}

	// Untagged parent layers go away with the image built on them; handle
	// them last and only check that they are gone.
	var direct, parents []Item
	for _, it := range p.Images {
		if it.Note == noteParentLayer {
			parents = append(parents, it)
		} else {
			direct = append(direct, it)
		}
	}
	if err := run("image", direct, func(it Item) error { return removeImage(ctx, c, it) }); err != nil {
		return res, err
	}
	if err := run("image", parents, func(it Item) error { return checkParentGone(ctx, c, it) }); err != nil {
		return res, err
	}
	return res, nil
}

// classify turns a daemon error into a skip when it is about this one item.
func classify(err error, what string) error {
	if err == nil {
		return nil
	}
	var apiErr *engine.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.StatusCode {
	case http.StatusNotFound:
		return skip("%s no longer exists", what)
	case http.StatusConflict, http.StatusForbidden:
		return skip("%s", apiErr.Message)
	}
	if apiErr.StatusCode < 500 {
		return skip("%s", apiErr.Message)
	}
	return err
}

func removeContainer(ctx context.Context, c *engine.Client, it Item) error {
	var info struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	path := "/containers/" + url.PathEscape(it.ID)
	if err := classify(c.Call(ctx, engine.Request{Method: http.MethodGet, Path: path + "/json"}, &info), "the container"); err != nil {
		return err
	}
	if !removableState(info.State.Status) {
		return skip("the container is now %s", info.State.Status)
	}
	// Like `docker container prune`, keep the container's anonymous volumes;
	// a volume prune handles those.
	return classify(c.Call(ctx, engine.Request{Method: http.MethodDelete, Path: path}, nil), "the container")
}

func removeNetwork(ctx context.Context, c *engine.Client, it Item) error {
	var n engine.Network
	path := "/networks/" + url.PathEscape(it.ID)
	if err := classify(c.Call(ctx, engine.Request{Method: http.MethodGet, Path: path}, &n), "the network"); err != nil {
		return err
	}
	if len(n.Containers) > 0 {
		return skip("the network is now used by a running container")
	}
	return classify(c.Call(ctx, engine.Request{Method: http.MethodDelete, Path: path}, nil), "the network")
}

func removeVolume(ctx context.Context, c *engine.Client, it Item) error {
	var v engine.Volume
	path := "/volumes/" + url.PathEscape(it.ID)
	if err := classify(c.Call(ctx, engine.Request{Method: http.MethodGet, Path: path}, &v), "the volume"); err != nil {
		return err
	}
	// A volume with the same name may have been removed and created again.
	if created, err := time.Parse(time.RFC3339Nano, v.CreatedAt); err == nil && !it.Created.IsZero() && !created.Equal(it.Created) {
		return skip("the volume was recreated after the plan was made")
	}
	return classify(c.Call(ctx, engine.Request{Method: http.MethodDelete, Path: path}, nil), "the volume")
}

// imageRefs is an image's current ID, tags and digest references.
type imageRefs struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
}

func inspectImage(ctx context.Context, c *engine.Client, ref string) (imageRefs, error) {
	var img imageRefs
	err := c.Call(ctx, engine.Request{Method: http.MethodGet, Path: "/images/" + url.PathEscape(ref) + "/json"}, &img)
	return img, err
}

func deleteImageRef(ctx context.Context, c *engine.Client, ref string) error {
	return c.Call(ctx, engine.Request{
		Method: http.MethodDelete,
		Path:   "/images/" + url.PathEscape(ref),
		Query:  url.Values{"force": {"0"}, "noprune": {"0"}},
	}, nil)
}

// imageInUse reports whether any container, running or not, uses the image.
func imageInUse(ctx context.Context, c *engine.Client, id string) (bool, error) {
	var list []engine.Container
	if err := c.Call(ctx, engine.Request{Method: http.MethodGet, Path: "/containers/json", Query: url.Values{"all": {"1"}}}, &list); err != nil {
		return false, err
	}
	for _, ct := range list {
		if ct.ImageID == id {
			return true, nil
		}
	}
	return false, nil
}

// unplannedRefs returns references the image has now that the plan did not
// show. Deleting the image would take them with it.
func unplannedRefs(img imageRefs, it Item) []string {
	planned := map[string]bool{}
	for _, r := range append(append([]string{}, it.Tags...), it.Digests...) {
		planned[r] = true
	}
	var extra []string
	for _, r := range append(append([]string{}, img.RepoTags...), img.RepoDigests...) {
		if r != "<none>:<none>" && r != "<none>@<none>" && !planned[r] {
			extra = append(extra, r)
		}
	}
	return extra
}

// removeImage removes an image by deleting each of its planned tags (an
// image with several tags cannot be deleted by ID without force). Before
// anything is touched it checks that the image is unused and has gained no
// tag or digest since the plan; each tag is also checked to still point at
// the planned image, so a tag moved to a newer image is never touched.
func removeImage(ctx context.Context, c *engine.Client, it Item) error {
	img, err := inspectImage(ctx, c, it.ID)
	if err := classify(err, "the image"); err != nil {
		return err
	}
	if img.ID != it.ID {
		return skip("the image changed after the plan was made")
	}
	if extra := unplannedRefs(img, it); len(extra) > 0 {
		return skip("it was tagged after the plan was made (%s)", strings.Join(extra, ", "))
	}
	if used, err := imageInUse(ctx, c, it.ID); err != nil {
		return err
	} else if used {
		return skip("a container now uses it")
	}

	if len(it.Tags) == 0 {
		// Digest references cannot move to another image, so they are
		// safe to delete as they are. With more than one, Docker refuses
		// to delete by ID without force.
		if len(it.Digests) > 1 {
			for _, d := range it.Digests {
				if err := classify(deleteImageRef(ctx, c, d), "reference "+d); err != nil {
					return err
				}
			}
			if _, err := inspectImage(ctx, c, it.ID); engine.IsNotFound(err) {
				return nil
			}
		}
		return deleteByIDIfUnchanged(ctx, c, it)
	}

	removedAny := false
	for _, tag := range it.Tags {
		current, err := inspectImage(ctx, c, tag)
		if engine.IsNotFound(err) {
			continue
		}
		if err := classify(err, "tag "+tag); err != nil {
			return err
		}
		if current.ID != it.ID {
			continue // the tag now names a different image; leave it alone
		}
		if err := classify(deleteImageRef(ctx, c, tag), "tag "+tag); err != nil {
			var s errSkip
			if removedAny && errors.As(err, &s) {
				return skip("partly untagged: %s", s.reason)
			}
			return err
		}
		removedAny = true
	}
	if !removedAny {
		return skip("its tags now point to other images")
	}
	if it.Note != "" {
		return nil // only untagged, as planned
	}
	// After the last tag goes the image is normally gone. If it is still
	// there (it also had digest references), delete it by ID, but only if it
	// has not gained a reference meanwhile.
	if _, err := inspectImage(ctx, c, it.ID); engine.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	return deleteByIDIfUnchanged(ctx, c, it)
}

// deleteByIDIfUnchanged deletes an image by ID after checking once more that
// it has no references the plan did not show. Deleting by ID removes the
// image's last tag too, so this check is what keeps a newly added tag safe.
func deleteByIDIfUnchanged(ctx context.Context, c *engine.Client, it Item) error {
	img, err := inspectImage(ctx, c, it.ID)
	if engine.IsNotFound(err) {
		return nil
	}
	if err := classify(err, "the image"); err != nil {
		return err
	}
	if extra := unplannedRefs(img, it); len(extra) > 0 {
		return skip("it was tagged after the plan was made (%s)", strings.Join(extra, ", "))
	}
	return classify(deleteImageRef(ctx, c, it.ID), "the image")
}

func checkParentGone(ctx context.Context, c *engine.Client, it Item) error {
	_, err := inspectImage(ctx, c, it.ID)
	if engine.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return skip("still needed by another image")
}
