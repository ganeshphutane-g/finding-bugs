// Package integration checks docker-mop against a real Docker daemon.
//
// These tests create and prune real Docker objects, so they only run when
// asked to:
//
//	MOP_INTEGRATION=1            tests that only touch objects they created
//	                             (every prune is scoped with a label filter)
//	MOP_INTEGRATION=destructive  also runs unscoped prunes that can remove
//	                             anything on the daemon; use a throwaway daemon
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

const testLabel = "io.github.docker-mop.test"

func requireDocker(t *testing.T) *engine.Client {
	t.Helper()
	mode := os.Getenv("MOP_INTEGRATION")
	if mode == "" {
		t.Skip("set MOP_INTEGRATION=1 to run tests against a real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("MOP_INTEGRATION is set but the docker CLI is not on PATH")
	}
	ep, err := engine.ResolveEndpoint(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := engine.New(ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("MOP_INTEGRATION is set but the daemon is not reachable: %v", err)
	}
	return c
}

func requireDestructive(t *testing.T) {
	t.Helper()
	if os.Getenv("MOP_INTEGRATION") != "destructive" {
		t.Skip("set MOP_INTEGRATION=destructive to run unscoped prunes (removes anything unused on the daemon)")
	}
}

// docker runs the docker CLI and fails the test on error.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// dockerQuiet runs the docker CLI and ignores errors (for cleanup).
func dockerQuiet(args ...string) {
	_ = exec.Command("docker", args...).Run()
}

func newRunID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "t" + hex.EncodeToString(b)
}

var (
	helperOnce sync.Once
	helperPath string
	helperErr  error
)

// helperBinary builds a tiny static program used as the entrypoint of test
// images, so no image ever has to be pulled from a registry.
func helperBinary(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mop-helper")
		if err != nil {
			helperErr = err
			return
		}
		src := `package main

import (
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		time.Sleep(time.Hour)
	}
}
`
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
			helperErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module helper\n\ngo 1.22\n"), 0o644); err != nil {
			helperErr = err
			return
		}
		cmd := exec.Command("go", "build", "-o", "helper", ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			helperErr = &buildError{err: err, out: string(out)}
			return
		}
		helperPath = filepath.Join(dir, "helper")
	})
	if helperErr != nil {
		t.Fatal(helperErr)
	}
	return helperPath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

// buildImage builds a scratch image labelled with run, tagged tag. salt makes
// the content unique, so rebuilding a tag with a new salt leaves the old image
// dangling.
func buildImage(t *testing.T, run, tag, salt string) {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(helperBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "salt"), []byte(salt), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM scratch\nCOPY helper /helper\nCOPY salt /salt\nLABEL " + testLabel + "=" + run + "\nENTRYPOINT [\"/helper\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	docker(t, "build", "-q", "-t", tag, dir)
}

// importUntagged imports an empty, untagged image labelled with run. Docker
// lists it as dangling.
func importUntagged(t *testing.T, run string) {
	t.Helper()
	tarPath := filepath.Join(t.TempDir(), "empty.tar")
	// An empty tar archive is two 512-byte zero blocks.
	if err := os.WriteFile(tarPath, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	docker(t, "import", "--change", "LABEL "+testLabel+"="+run, "--message", run, tarPath)
}

// snapshot records what exists on the daemon.
type snapshot struct {
	containers map[string]bool
	images     map[string][]string // image ID -> tags
	volumes    map[string]bool
	networks   map[string]bool
}

func takeSnapshot(t *testing.T, c *engine.Client) snapshot {
	t.Helper()
	ctx := context.Background()
	du, err := c.DiskUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nets, err := c.Networks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := c.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot{
		containers: map[string]bool{},
		images:     map[string][]string{},
		volumes:    map[string]bool{},
		networks:   map[string]bool{},
	}
	for _, ct := range du.Containers {
		s.containers[ct.ID] = true
	}
	for _, img := range imgs {
		tags := img.Tags()
		sort.Strings(tags)
		s.images[img.ID] = tags
	}
	for _, v := range du.Volumes {
		s.volumes[v.Name] = true
	}
	for _, n := range nets {
		s.networks[n.ID] = true
	}
	return s
}

func removed(before, after map[string]bool) []string {
	var out []string
	for k := range before {
		if !after[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func removedImages(before, after map[string][]string) []string {
	var out []string
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func untaggedOnly(before, after map[string][]string) []string {
	var out []string
	for k, tags := range before {
		if now, ok := after[k]; ok && len(now) < len(tags) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func cleanupRun(t *testing.T, run string) {
	t.Cleanup(func() {
		filter := "label=" + testLabel + "=" + run
		ids := exec.Command("docker", "ps", "-aq", "--filter", filter)
		if out, err := ids.Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				dockerQuiet("rm", "-f", "-v", id)
			}
		}
		if out, err := exec.Command("docker", "image", "ls", "-a", "-q", "--filter", filter).Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				dockerQuiet("rmi", "-f", id)
			}
		}
		if out, err := exec.Command("docker", "volume", "ls", "-q", "--filter", filter).Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				dockerQuiet("volume", "rm", "-f", id)
			}
		}
		if out, err := exec.Command("docker", "network", "ls", "-q", "--filter", filter).Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				dockerQuiet("network", "rm", id)
			}
		}
	})
}

// waitForState waits until a container reaches state (e.g. "exited").
func waitForState(t *testing.T, id, state string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if docker(t, "inspect", "-f", "{{.State.Status}}", id) == state {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s did not reach state %q", id, state)
}
