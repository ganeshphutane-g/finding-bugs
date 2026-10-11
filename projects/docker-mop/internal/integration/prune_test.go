package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/plan"
)

// TestPruneRemovesExactlyThePlan applies each plan with docker-mop and checks
// that exactly the planned objects are gone. Together with
// TestPlanMatchesRealPrune this shows docker-mop prune ends in the same state
// as Docker's own prune.
func TestPruneRemovesExactlyThePlan(t *testing.T) {
	c := requireDocker(t)
	for _, tc := range pruneCases {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			run := newRunID(t)
			world(t, run)
			filters := append([]string{"label=" + testLabel + "=" + run}, tc.filters...)
			compare(t, c, tc, filters, true)
		})
	}
	t.Run("classic_builder", func(t *testing.T) {
		run := newRunID(t)
		cleanupRun(t, run)
		if !buildClassicChain(t, run) {
			t.Skip("this daemon does not keep parent images")
		}
		compare(t, c, pruneCase{kind: plan.Images, all: true}, []string{"label=" + testLabel + "=" + run}, true)
	})
}

func planFor(t *testing.T, kind plan.Kind, all bool, run string) plan.Plan {
	t.Helper()
	c := requireDocker(t)
	st, err := plan.Gather(context.Background(), c, kind)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := plan.ParseFilters([]string{"label=" + testLabel + "=" + run})
	p, err := plan.Compute(st, plan.Options{Kind: kind, All: all, Filters: f})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func skippedReason(res plan.Result, id string) string {
	for _, o := range res.Skipped() {
		if o.Item.ID == id || o.Item.Name == id {
			return o.Reason
		}
	}
	return ""
}

func TestPruneSkipsContainerStartedAfterPlan(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	repo := "mop-" + run
	buildImage(t, run, repo+":app", "app")
	id := docker(t, "create", "--label", testLabel+"="+run, repo+":app", "sleep")

	p := planFor(t, plan.Containers, false, run)
	if len(p.Containers) != 1 {
		t.Fatalf("expected the created container in the plan, got %+v", p.Containers)
	}
	docker(t, "start", id)

	res, err := plan.Apply(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if reason := skippedReason(res, id); !strings.Contains(reason, "running") {
		t.Fatalf("expected a skip because the container is running, got %q (%+v)", reason, res)
	}
	if docker(t, "inspect", "-f", "{{.State.Status}}", id) != "running" {
		t.Fatal("the running container was touched")
	}
}

func TestPruneLeavesTagMovedAfterPlan(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	tag := "mop-" + run + ":moving"
	buildImage(t, run, tag, "old")

	p := planFor(t, plan.Images, true, run)
	if len(p.Images) != 1 {
		t.Fatalf("expected one image in the plan, got %+v", p.Images)
	}
	buildImage(t, run, tag, "new") // the tag now names a different image
	newID := docker(t, "image", "inspect", "-f", "{{.Id}}", tag)
	if newID == p.Images[0].ID {
		t.Fatal("test setup: the tag did not move")
	}

	res, err := plan.Apply(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped()) != 1 {
		t.Fatalf("expected the moved image to be skipped, got %+v", res)
	}
	if got := docker(t, "image", "inspect", "-f", "{{.Id}}", tag); got != newID {
		t.Fatalf("the new image behind %s was touched", tag)
	}
}

func TestPruneLeavesVolumeRecreatedAfterPlan(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	name := "mop-" + run + "-vol"
	docker(t, "volume", "create", "--label", testLabel+"="+run, name)

	p := planFor(t, plan.Volumes, true, run)
	if len(p.Volumes) != 1 {
		t.Fatalf("expected one volume in the plan, got %+v", p.Volumes)
	}
	docker(t, "volume", "rm", name)
	time.Sleep(1100 * time.Millisecond) // CreatedAt has one-second precision
	docker(t, "volume", "create", "--label", testLabel+"="+run, name)

	res, err := plan.Apply(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if reason := skippedReason(res, name); !strings.Contains(reason, "recreated") {
		t.Fatalf("expected a skip because the volume was recreated, got %q (%+v)", reason, res)
	}
	docker(t, "volume", "inspect", name)
}

func TestPruneSkipsVolumeTakenIntoUse(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	repo := "mop-" + run
	buildImage(t, run, repo+":app", "app")
	docker(t, "volume", "create", "--label", testLabel+"="+run, repo+"-data")

	p := planFor(t, plan.Volumes, true, run)
	if len(p.Volumes) != 1 {
		t.Fatalf("expected one volume in the plan, got %+v", p.Volumes)
	}
	docker(t, "create", "--label", testLabel+"="+run, "-v", repo+"-data:/data", repo+":app")

	res, err := plan.Apply(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	if reason := skippedReason(res, repo+"-data"); reason == "" {
		t.Fatalf("expected the volume to be skipped, got %+v", res)
	}
	docker(t, "volume", "inspect", repo+"-data")
}
