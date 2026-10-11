// Package volume copies and renames Docker volumes.
//
// Data is moved with the container archive API through containers that are
// created but never started, from a tiny local helper image. Nothing is pulled
// from a registry, and every copy is checked file by file before it counts as
// done.
package volume

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

const (
	// HelperImage is the empty image the helper containers are created from.
	HelperImage = "docker-mop-helper:1"
	// HelperLabel marks helper containers so they are easy to find.
	HelperLabel = "io.github.docker-mop.helper"
	// mountPoint is where volumes are mounted inside helper containers.
	mountPoint = "/volume"

	composeProjectLabel = "com.docker.compose.project"
	composeVolumeLabel  = "com.docker.compose.volume"
)

// Options control copy and rename.
type Options struct {
	// ComposeProject, when set, relabels the new volume so Docker Compose
	// project ComposeProject owns it. The new name must then be
	// "<project>_<volume>".
	ComposeProject string
	// Progress, when set, receives short status lines.
	Progress func(string)
	// Warn, when set, receives warnings that should be shown even in quiet
	// mode.
	Warn func(string)
}

func (o Options) progress(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(fmt.Sprintf(format, args...))
	}
}

func (o Options) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(fmt.Sprintf(format, args...))
	}
}

// Report describes a finished copy.
type Report struct {
	Files int   // regular files copied
	Bytes int64 // bytes of file content copied
}

// User is a container that uses a volume.
type User struct {
	ID      string
	Name    string
	State   string
	Running bool
	// Helper is true for a docker-mop helper container left over from an
	// interrupted run.
	Helper bool
}

// Users lists containers (running or not) that mount the volume.
func Users(ctx context.Context, c *engine.Client, name string) ([]User, error) {
	filters, _ := json.Marshal(map[string][]string{"volume": {name}})
	var list []engine.Container
	err := c.Call(ctx, engine.Request{
		Method: http.MethodGet,
		Path:   "/containers/json",
		Query:  url.Values{"all": {"1"}, "filters": {string(filters)}},
	}, &list)
	if err != nil {
		return nil, err
	}
	var users []User
	for _, ct := range list {
		users = append(users, User{
			ID:      ct.ID,
			Name:    ct.Name(),
			State:   ct.State,
			Running: ct.State == "running" || ct.State == "paused" || ct.State == "restarting",
			Helper:  ct.Labels[HelperLabel] != "",
		})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	return users, nil
}

func describeUsers(users []User) string {
	parts := make([]string, len(users))
	for i, u := range users {
		if u.Helper {
			parts[i] = fmt.Sprintf("%s (a docker-mop helper left over from an interrupted run; remove it with 'docker rm %s')", u.Name, u.Name)
		} else {
			parts[i] = fmt.Sprintf("%s (%s)", u.Name, u.State)
		}
	}
	return strings.Join(parts, ", ")
}

// Copy copies volume src into a new volume dst and verifies the copy. dst
// must not exist. On failure the new volume is removed again and src is
// untouched.
func Copy(ctx context.Context, c *engine.Client, src, dst string, opts Options) (Report, error) {
	srcVol, err := prepare(ctx, c, src, dst)
	if err != nil {
		return Report{}, err
	}
	users, err := Users(ctx, c, src)
	if err != nil {
		return Report{}, err
	}
	var running []User
	for _, u := range users {
		if u.Running {
			running = append(running, u)
		}
	}
	if len(running) > 0 {
		return Report{}, fmt.Errorf("volume %q is used by running %s: %s; stop %s first so the copy is consistent",
			src, plural(len(running), "container"), describeUsers(running), pronoun(len(running)))
	}
	return copyVolume(ctx, c, srcVol, dst, opts)
}

// Rename copies src to dst, verifies the copy, then removes src. It refuses
// when any container, running or stopped, uses src, because that container
// would keep pointing at the old name.
func Rename(ctx context.Context, c *engine.Client, src, dst string, opts Options) (Report, error) {
	srcVol, err := prepare(ctx, c, src, dst)
	if err != nil {
		return Report{}, err
	}
	users, err := Users(ctx, c, src)
	if err != nil {
		return Report{}, err
	}
	if len(users) > 0 {
		return Report{}, fmt.Errorf("volume %q is used by %s: %s; remove %s (or recreate %s with the new volume name) first",
			src, plural(len(users), "container"), describeUsers(users), pronoun(len(users)), pronoun(len(users)))
	}

	report, err := copyVolume(ctx, c, srcVol, dst, opts)
	if err != nil {
		return report, err
	}

	opts.progress("Removing volume %s", src)
	if err := c.Call(ctx, engine.Request{Method: http.MethodDelete, Path: "/volumes/" + url.PathEscape(src)}, nil); err != nil {
		return report, fmt.Errorf("copied and verified %q as %q, but removing %q failed: %w; both volumes now exist", src, dst, src, err)
	}
	return report, nil
}

// prepare checks that src can be copied and that dst is free.
func prepare(ctx context.Context, c *engine.Client, src, dst string) (engine.Volume, error) {
	if src == dst {
		return engine.Volume{}, fmt.Errorf("source and destination are the same volume")
	}
	srcVol, err := inspect(ctx, c, src)
	if err != nil {
		return srcVol, err
	}
	if srcVol.Driver != "local" {
		return srcVol, fmt.Errorf("volume %q uses the %q driver; only local volumes can be copied", src, srcVol.Driver)
	}
	if len(srcVol.Options) > 0 {
		return srcVol, fmt.Errorf("volume %q has driver options (%s), so its data may live outside Docker (NFS, a bind device, tmpfs); copying it is not supported", src, describeOptions(srcVol.Options))
	}
	if _, err := inspect(ctx, c, dst); err == nil {
		return srcVol, fmt.Errorf("volume %q already exists", dst)
	} else if !engine.IsNotFound(err) {
		return srcVol, err
	}
	return srcVol, nil
}

// errNotOurs means the target volume was not (only) created by this copy, so
// it must be left alone.
var errNotOurs = errors.New("not created by this copy")

func copyVolume(ctx context.Context, c *engine.Client, srcVol engine.Volume, dst string, opts Options) (Report, error) {
	labels, dropped := newLabels(srcVol.Labels, dst, opts.ComposeProject)
	if opts.ComposeProject != "" {
		if err := checkComposeName(dst, opts.ComposeProject); err != nil {
			return Report{}, err
		}
	} else if dropped {
		opts.warn("%q had Docker Compose labels for project %q; they were not copied. Use --compose-project to label %q for a Compose project.",
			srcVol.Name, srcVol.Labels[composeProjectLabel], dst)
	}

	if err := ensureHelperImage(ctx, c); err != nil {
		return Report{}, err
	}

	opts.progress("Creating volume %s", dst)
	err := c.Call(ctx, engine.Request{
		Method: http.MethodPost,
		Path:   "/volumes/create",
		JSON:   map[string]any{"Name": dst, "Driver": srcVol.Driver, "Labels": labels},
	}, nil)
	if err != nil {
		// The daemon may have created the volume before the request was
		// interrupted.
		removeIfOurs(ctx, c, dst, labels, opts)
		return Report{}, fmt.Errorf("creating volume %q: %w", dst, err)
	}
	// Creating an existing local volume succeeds silently. If someone else
	// created dst after the check above, it has their labels, not ours.
	if v, err := inspect(ctx, c, dst); err != nil {
		return Report{}, err
	} else if !sameLabels(v.Labels, labels) {
		return Report{}, fmt.Errorf("volume %q was created by something else while copying; it was left untouched", dst)
	}

	report, err := copyAndVerify(ctx, c, srcVol.Name, dst, opts)
	if errors.Is(err, errNotOurs) {
		return report, fmt.Errorf("volume %q already had data in it when the copy started (was it created at the same time by something else?); it was left untouched", dst)
	}
	if err != nil {
		if rmErr := removeVolume(ctx, c, dst); rmErr != nil {
			return report, fmt.Errorf("%w (and removing the incomplete volume %q failed: %v)", err, dst, rmErr)
		}
		return report, err
	}
	return report, nil
}

// removeIfOurs removes dst after a failed create, but only when it exists,
// carries exactly our labels and is empty.
func removeIfOurs(ctx context.Context, c *engine.Client, dst string, labels map[string]string, opts Options) {
	ctx = context.WithoutCancel(ctx)
	v, err := inspect(ctx, c, dst)
	if err != nil || !sameLabels(v.Labels, labels) {
		return
	}
	if err := removeVolume(ctx, c, dst); err != nil {
		opts.warn("could not remove volume %q after the failed copy: %v", dst, err)
	}
}

// removeVolume uses a context that survives an interrupt, so cleanup still
// happens after Ctrl-C.
func removeVolume(ctx context.Context, c *engine.Client, name string) error {
	return c.Call(context.WithoutCancel(ctx), engine.Request{Method: http.MethodDelete, Path: "/volumes/" + url.PathEscape(name)}, nil)
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func inspect(ctx context.Context, c *engine.Client, name string) (engine.Volume, error) {
	var v engine.Volume
	err := c.Call(ctx, engine.Request{Method: http.MethodGet, Path: "/volumes/" + url.PathEscape(name)}, &v)
	if engine.IsNotFound(err) {
		return v, fmt.Errorf("volume %q not found: %w", name, err)
	}
	return v, err
}

func describeOptions(o map[string]string) string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + o[k]
	}
	return strings.Join(parts, ", ")
}

// newLabels copies src's labels. Docker Compose labels are only kept when a
// Compose project is given, and then they name the new project and volume;
// otherwise they are dropped (dropped reports that), so the new volume does
// not claim to belong to the old project.
func newLabels(src map[string]string, dst, project string) (labels map[string]string, dropped bool) {
	labels = map[string]string{}
	for k, v := range src {
		labels[k] = v
	}
	_, hasProject := labels[composeProjectLabel]
	_, hasVolume := labels[composeVolumeLabel]
	delete(labels, composeProjectLabel)
	delete(labels, composeVolumeLabel)
	if project == "" {
		return labels, hasProject || hasVolume
	}
	if short, ok := strings.CutPrefix(dst, project+"_"); ok && short != "" {
		labels[composeProjectLabel] = project
		labels[composeVolumeLabel] = short
	}
	return labels, false
}

// checkComposeName requires the "<project>_<volume>" name Compose uses.
func checkComposeName(dst, project string) error {
	if short, ok := strings.CutPrefix(dst, project+"_"); !ok || short == "" {
		return fmt.Errorf("with --compose-project %s the new name must look like %s_<volume>, as Compose names it", project, project)
	}
	return nil
}

// ensureHelperImage creates the empty helper image if it is missing.
func ensureHelperImage(ctx context.Context, c *engine.Client) error {
	err := c.Call(ctx, engine.Request{Method: http.MethodGet, Path: "/images/" + url.PathEscape(HelperImage) + "/json"}, nil)
	if err == nil {
		return nil
	}
	if !engine.IsNotFound(err) {
		return err
	}
	repo, tag, _ := strings.Cut(HelperImage, ":")
	// An empty tar archive is two 512-byte zero blocks.
	resp, err := c.Do(ctx, engine.Request{
		Method:      http.MethodPost,
		Path:        "/images/create",
		Query:       url.Values{"fromSrc": {"-"}, "repo": {repo}, "tag": {tag}},
		Body:        bytes.NewReader(make([]byte, 1024)),
		ContentType: "application/x-tar",
	})
	if err != nil {
		return fmt.Errorf("creating helper image: %w", err)
	}
	defer resp.Body.Close()
	return readJSONStream(resp.Body)
}

// readJSONStream drains a progress stream and returns the first error in it.
func readJSONStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.ErrorDetail.Message != "" {
			return errors.New(msg.ErrorDetail.Message)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}

// helper creates a never-started container with volume mounted at mountPoint.
func helper(ctx context.Context, c *engine.Client, volume string, readOnly bool) (string, func() error, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err := c.Call(ctx, engine.Request{
		Method: http.MethodPost,
		Path:   "/containers/create",
		Query:  url.Values{"name": {"docker-mop-helper-" + hex.EncodeToString(suffix)}},
		JSON: map[string]any{
			"Image":           HelperImage,
			"Cmd":             []string{"/docker-mop-never-started"},
			"Labels":          map[string]string{HelperLabel: "1"},
			"NetworkDisabled": true,
			"HostConfig": map[string]any{
				"NetworkMode": "none",
				"Mounts": []map[string]any{{
					"Type":     "volume",
					"Source":   volume,
					"Target":   mountPoint,
					"ReadOnly": readOnly,
				}},
			},
		},
	}, &created)
	if err != nil {
		return "", nil, fmt.Errorf("creating helper container for %q: %w", volume, err)
	}
	cleanup := func() error {
		err := c.Call(context.WithoutCancel(ctx), engine.Request{
			Method: http.MethodDelete,
			Path:   "/containers/" + url.PathEscape(created.ID),
			Query:  url.Values{"force": {"1"}},
		}, nil)
		if err != nil {
			return fmt.Errorf("removing helper container %s: %w", created.ID[:12], err)
		}
		return nil
	}
	return created.ID, cleanup, nil
}

// readArchive streams the volume's contents out of a helper container.
func readArchive(ctx context.Context, c *engine.Client, id string) (io.ReadCloser, error) {
	resp, err := c.Do(ctx, engine.Request{
		Method: http.MethodGet,
		Path:   "/containers/" + url.PathEscape(id) + "/archive",
		Query:  url.Values{"path": {mountPoint}},
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func copyAndVerify(ctx context.Context, c *engine.Client, src, dst string, opts Options) (_ Report, err error) {
	from, cleanFrom, err := helper(ctx, c, src, true)
	if err != nil {
		return Report{}, err
	}
	defer func() {
		if cerr := cleanFrom(); cerr != nil {
			opts.warn("%v", cerr)
		}
	}()
	to, cleanTo, err := helper(ctx, c, dst, false)
	if err != nil {
		return Report{}, err
	}
	// The target helper must be gone before a failed copy removes dst.
	defer func() {
		if cerr := cleanTo(); cerr != nil {
			opts.warn("%v", cerr)
		}
	}()

	// The target must still be empty: only its root directory.
	if existing, err := manifestOf(ctx, c, to); err != nil {
		return Report{}, fmt.Errorf("reading %q: %w", dst, err)
	} else if len(existing) > 1 {
		return Report{}, errNotOurs
	}

	opts.progress("Copying %s to %s", src, dst)
	archive, err := readArchive(ctx, c, from)
	if err != nil {
		return Report{}, fmt.Errorf("reading %q: %w", src, err)
	}
	defer archive.Close()

	// Record the source manifest from the same stream that is written, so
	// the source is read once for the copy and the check together.
	pr, pw := io.Pipe()
	type result struct {
		m   manifest
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := buildManifest(tar.NewReader(pr))
		if err == nil {
			_, err = io.Copy(io.Discard, pr) // drain padding after the end marker
		}
		pr.CloseWithError(err)
		done <- result{m, err}
	}()

	// The archive's entries start with the mount point's name, so extracting
	// it at "/" in a container that mounts dst at the same path fills dst.
	body := newNotifyingBody(io.TeeReader(archive, pw))
	resp, err := c.Do(ctx, engine.Request{
		Method:      http.MethodPut,
		Path:        "/containers/" + url.PathEscape(to) + "/archive",
		Query:       url.Values{"path": {"/"}},
		Body:        body,
		ContentType: "application/x-tar",
	})
	if err != nil {
		pw.CloseWithError(err)
		<-body.closed
		<-done
		return Report{}, fmt.Errorf("writing %q: %w", dst, err)
	}
	resp.Body.Close()
	// The HTTP client may still be sending the end of the body after the
	// response arrives; wait until it lets go, then feed whatever it did not
	// read to the manifest so that it covers the whole source.
	<-body.closed
	_, copyErr := io.Copy(pw, archive)
	pw.CloseWithError(copyErr)
	res := <-done
	if res.err != nil {
		return Report{}, fmt.Errorf("reading %q: %w", src, res.err)
	}

	opts.progress("Verifying %s against %s", dst, src)
	got, err := manifestOf(ctx, c, to)
	if err != nil {
		return Report{}, fmt.Errorf("reading %q back: %w", dst, err)
	}
	if diffs := res.m.diff(got); len(diffs) > 0 {
		return Report{}, &VerifyError{Diffs: diffs}
	}
	return res.m.report(), nil
}

// notifyingBody tells when the HTTP client is done with a request body: the
// client always closes the body once it stops reading it.
type notifyingBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func newNotifyingBody(r io.Reader) *notifyingBody {
	return &notifyingBody{Reader: r, closed: make(chan struct{})}
}

func (b *notifyingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func manifestOf(ctx context.Context, c *engine.Client, id string) (manifest, error) {
	r, err := readArchive(ctx, c, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return buildManifest(tar.NewReader(r))
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
