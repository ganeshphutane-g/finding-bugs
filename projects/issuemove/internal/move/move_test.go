package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/plan"
	"github.com/ganeshphutane-g/issuemove/internal/ref"
	"github.com/ganeshphutane-g/issuemove/internal/testkit"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func doMove(t *testing.T, src, dst *gitea.Client, source, target string, hook func(string) error) ([]Result, error) {
	t.Helper()
	s, err := ref.ParseSource(source)
	must(t, err)
	r, _ := ref.ParseRepo(target)
	p, err := plan.Build(ctx, plan.Input{Src: src, Dst: dst, Source: s, Target: r, State: "all"})
	must(t, err)
	if !p.OK() {
		t.Fatalf("plan blocked: %q", p.Blockers)
	}
	return Run(ctx, Options{Src: src, Dst: dst, Plan: p, hook: hook})
}

// seed makes root/src with two issues: #1 by alice with a label, a
// milestone, an attachment linked in the body, comments by bob (with an
// attachment and a reference to #2) and reactions; #2 closed, referring to
// #1 and to #9 (not moved).
func seed(t *testing.T, s *testkit.Server) {
	s.User("alice", false)
	s.User("bob", false)
	s.Repo("src")
	s.Repo("dst")
	for _, u := range []string{"alice", "bob"} {
		s.Call("PUT", "/repos/root/src/collaborators/"+u, map[string]any{"permission": "write"}, nil)
		s.Call("PUT", "/repos/root/dst/collaborators/"+u, map[string]any{"permission": "write"}, nil)
	}
	s.Call("POST", "/repos/root/src/labels", map[string]any{"name": "bug", "color": "#ee0701", "description": "it is broken"}, nil)
	s.Call("POST", "/repos/root/src/milestones", map[string]any{"title": "v2", "description": "next"}, nil)
	alice := s.Admin.As("alice")
	bob := s.Admin.As("bob")
	is, err := alice.CreateIssue(ctx, "root", "src", gitea.NewIssue{Title: "Crash on save", Body: "It crashes.", Labels: []int64{1}, Milestone: 1, Assignees: []string{"bob"}})
	must(t, err)
	a, err := alice.AttachToIssue(ctx, "root", "src", is.Number, "trace.txt", strings.NewReader("stack trace here"))
	must(t, err)
	_, err = alice.EditIssue(ctx, "root", "src", is.Number, map[string]any{"body": "It crashes. See [trace](/attachments/" + a.UUID + ")."})
	must(t, err)
	c, err := bob.CreateComment(ctx, "root", "src", is.Number, "Same here, related to #2.")
	must(t, err)
	ca, err := bob.AttachToComment(ctx, "root", "src", c.ID, "shot.png", bytes.NewReader([]byte("\x89PNG fake")))
	must(t, err)
	must(t, bob.EditComment(ctx, "root", "src", c.ID, "Same here, related to #2. ![shot](/attachments/"+ca.UUID+")"))
	must(t, bob.React(ctx, "root", "src", is.Number, 0, "+1"))
	must(t, alice.React(ctx, "root", "src", is.Number, c.ID, "heart"))
	_, err = alice.CreateComment(ctx, "root", "src", is.Number, "Fixed in my branch, `#1` is literal code.")
	must(t, err)
	is2, err := bob.CreateIssue(ctx, "root", "src", gitea.NewIssue{Title: "Old report", Body: "Duplicate of #1, also see #9."})
	must(t, err)
	_, err = bob.EditIssue(ctx, "root", "src", is2.Number, map[string]any{"state": "closed"})
	must(t, err)
	// #1 depends on #2.
	must(t, s.Admin.AddDependency(ctx, "root", "src", is.Number, "root", "src", is2.Number))
}

func TestMoveSameInstanceAsAuthors(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", nil)
	must(t, err)
	if len(res) != 2 {
		t.Fatalf("results %+v", res)
	}
	n1, n2 := res[0].To, res[1].To
	d := s.Admin
	i1, err := d.Issue(ctx, "root", "dst", n1)
	must(t, err)
	if i1.Title != "Crash on save" || i1.User.Login != "alice" || len(i1.Labels) != 1 || i1.Labels[0].Name != "bug" ||
		i1.Milestone == nil || i1.Milestone.Title != "v2" || len(i1.Assignees) != 1 || i1.Assignees[0].Login != "bob" {
		t.Fatalf("issue 1: %+v", i1)
	}
	if len(i1.Assets) != 1 || !strings.Contains(i1.Body, i1.Assets[0].UUID) || !strings.Contains(i1.Body, "Moved from [root/src](") {
		t.Fatalf("issue 1 body/attachment:\n%s\n%+v", i1.Body, i1.Assets)
	}
	data, err := d.DownloadAttachment(ctx, i1.Assets[0].UUID)
	must(t, err)
	if string(data) != "stack trace here" {
		t.Fatalf("attachment %q", data)
	}
	cs, err := d.Comments(ctx, "root", "dst", n1)
	must(t, err)
	if len(cs) != 2 || cs[0].User.Login != "bob" || cs[1].User.Login != "alice" {
		t.Fatalf("comments %+v", cs)
	}
	if !strings.Contains(cs[0].Body, fmt.Sprintf("related to #%d", n2)) || len(cs[0].Assets) != 1 || !strings.Contains(cs[0].Body, cs[0].Assets[0].UUID) {
		t.Fatalf("comment 1:\n%s", cs[0].Body)
	}
	if !strings.Contains(cs[1].Body, "`#1` is literal code") {
		t.Fatalf("code was rewritten:\n%s", cs[1].Body)
	}
	rs, err := d.IssueReactions(ctx, "root", "dst", n1)
	must(t, err)
	if len(rs) != 1 || rs[0].User.Login != "bob" || rs[0].Content != "+1" {
		t.Fatalf("reactions %+v", rs)
	}
	crs, err := d.CommentReactions(ctx, "root", "dst", cs[0].ID)
	must(t, err)
	if len(crs) != 1 || crs[0].User.Login != "alice" {
		t.Fatalf("comment reactions %+v", crs)
	}
	i2, err := d.Issue(ctx, "root", "dst", n2)
	must(t, err)
	if i2.State != "closed" || !strings.Contains(i2.Body, fmt.Sprintf("Duplicate of #%d", n1)) || !strings.Contains(i2.Body, "root/src#9") {
		t.Fatalf("issue 2: %s\n%s", i2.State, i2.Body)
	}
	deps, err := d.Dependencies(ctx, "root", "dst", n1)
	must(t, err)
	if len(deps) != 1 || deps[0].Number != n2 || deps[0].Repository.FullName != "root/dst" {
		t.Fatalf("dependencies %+v", deps)
	}
	// The originals are closed and point to the copies.
	for _, r := range res {
		o, err := d.Issue(ctx, "root", "src", r.From)
		must(t, err)
		oc, err := d.Comments(ctx, "root", "src", r.From)
		must(t, err)
		last := oc[len(oc)-1].Body
		if o.State != "closed" || !strings.Contains(last, fmt.Sprintf("Moved to root/dst#%d", r.To)) {
			t.Fatalf("original #%d: %s, last comment %q", r.From, o.State, last)
		}
	}
	// A second run finds everything and adds nothing.
	res2, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", nil)
	must(t, err)
	all, err := d.Issues(ctx, "root", "dst", "all", "")
	must(t, err)
	cs2, _ := d.Comments(ctx, "root", "dst", n1)
	oc, _ := d.Comments(ctx, "root", "src", 1)
	if len(all) != 2 || len(cs2) != 2 || !res2[0].Resumed || strings.Count(fmt.Sprint(bodies(oc)), "Moved to") != 1 {
		t.Fatalf("second run duplicated: %d issues, %d comments, %d source comments", len(all), len(cs2), len(oc))
	}
}

func bodies(cs []gitea.Comment) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

func TestMoveWithoutAdminAttributes(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	alice := s.Client("alice")
	res, err := doMove(t, alice, alice, "root/src#1", "root/dst", nil)
	must(t, err)
	i, err := s.Admin.Issue(ctx, "root", "dst", res[0].To)
	must(t, err)
	if i.User.Login != "alice" || !strings.Contains(i.Body, "Opened by **@alice** on ") {
		t.Fatalf("issue:\n%s", i.Body)
	}
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if cs[0].User.Login != "alice" || !strings.Contains(cs[0].Body, "Written by **@bob** on ") {
		t.Fatalf("comment by %s:\n%s", cs[0].User.Login, cs[0].Body)
	}
}

func TestResumeAfterAFailureDuplicatesNothing(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	comments := 0
	_, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", func(what string) error {
		if strings.HasPrefix(what, "comment") {
			comments++
			if comments == 1 {
				return errors.New("injected failure")
			}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("got %v", err)
	}
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", nil)
	must(t, err)
	all, _ := s.Admin.Issues(ctx, "root", "dst", "all", "")
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if len(all) != 2 || len(cs) != 2 || !res[0].Resumed {
		t.Fatalf("after resume: %d issues, %d comments, %+v", len(all), len(cs), res)
	}
}

func TestMoveToAnotherInstance(t *testing.T) {
	a := testkit.Start(t)
	b := testkit.Start(t)
	seed(t, a)
	b.User("alice", false) // same name and email: the same person
	b.Repo("dst")
	b.Call("PUT", "/repos/root/dst/collaborators/alice", map[string]any{"permission": "write"}, nil)
	res, err := doMove(t, a.Admin, b.Admin, "root/src#1", "root/dst", nil)
	must(t, err)
	i, err := b.Admin.Issue(ctx, "root", "dst", res[0].To)
	must(t, err)
	if i.User.Login != "alice" || len(i.Assets) != 1 {
		t.Fatalf("issue by %s with %d assets", i.User.Login, len(i.Assets))
	}
	cs, _ := b.Admin.Comments(ctx, "root", "dst", res[0].To)
	// bob does not exist on b: his comment is posted by root and names him.
	if cs[0].User.Login != "root" || !strings.Contains(cs[0].Body, "Written by **@bob**") ||
		!strings.Contains(cs[0].Body, "[root/src#2]("+a.URL+"/root/src/issues/2)") {
		t.Fatalf("comment by %s:\n%s", cs[0].User.Login, cs[0].Body)
	}
	oc, _ := a.Admin.Comments(ctx, "root", "src", 1)
	if last := oc[len(oc)-1].Body; !strings.Contains(last, b.URL+"/root/dst/issues/") {
		t.Fatalf("source link %q", last)
	}
}
