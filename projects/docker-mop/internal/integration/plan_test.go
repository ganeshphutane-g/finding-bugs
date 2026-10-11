package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
	"github.com/ganeshphutane-g/docker-mop/internal/plan"
)

// world creates a mix of objects that prunes treat differently, all labelled
// with the run ID so scoped prunes only see them.
func world(t *testing.T, run string) {
	t.Helper()
	cleanupRun(t, run)
	label := testLabel + "=" + run
	repo := "mop-" + run

	buildImage(t, run, repo+":running", "running")
	buildImage(t, run, repo+":exited", "exited")
	buildImage(t, run, repo+":created", "created")
	buildImage(t, run, repo+":unused", "unused")
	buildImage(t, run, repo+":rebuilt", "v1")
	buildImage(t, run, repo+":rebuilt", "v2") // v1 is now dangling on the classic image store
	importUntagged(t, run)                    // dangling on every image store

	for _, v := range []struct{ name, anon string }{
		{repo + "-named-unused", ""},
		{repo + "-named-stopped", ""},
		{repo + "-anon-unused", "anon"},
		{repo + "-anon-stopped", "anon"},
	} {
		args := []string{"volume", "create", "--label", label}
		if v.anon != "" {
			args = append(args, "--label", "com.docker.volume.anonymous=")
		}
		docker(t, append(args, v.name)...)
	}

	docker(t, "network", "create", "--label", label, repo+"-net-unused")
	docker(t, "network", "create", "--label", label, repo+"-net-running")
	docker(t, "network", "create", "--label", label, repo+"-net-stopped")

	docker(t, "run", "-d", "--name", repo+"-running", "--label", label,
		"--network", repo+"-net-running", repo+":running", "sleep")
	exited := docker(t, "run", "-d", "--name", repo+"-exited", "--label", label, "--label", "extra=x",
		"--network", repo+"-net-stopped",
		"-v", repo+"-named-stopped:/named", "-v", repo+"-anon-stopped:/anon",
		repo+":exited")
	waitForState(t, exited, "exited")
	docker(t, "create", "--name", repo+"-created", "--label", label, repo+":created")
}

type pruneCase struct {
	name    string
	kind    plan.Kind
	all     bool
	volumes bool
	filters []string // in addition to the run label
	cli     []string // the real docker prune command, without -f and filters
}

var pruneCases = []pruneCase{
	{name: "containers", kind: plan.Containers, cli: []string{"container", "prune"}},
	{name: "containers until future", kind: plan.Containers, filters: []string{"until=-1h"}, cli: []string{"container", "prune"}},
	{name: "containers until past", kind: plan.Containers, filters: []string{"until=1h"}, cli: []string{"container", "prune"}},
	{name: "containers label not", kind: plan.Containers, filters: []string{"label!=extra=x"}, cli: []string{"container", "prune"}},
	{name: "images dangling", kind: plan.Images, cli: []string{"image", "prune"}},
	{name: "images all", kind: plan.Images, all: true, cli: []string{"image", "prune", "-a"}},
	{name: "volumes anonymous", kind: plan.Volumes, cli: []string{"volume", "prune"}},
	{name: "volumes all", kind: plan.Volumes, all: true, cli: []string{"volume", "prune", "-a"}},
	{name: "networks", kind: plan.Networks, cli: []string{"network", "prune"}},
	{name: "system", kind: plan.System, cli: []string{"system", "prune"}},
	{name: "system all volumes", kind: plan.System, all: true, volumes: true, cli: []string{"system", "prune", "-a", "--volumes"}},
}

func TestPlanMatchesRealPrune(t *testing.T) {
	c := requireDocker(t)
	for _, tc := range pruneCases {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			run := newRunID(t)
			world(t, run)
			filters := append([]string{"label=" + testLabel + "=" + run}, tc.filters...)
			comparePlanWithPrune(t, c, tc, filters)
		})
	}
}

func comparePlanWithPrune(t *testing.T, c *engine.Client, tc pruneCase, filters []string) {
	t.Helper()
	ctx := context.Background()
	f, err := plan.ParseFilters(filters)
	if err != nil {
		t.Fatal(err)
	}
	st, err := plan.Gather(ctx, c, tc.kind)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Compute(st, plan.Options{Kind: tc.kind, All: tc.all, Volumes: tc.volumes, Filters: f})
	if err != nil {
		t.Fatal(err)
	}

	before := takeSnapshot(t, c)
	args := append([]string{}, tc.cli...)
	args = append(args, "-f")
	for _, fl := range filters {
		args = append(args, "--filter", fl)
	}
	docker(t, args...)
	after := takeSnapshot(t, c)

	check := func(what string, planned, actual []string) {
		t.Helper()
		if planned == nil {
			planned = []string{}
		}
		if actual == nil {
			actual = []string{}
		}
		if !reflect.DeepEqual(planned, actual) {
			t.Errorf("%s: plan said %v, docker removed %v", what, planned, actual)
		}
	}
	check("containers", ids(p.Containers, nil), removed(before.containers, after.containers))
	check("volumes", ids(p.Volumes, nil), removed(before.volumes, after.volumes))
	check("networks", ids(p.Networks, nil), removed(before.networks, after.networks))
	isUntagOnly := func(it plan.Item) bool { return strings.HasPrefix(it.Note, "only untagged") }
	check("images removed", ids(p.Images, func(it plan.Item) bool { return !isUntagOnly(it) }), removedImages(before.images, after.images))
	check("images untagged only", ids(p.Images, isUntagOnly), untaggedOnly(before.images, after.images))

	t.Logf("planned: %d containers, %d networks, %d volumes, %d images", len(p.Containers), len(p.Networks), len(p.Volumes), len(p.Images))
	if t.Failed() {
		t.Logf("plan: %+v", p)
	}
}

func ids(items []plan.Item, keep func(plan.Item) bool) []string {
	var out []string
	for _, it := range items {
		if keep == nil || keep(it) {
			out = append(out, it.ID)
		}
	}
	sort.Strings(out)
	return out
}

// TestPlanMatchesRealPruneUnscoped runs prunes without a label filter, which is
// how most people use them. It removes anything unused on the daemon.
func TestPlanMatchesRealPruneUnscoped(t *testing.T) {
	c := requireDocker(t)
	requireDestructive(t)
	for _, tc := range pruneCases {
		if len(tc.filters) > 0 {
			continue
		}
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			run := newRunID(t)
			world(t, run)
			comparePlanWithPrune(t, c, tc, nil)
		})
	}
}

// buildClassicChain builds base and app images with the classic builder,
// which leaves untagged parent images between them. It returns false when the
// classic builder is not available.
func buildClassicChain(t *testing.T, run string) bool {
	t.Helper()
	repo := "mop-" + run
	buildImage(t, run, repo+":base", "base")
	dir := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+run), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dockerfile := "FROM " + repo + ":base\nCOPY one /one\nCOPY two /two\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "build", "-q", "-t", repo+":app", dir)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("classic builder unavailable: %v\n%s", err, out)
		return false
	}
	parent := docker(t, "image", "inspect", "-f", "{{.Parent}}", repo+":app")
	return parent != ""
}

func TestPlanMatchesRealPruneClassicBuilder(t *testing.T) {
	c := requireDocker(t)
	for _, withContainer := range []bool{false, true} {
		name := "unused"
		if withContainer {
			name = "app_in_use"
		}
		t.Run(name, func(t *testing.T) {
			run := newRunID(t)
			cleanupRun(t, run)
			if !buildClassicChain(t, run) {
				t.Skip("this daemon does not keep parent images (classic builder with the classic image store)")
			}
			if withContainer {
				docker(t, "create", "--label", testLabel+"="+run, "mop-"+run+":app")
			}
			comparePlanWithPrune(t, c, pruneCase{kind: plan.Images, all: true, cli: []string{"image", "prune", "-a"}},
				[]string{"label=" + testLabel + "=" + run})
		})
	}
}
