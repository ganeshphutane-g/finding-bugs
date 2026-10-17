package recreate

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ganeshphutane-g/docker-edit/internal/engine"
)

// SwapOptions control a swap.
type SwapOptions struct {
	KeepOld bool               // keep the old container (renamed) after success
	Log     func(string)       // progress
	Now     func() time.Time   // for the backup name
	Started func(string) error // checks the new container once started (nil: none)
}

// Swap replaces the container described by old with one created from spec:
// the old one is stopped and renamed, the new one created under its name
// and started (if the old one was running). On any failure the new one is
// removed and the old one gets its name back and is started again.
func Swap(ctx context.Context, c *engine.Client, old Doc, spec *Spec, o SwapOptions) (newID string, err error) {
	log := func(f string, a ...any) {
		if o.Log != nil {
			o.Log(fmt.Sprintf(f, a...))
		}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	api, err := c.APIVersion(ctx)
	if err != nil {
		return "", err
	}
	oldID := str(old["Id"])
	running := asDoc(old["State"])["Running"] == true
	backup := fmt.Sprintf("%s.old-%s", spec.Name, now().UTC().Format("20060102-150405"))

	// Undo, step by step, whatever was done. It runs on a fresh context:
	// the caller's may be cancelled (Ctrl-C) by then.
	var undo []func(context.Context) error
	rollback := func(cause error) error {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for i := len(undo) - 1; i >= 0; i-- {
			if uerr := undo[i](rctx); uerr != nil {
				// (Not wrapping the cause: this is no plain interruption.)
				return fmt.Errorf("%v; putting the old container back also failed: %v (it is %s, stopped)", cause, uerr, backup)
			}
		}
		log("put the old container back")
		return cause
	}

	// Each undo is registered before its step and copes with the step not
	// having happened: a cancelled call may still have been carried out.
	if running {
		undo = append(undo, func(ctx context.Context) error { return c.StartContainer(ctx, oldID) })
		log("stopping %s", spec.Name)
		// The daemon goes on stopping when the call is cancelled, so the
		// stop is not cancelled: the rollback then starts it again.
		if err := c.StopContainer(context.WithoutCancel(ctx), oldID, -1); err != nil {
			return "", rollback(fmt.Errorf("stopping the container: %w", err))
		}
		if err := ctx.Err(); err != nil {
			return "", rollback(err)
		}
	}
	undo = append(undo, func(ctx context.Context) error {
		d, err := c.InspectContainer(ctx, oldID)
		if err != nil || str(d["Name"]) == "/"+spec.Name {
			return err
		}
		return c.RenameContainer(ctx, oldID, spec.Name)
	})
	if err := c.RenameContainer(ctx, oldID, backup); err != nil {
		return "", rollback(fmt.Errorf("renaming the container: %w", err))
	}

	undo = append(undo, func(ctx context.Context) error {
		id := newID
		if id == "" {
			// The create call failed, but may have made the container.
			d, err := c.InspectContainer(ctx, spec.Name)
			if engine.IsNotFound(err) {
				return nil
			} else if err != nil {
				return err
			}
			if id = str(d["Id"]); id == oldID {
				return nil
			}
		}
		return c.RemoveContainer(ctx, id, true)
	})
	log("creating the new %s", spec.Name)
	newID, err = c.CreateContainer(ctx, spec.Name, spec.Body(api))
	if err != nil {
		newID = ""
		return "", rollback(fmt.Errorf("creating the new container: %w", err))
	}
	for _, ep := range spec.Later(api) {
		if err := c.ConnectNetwork(ctx, ep.Network, newID, ep.Config); err != nil {
			return "", rollback(fmt.Errorf("connecting the new container to %s: %w", ep.Network, err))
		}
	}
	if running {
		log("starting the new %s", spec.Name)
		if err := c.StartContainer(ctx, newID); err != nil {
			return "", rollback(fmt.Errorf("starting the new container: %w", err))
		}
		if o.Started != nil {
			if err := o.Started(newID); err != nil {
				return "", rollback(err)
			}
		}
	}
	if o.KeepOld {
		// A kept backup must not start again when Docker restarts.
		if err := c.Call(ctx, engine.Request{Method: http.MethodPost, Path: "/containers/" + url.PathEscape(oldID) + "/update",
			JSON: Doc{"RestartPolicy": Doc{"Name": "no"}}}, nil); err != nil {
			log("warning: the old container %s keeps its restart policy, so it may start again when Docker restarts: %v", backup, err)
		}
		log("kept the old container as %s (stopped, never restarted)", backup)
		return newID, nil
	}
	if err := c.RemoveContainer(ctx, oldID, false); err != nil {
		log("the old container is kept as %s: removing it failed: %v", backup, err)
	}
	return newID, nil
}
