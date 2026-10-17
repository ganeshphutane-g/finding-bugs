// Package cli implements the docker-edit command line (also run by the
// docker CLI as the plugin "docker edit").
package cli

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/docker-edit/internal/edit"
	"github.com/ganeshphutane-g/docker-edit/internal/engine"
	"github.com/ganeshphutane-g/docker-edit/internal/recreate"
)

// Version is set at build time.
var Version = "dev"

const pluginName = "edit"

const metadataCommand = "docker-cli-plugin-metadata"

// Env is the outside world the command sees.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	NewClient      func(engine.Endpoint) (*engine.Client, error)
}

// DefaultEnv uses the process environment.
func DefaultEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, NewClient: engine.New}
}

var errUsage = errors.New("usage")

func usagef(f string, a ...any) error { return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(f, a...)) }

// Run runs docker-edit and returns the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "docker edit: %s\nRun 'docker edit --help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "docker edit: interrupted")
		return 130
	default:
		fmt.Fprintf(env.Stderr, "docker edit: %v\n", err)
		return 1
	}
}

const usage = `Usage: docker edit [OPTIONS] CONTAINER

Change an existing container: its ports, environment, mounts, restart
policy, labels, image or networks. Docker cannot change these in place, so
docker edit makes the container again exactly as it was, with only the
changes, and swaps it in: the old one is stopped and kept until the new one
runs, and put back if anything fails. Volumes and their data stay.

Changes (each can be given several times):
  -p, --publish [IP:]HOST:CONTAINER[/PROTO]   Publish a port
      --unpublish CONTAINER[/PROTO]           Stop publishing a port
  -e, --env NAME=VALUE                        Set a variable
      --unset-env NAME                        Remove a variable
  -v, --volume SOURCE:/PATH[:OPTIONS]         Mount a folder or volume
      --mount type=...,source=...,target=...  Mount (docker run --mount syntax)
      --unmount /PATH                         Remove what is mounted there
      --restart POLICY                        no, always, unless-stopped, on-failure[:N]
  -l, --label NAME=VALUE                      Set a label
      --unlabel NAME                          Remove a label
      --image IMAGE                           Use another image (or tag)
      --network NAME[:ALIAS,...]              Connect to a network
      --disconnect NAME                       Leave a network

Options:
      --dry-run           Show the changes and stop
  -y, --yes               Do not ask
      --keep-old          Keep the old container (stopped, renamed) afterwards
      --wait DURATION     How long the new container must run, or become
                          healthy, before the swap counts (default 30s)
      --discard-changes   Go ahead although files were changed inside the
                          container (outside its volumes), which are lost

Exit codes: 0 done (or nothing to change), 1 error or refused, 2 bad usage,
130 interrupted (the old container is put back).
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	changes        edit.Changes
	dryRun, yes    bool
	keepOld        bool
	discardChanges bool
	wait           time.Duration
	help           bool
	container      string
}

func parse(args []string) (options, error) {
	var o options
	c := &o.changes
	fs := flag.NewFlagSet("docker edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range []string{"p", "publish"} {
		fs.Var((*multi)(&c.Publish), name, "")
	}
	fs.Var((*multi)(&c.Unpublish), "unpublish", "")
	for _, name := range []string{"e", "env"} {
		fs.Var((*multi)(&c.Env), name, "")
	}
	fs.Var((*multi)(&c.UnsetEnv), "unset-env", "")
	for _, name := range []string{"v", "volume"} {
		fs.Var((*multi)(&c.Volumes), name, "")
	}
	fs.Var((*multi)(&c.Mounts), "mount", "")
	fs.Var((*multi)(&c.Unmount), "unmount", "")
	fs.StringVar(&c.Restart, "restart", "", "")
	for _, name := range []string{"l", "label"} {
		fs.Var((*multi)(&c.Labels), name, "")
	}
	fs.Var((*multi)(&c.Unlabel), "unlabel", "")
	fs.StringVar(&c.Image, "image", "", "")
	fs.Var((*multi)(&c.Networks), "network", "")
	fs.Var((*multi)(&c.Disconnect), "disconnect", "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.keepOld, "keep-old", false, "")
	fs.BoolVar(&o.discardChanges, "discard-changes", false, "")
	fs.DurationVar(&o.wait, "wait", 30*time.Second, "")
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); errors.Is(err, flag.ErrHelp) {
			o.help = true
			return o, nil
		} else if err != nil {
			return o, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) != 1 {
		return o, usagef("name one container")
	}
	o.container = pos[0]
	if o.wait < 0 {
		return o, usagef("--wait must not be negative")
	}
	return o, nil
}

func run(ctx context.Context, env Env, args []string) error {
	if len(args) > 0 && args[0] == metadataCommand {
		return json.NewEncoder(env.Stdout).Encode(map[string]string{
			"SchemaVersion": "0.1.0", "Vendor": "Ganesh Phutane", "Version": Version,
			"ShortDescription": "Change a container's ports, env, mounts, restart, labels, image or networks",
			"URL":              "https://github.com/ganeshphutane-g/docker-edit",
		})
	}
	// Called by the docker CLI: docker's global flags, then the plugin name.
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && contains(args, pluginName) {
		var err error
		if args, env, err = globalFlags(args, env); err != nil {
			return err
		}
	}
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(env.Stdout, "docker-edit %s\n", Version)
		return nil
	}
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	o, err := parse(args)
	if err != nil {
		return err
	}
	if o.help {
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	ep, err := engine.ResolveEndpoint(env.Getenv)
	if err != nil {
		return err
	}
	c, err := env.NewClient(ep)
	if err != nil {
		return err
	}
	return editContainer(ctx, env, c, o)
}

func editContainer(ctx context.Context, env Env, c *engine.Client, o options) error {
	out := func(f string, a ...any) { fmt.Fprintf(env.Stdout, f+"\n", a...) }
	ctr, err := recreate.Read(ctx, c, o.container)
	if engine.IsNotFound(err) {
		return fmt.Errorf("no container %q", o.container)
	}
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(str(ctr["Name"]), "/")
	id := str(ctr["Id"])
	cfg := asDoc(ctr["Config"])
	host := asDoc(ctr["HostConfig"])
	state := asDoc(ctr["State"])

	// What docker edit will not do.
	if p := str(asDoc(cfg["Labels"])["com.docker.compose.project"]); p != "" {
		return fmt.Errorf("%s belongs to the Compose project %q: change its Compose file and run docker compose up instead", name, p)
	}
	if host["AutoRemove"] == true {
		return fmt.Errorf("%s was started with --rm, so it is deleted as soon as it stops: there would be nothing to go back to if the new one failed", name)
	}
	if deps, err := dependents(ctx, c, id, name); err != nil {
		return err
	} else if len(deps) > 0 {
		return fmt.Errorf("other containers depend on %s and would break: %s. Recreate them together, or move them to a user-defined network", name, strings.Join(deps, ", "))
	}
	if !o.changes.Empty() && !o.changes.OnlyRestart() && !o.discardChanges {
		changed, err := lostFiles(ctx, c, id, list(ctr["Mounts"]))
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			shown := changed
			if len(shown) > 5 {
				shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(changed)-5))
			}
			return fmt.Errorf("files were changed inside %s, outside its volumes, and would be lost with the old container:\n  %s\nMove them into a volume first, or use --discard-changes", name, strings.Join(shown, "\n  "))
		}
	}

	before, err := recreate.FromInspect(ctr)
	if err != nil {
		return err
	}
	after, err := recreate.FromInspect(ctr)
	if err != nil {
		return err
	}
	var notes []string
	imageRef := str(cfg["Image"])
	oldImg, err := c.InspectImage(ctx, str(ctr["Image"]))
	if err != nil && !engine.IsNotFound(err) {
		return fmt.Errorf("reading the container's image: %w", err)
	}
	oldImageGone := err != nil
	newImg := oldImg
	if o.changes.Image != "" {
		if newImg, err = c.InspectImage(ctx, o.changes.Image); engine.IsNotFound(err) {
			return fmt.Errorf("there is no image %s here: pull it first (docker pull %s)", o.changes.Image, o.changes.Image)
		} else if err != nil {
			return err
		}
		if oldImageGone {
			// Without the old image its defaults cannot be told apart: take
			// what the new image sets to be image defaults.
			if guessed := after.DropNewImageNames(newImg); len(guessed) > 0 {
				notes = append(notes, fmt.Sprintf("the old image is gone, so its defaults are unknown; %s now come from %s (set them with --env or --label to keep a value)", strings.Join(guessed, ", "), o.changes.Image))
			}
		} else {
			after.DropImageDefaults(oldImg)
		}
	} else if img, err := c.InspectImage(ctx, imageRef); err == nil && str(img["Id"]) != str(ctr["Image"]) || engine.IsNotFound(err) {
		// The tag now names another image (or is gone).
		if oldImageGone {
			return fmt.Errorf("the image %s runs is no longer here: %s now names a different image, and Docker removed the old one. Use --image %s to move to the image it names now, or tag the old image again", name, imageRef, imageRef)
		}
		// Keep the very image the container runs.
		after.Config["Image"] = str(ctr["Image"])
		before.Config["Image"] = str(ctr["Image"])
		notes = append(notes, fmt.Sprintf("%s now names a different image; %s keeps the one it runs (use --image %s to move to it)", imageRef, name, imageRef))
	} else if err != nil {
		return err
	}
	if err := edit.Apply(after, o.changes); err != nil {
		return usagef("%v", err)
	}
	// Show what the new container will really have: what it leaves to its
	// image comes from the (new) image.
	shown := edit.Effective(after, newImg)
	if oldID, newID := str(ctr["Image"]), str(newImg["Id"]); o.changes.Image != "" && oldID != newID && str(shown.Config["Image"]) == str(before.Config["Image"]) {
		// The same name for another image: show the images.
		before.Config["Image"] = fmt.Sprintf("%s (%s)", imageRef, shortImage(oldID))
		shown.Config["Image"] = fmt.Sprintf("%s (%s)", o.changes.Image, shortImage(newID))
	}
	lines := edit.Diff(before, shown)
	status := str(state["Status"])
	shownRef := imageRef
	if strings.HasPrefix(imageRef, "sha256:") {
		shownRef = shortImage(imageRef)
	}
	out("%s (%s, %s)", name, shownRef, status)
	for _, n := range notes {
		out("  note: %s", n)
	}
	if len(lines) == 0 {
		out("Nothing to change.")
		return nil
	}
	for _, l := range lines {
		out("%s", l)
	}
	inPlace := o.changes.OnlyRestart()
	if inPlace {
		out("This changes in place; the container keeps running.")
	} else if state["Running"] == true {
		out("%s will be stopped and made again with these changes; its volumes and their data stay.", name)
	} else {
		out("%s will be made again with these changes (it stays stopped); its volumes and their data stay.", name)
	}
	if o.dryRun {
		return nil
	}
	if !o.yes {
		yes, err := confirm(ctx, env.Stdin, env.Stdout, "Go ahead?")
		if err != nil {
			return err
		}
		if !yes {
			return errors.New("stopped; nothing was changed")
		}
	}
	if inPlace {
		if err := c.Call(ctx, engine.Request{Method: http.MethodPost, Path: "/containers/" + url.PathEscape(id) + "/update",
			JSON: engine.Doc{"RestartPolicy": after.Host["RestartPolicy"]}}, nil); err != nil {
			return err
		}
		out("Done.")
		return nil
	}
	newID, err := recreate.Swap(ctx, c, ctr, after, recreate.SwapOptions{
		KeepOld: o.keepOld,
		Log:     func(s string) { out("%s", s) },
		Started: func(id string) error { return waitHealthy(ctx, c, id, o.wait) },
	})
	if err != nil {
		return err
	}
	out("Done: %s (%s) runs with the changes.", name, short(newID))
	return nil
}

// dependents are containers that use this one's network.
func dependents(ctx context.Context, c *engine.Client, id, name string) ([]string, error) {
	all, err := c.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, x := range all {
		mode := str(asDoc(x["HostConfig"])["NetworkMode"])
		if mode == "container:"+id || mode == "container:"+name {
			for _, n := range list(x["Names"]) {
				out = append(out, strings.TrimPrefix(str(n), "/")+" (--network container:"+name+")")
			}
		}
		// A legacy link shows as "/linker/alias" among the linked
		// container's names. Renaming the container moves the link to the
		// old one, so the linker would break when it is removed.
		if str(x["Id"]) == id {
			for _, n := range list(x["Names"]) {
				if linker, _, ok := strings.Cut(strings.TrimPrefix(str(n), "/"), "/"); ok {
					out = append(out, linker+" (--link)")
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// runtimePaths hold what programs write while running; changes there are
// not data.
var runtimePaths = []string{"/run", "/var/run", "/tmp", "/var/tmp", "/var/cache"}

// lostFiles lists files changed in the container's writable layer, which a
// new container would not have.
func lostFiles(ctx context.Context, c *engine.Client, id string, mounts []any) ([]string, error) {
	changes, err := c.Changes(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("checking for changed files: %w", err)
	}
	// Docker makes the folders volumes are mounted on; they hold nothing.
	var dests []string
	for _, m := range mounts {
		dests = append(dests, str(asDoc(m)["Destination"]))
	}
	paths := map[string]bool{}
	for _, ch := range changes {
		paths[str(ch["Path"])] = true
	}
	var out []string
	for _, ch := range changes {
		p := str(ch["Path"])
		if underAny(p, runtimePaths) || mountPoint(p, dests) {
			continue
		}
		// A directory marked changed only because something in it changed.
		if fmt.Sprint(ch["Kind"]) == "0" && hasChild(p, paths) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// mountPoint reports a mount's folder or one of its parents.
func mountPoint(p string, dests []string) bool {
	for _, d := range dests {
		if d == p || strings.HasPrefix(d, p+"/") {
			return true
		}
	}
	return false
}

func underAny(p string, prefixes []string) bool {
	for _, pre := range prefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

func hasChild(p string, paths map[string]bool) bool {
	for q := range paths {
		if strings.HasPrefix(q, p+"/") {
			return true
		}
	}
	return false
}

// waitHealthy waits until the new container has run for a moment (or, with
// a healthcheck, is healthy).
func waitHealthy(ctx context.Context, c *engine.Client, id string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	steady := time.Now().Add(min(3*time.Second, wait))
	for {
		d, err := c.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		st := asDoc(d["State"])
		if st["Running"] != true || st["Restarting"] == true {
			return fmt.Errorf("the new container stopped (exit code %v)%s", st["ExitCode"], logTail(ctx, c, id))
		}
		health := asDoc(st["Health"])
		switch str(health["Status"]) {
		case "healthy":
			return nil
		case "unhealthy":
			return fmt.Errorf("the new container is unhealthy%s", logTail(ctx, c, id))
		case "":
			if time.Now().After(steady) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if health != nil {
				return fmt.Errorf("the new container did not become healthy within %s (raise --wait if it starts slowly)", wait)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// logTail is the end of a container's log, for an error message.
func logTail(ctx context.Context, c *engine.Client, id string) string {
	resp, err := c.Do(ctx, engine.Request{Method: http.MethodGet, Path: "/containers/" + url.PathEscape(id) + "/logs",
		Query: url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"10"}}})
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	// Without a TTY the log comes in frames: 8-byte header, then text.
	var text strings.Builder
	for len(data) >= 8 && (data[0] == 1 || data[0] == 2) {
		n := int(binary.BigEndian.Uint32(data[4:8]))
		if 8+n > len(data) {
			break
		}
		text.Write(data[8 : 8+n])
		data = data[8+n:]
	}
	if text.Len() == 0 {
		text.Write(data)
	}
	t := strings.TrimSpace(text.String())
	if t == "" {
		return ""
	}
	return "; its last output:\n  " + strings.ReplaceAll(t, "\n", "\n  ")
}

func confirm(ctx context.Context, in io.Reader, out io.Writer, q string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N] ", q)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		answer <- line
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(out)
		return false, ctx.Err()
	case line := <-answer:
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
}

// shortImage is an image ID as docker images shows it.
func shortImage(id string) string {
	return short(strings.TrimPrefix(id, "sha256:"))
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func asDoc(v any) engine.Doc {
	d, _ := v.(engine.Doc)
	return d
}
