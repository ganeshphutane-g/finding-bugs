// Package images works out which image a Postgres service should move to,
// and what an image provides.
package images

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/pg"
)

// Ref is an image reference split into repository and tag.
type Ref struct {
	Repo   string // as written, e.g. "postgres" or "docker.io/library/postgres"
	Tag    string // "" for none
	Digest string // "sha256:..." when pinned by digest
}

// Parse splits an image reference.
func Parse(s string) Ref {
	var r Ref
	if i := strings.Index(s, "@"); i >= 0 {
		s, r.Digest = s[:i], s[i+1:]
	}
	// A colon after the last slash starts the tag (a colon before it is a
	// registry port).
	slash := strings.LastIndex(s, "/")
	if i := strings.LastIndex(s, ":"); i > slash {
		s, r.Tag = s[:i], s[i+1:]
	}
	r.Repo = s
	return r
}

func (r Ref) String() string {
	s := r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// short is the repository without a Docker Hub registry prefix.
func (r Ref) short() string {
	s := strings.TrimPrefix(r.Repo, "docker.io/")
	s = strings.TrimPrefix(s, "index.docker.io/")
	return strings.TrimPrefix(s, "library/")
}

var (
	officialTag = regexp.MustCompile(`^(\d+)(\.\d+)*(-(.+))?$`)
	pgvectorTag = regexp.MustCompile(`^(?:[\d.]+-)?pg(\d+)(-.+)?$`)
	timescale   = regexp.MustCompile(`^(?:latest|[\d.]+)-pg(\d+)(-.+)?$`)
)

// Target returns the image to move to for major version `to`, for the
// image families pgcarry knows. For others it returns an error asking for
// --image.
func Target(current string, to int) (string, error) {
	r := Parse(current)
	if r.Digest != "" {
		return "", fmt.Errorf("%s is pinned by digest; give the new image with --image", current)
	}
	switch r.short() {
	case "postgres":
		variant := ""
		if m := officialTag.FindStringSubmatch(r.Tag); m != nil && strings.HasPrefix(m[4], "alpine") {
			variant = "-alpine"
		} else if strings.HasPrefix(r.Tag, "alpine") {
			variant = "-alpine"
		}
		return Ref{Repo: r.Repo, Tag: strconv.Itoa(to) + variant}.String(), nil
	case "pgvector/pgvector":
		if m := pgvectorTag.FindStringSubmatch(r.Tag); m != nil {
			return Ref{Repo: r.Repo, Tag: "pg" + strconv.Itoa(to)}.String(), nil
		}
	case "timescale/timescaledb", "timescale/timescaledb-ha":
		if m := timescale.FindStringSubmatch(r.Tag); m != nil {
			return Ref{Repo: r.Repo, Tag: "latest-pg" + strconv.Itoa(to) + m[2]}.String(), nil
		}
	}
	return "", fmt.Errorf("pgcarry cannot tell which %s tag holds PostgreSQL %d; give it with --image (for example the same image family's tag for %d)", r.Repo, to, to)
}

// Info is what an image's configuration says.
type Info struct {
	ID      string
	PGData  string   // PGDATA from the image environment
	PGMajor string   // PG_MAJOR from the image environment, if set
	Volumes []string // declared VOLUMEs
}

// Inspect reads an image's configuration; pull fetches it if missing.
func Inspect(ctx context.Context, cli *docker.CLI, image string, pull bool) (*Info, error) {
	out, err := cli.Run(ctx, "image", "inspect", image)
	if err != nil && pull && docker.IsNotFound(err) {
		if _, perr := cli.Run(ctx, "pull", "-q", image); perr != nil {
			return nil, fmt.Errorf("pulling %s: %w", image, perr)
		}
		out, err = cli.Run(ctx, "image", "inspect", image)
	}
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID     string `json:"Id"`
		Config struct {
			Env     []string            `json:"Env"`
			Volumes map[string]struct{} `json:"Volumes"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) == 0 {
		return nil, fmt.Errorf("reading docker image inspect output for %s", image)
	}
	info := &Info{ID: list[0].ID}
	for _, e := range list[0].Config.Env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "PGDATA":
			info.PGData = v
		case "PG_MAJOR":
			info.PGMajor = v
		}
	}
	for v := range list[0].Config.Volumes {
		info.Volumes = append(info.Volumes, v)
	}
	return info, nil
}

// Probe is what a fresh cluster of an image looks like.
type Probe struct {
	VersionNum    int               `json:"version_num"`
	DataChecksums bool              `json:"data_checksums"`
	Extensions    map[string]string `json:"extensions"` // available: name -> default version
	// TryErrors has, for each statement given to ProbeImage, the error it
	// gave ("" when it worked).
	TryErrors []string `json:"-"`
}

// Major is the probed major version.
func (p *Probe) Major() int { return p.VersionNum / 10000 }

// ProbeImage starts the image once with an empty, in-memory data folder and
// reads its version, its defaults and the extensions it can install. It
// also runs each of tries (statements creating a database named in their
// text), recording whether it worked, and drops what they created.
func ProbeImage(ctx context.Context, cli *docker.CLI, image string, info *Info, tries []string) (*Probe, error) {
	args := []string{"run", "-d", "--rm", "--label", "pgcarry.probe=1",
		"-e", "POSTGRES_PASSWORD=pgcarry-probe", "-e", "POSTGRES_USER=postgres"}
	for _, v := range info.Volumes {
		args = append(args, "--tmpfs", v)
	}
	args = append(args, image)
	out, err := cli.Run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("starting %s for a test: %w", image, err)
	}
	id := strings.TrimSpace(out)
	defer cli.Run(context.WithoutCancel(ctx), "rm", "-f", id)
	if err := WaitReady(ctx, cli, id, 2*time.Minute); err != nil {
		return nil, fmt.Errorf("test start of %s: %w", image, err)
	}
	srv := &pg.Server{CLI: cli, Container: id, User: "postgres"}
	var p Probe
	cl, err := srv.Inspect(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	p.VersionNum, p.DataChecksums = cl.VersionNum, cl.DataChecksums
	if p.Extensions, err = srv.AvailableExtensions(ctx, "postgres"); err != nil {
		return nil, err
	}
	for _, stmt := range tries {
		_, err := srv.Query(ctx, "postgres", stmt)
		msg := ""
		if err != nil {
			msg = firstError(err.Error())
		}
		p.TryErrors = append(p.TryErrors, msg)
	}
	return &p, nil
}

// firstError picks psql's ERROR line out of a failed command's message.
func firstError(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "ERROR:"); i >= 0 {
			return strings.TrimSpace(line[i+len("ERROR:"):])
		}
	}
	return strings.TrimSpace(s)
}

// WaitReady waits until a container started with the official entrypoint
// has finished initialising and accepts connections.
func WaitReady(ctx context.Context, cli *docker.CLI, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := cli.Run(ctx, "inspect", "-f", "{{.State.Running}}", id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(state) != "true" {
			logs, _ := cli.Run(ctx, "logs", "--tail", "20", id)
			return fmt.Errorf("the container stopped:\n%s", logs)
		}
		// The entrypoint runs a temporary server while initialising; only
		// the final one is reached over TCP.
		// -U: a container running as an arbitrary uid has no user name,
		// and pg_isready makes no attempt without one.
		if _, err := cli.Run(ctx, "exec", id, "pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
