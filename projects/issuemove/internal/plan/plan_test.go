package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/ref"
	"github.com/ganeshphutane-g/issuemove/internal/testkit"
)

func contains(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func build(t *testing.T, src, dst *gitea.Client, source, target string) *Plan {
	t.Helper()
	s, err := ref.ParseSource(source)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ref.ParseRepo(target)
	p, err := Build(context.Background(), Input{Src: src, Dst: dst, Source: s, Target: r})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanSameInstance(t *testing.T) {
	s := testkit.Start(t)
	s.User("alice", false)
	s.Repo("src")
	s.Repo("dst")
	s.Call("POST", "/repos/root/src/labels", map[string]any{"name": "bug", "color": "#ee0701"}, nil)
	s.Call("POST", "/repos/root/src/labels", map[string]any{"name": "ui", "color": "#00ff00"}, nil)
	s.Call("POST", "/repos/root/dst/labels", map[string]any{"name": "Bug", "color": "#ff0000"}, nil)
	s.Call("POST", "/repos/root/src/milestones", map[string]any{"title": "v2"}, nil)
	var labels []gitea.Label
	s.Call("GET", "/repos/root/src/labels", nil, &labels)
	var ids []int64
	for _, l := range labels {
		ids = append(ids, l.ID)
	}
	s.Call("POST", "/repos/root/src/issues", map[string]any{"title": "first", "body": "b", "labels": ids, "milestone": 1, "assignees": []string{"root"}}, nil)
	alice := s.Client("alice")
	if _, err := alice.CreateComment(context.Background(), "root", "src", 1, "a comment"); err != nil {
		t.Fatal(err)
	}
	p := build(t, s.Admin, s.Admin, "root/src#1", "root/dst")
	if !p.OK() || !p.SameInstance || !p.AsAuthors || len(p.Issues) != 1 {
		t.Fatalf("plan: %+v", p)
	}
	if _, ok := p.LabelMap["bug"]; !ok || len(p.NewLabels) != 1 || p.NewLabels[0].Name != "ui" {
		t.Fatalf("labels: %v new %v", p.LabelMap, p.NewLabels)
	}
	if len(p.NewMilestones) != 1 || p.NewMilestones[0].Title != "v2" {
		t.Fatalf("milestones %v", p.NewMilestones)
	}
	if p.Authors["alice"] != "alice" || p.Authors["root"] != "root" {
		t.Fatalf("authors %v", p.Authors)
	}
	if got := p.Issues[0].Assignees; len(got) != 1 || got[0] != "root" {
		t.Fatalf("assignees %v", got)
	}
	// Without an admin token: attributed, not posted as the authors.
	s.Call("PUT", "/repos/root/dst/collaborators/alice", map[string]any{"permission": "write"}, nil)
	s.Call("PUT", "/repos/root/src/collaborators/alice", map[string]any{"permission": "write"}, nil)
	p = build(t, alice, alice, "root/src#1", "root/dst")
	if p.AsAuthors || p.Authors["alice"] != "" || !contains(p.Warnings, "not a site admin") {
		t.Fatalf("non-admin plan: %+v", p)
	}
}

func TestPlanBlockers(t *testing.T) {
	s := testkit.Start(t)
	s.User("bob", false)
	s.Repo("src")
	s.Repo("dst")
	s.Repo("noissues")
	s.Call("PATCH", "/repos/root/noissues", map[string]any{"has_issues": false}, nil)
	s.Call("POST", "/repos/root/src/issues", map[string]any{"title": "one"}, nil)
	s.Call("POST", "/repos/root/src/labels", map[string]any{"name": "new-label", "color": "#123456"}, nil)
	s.Call("POST", "/repos/root/src/issues", map[string]any{"title": "two", "labels": []int64{1}}, nil)
	if p := build(t, s.Admin, s.Admin, "root/src#1,9", "root/dst"); !contains(p.Blockers, "#9 does not exist") {
		t.Fatalf("missing issue: %q", p.Blockers)
	}
	if p := build(t, s.Admin, s.Admin, "root/src#1", "root/noissues"); !contains(p.Blockers, "issues are turned off") {
		t.Fatalf("no issues: %q", p.Blockers)
	}
	bob := s.Client("bob")
	if p := build(t, bob, bob, "root/src#2", "root/dst"); !contains(p.Blockers, "cannot write to root/src") || !contains(p.Blockers, "cannot create labels") {
		t.Fatalf("bob: %q", p.Blockers)
	}
	if _, err := Build(context.Background(), Input{Src: s.Admin, Dst: s.Admin, Source: ref.Source{Owner: "root", Repo: "src"}, Target: ref.Repo{Owner: "root", Name: "src"}}); err == nil {
		t.Fatal("same repository should be refused")
	}
}
