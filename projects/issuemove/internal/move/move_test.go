package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/marker"
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
	return doMoveIn(t, plan.Input{Src: src, Dst: dst}, source, target, hook)
}

func doMoveIn(t *testing.T, in plan.Input, source, target string, hook func(string) error) ([]Result, error) {
	t.Helper()
	p := planOf(t, in, source, target)
	if !p.OK() {
		t.Fatalf("plan blocked: %q", p.Blockers)
	}
	return Run(ctx, Options{Src: in.Src, Dst: in.Dst, Plan: p, hook: hook})
}

func planOf(t *testing.T, in plan.Input, source, target string) *plan.Plan {
	t.Helper()
	var err error
	in.Source, err = ref.ParseSource(source)
	must(t, err)
	in.Target, _ = ref.ParseRepo(target)
	if len(in.Source.Numbers) == 0 && in.State == "" {
		in.State = "all"
	}
	p, err := plan.Build(ctx, in)
	must(t, err)
	return p
}

// failAt fails the run the first time what is reached.
func failAt(what string) func(string) error {
	done := false
	return func(w string) error {
		if w == what && !done {
			done = true
			return errors.New("injected failure at " + w)
		}
		return nil
	}
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
	if len(i1.Assets) != 1 || !strings.Contains(i1.Body, i1.Assets[0].UUID) || !strings.Contains(i1.Body, "Moved from [root/src#1](") {
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

// TestResumeAfterAFailureDuplicatesNothing stops a run right after bob's
// comment is created (before its attachment, its link to #2's copy and its
// reaction) and checks the rerun finishes all of it.
func TestResumeAfterAFailureDuplicatesNothing(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	scs, _ := s.Admin.Comments(ctx, "root", "src", 1)
	_, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", failAt(fmt.Sprintf("comment %d", scs[0].ID)))
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("got %v", err)
	}
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", nil)
	must(t, err)
	all, _ := s.Admin.Issues(ctx, "root", "dst", "all", "")
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if len(all) != 2 || len(cs) != 2 || !res[0].Resumed || res[1].Resumed {
		t.Fatalf("after resume: %d issues, %d comments, %+v", len(all), len(cs), res)
	}
	if len(cs[0].Assets) != 1 || !strings.Contains(cs[0].Body, cs[0].Assets[0].UUID) ||
		!strings.Contains(cs[0].Body, fmt.Sprintf("related to #%d", res[1].To)) {
		t.Fatalf("resumed comment:\n%s\n%+v", cs[0].Body, cs[0].Assets)
	}
	crs, _ := s.Admin.CommentReactions(ctx, "root", "dst", cs[0].ID)
	if len(crs) != 1 || crs[0].User.Login != "alice" {
		t.Fatalf("comment reactions after resume: %+v", crs)
	}
}

// TestResumeRightAfterCreatingTheIssue stops before anything but the bare
// issue exists, with an author who cannot write to the target (so Gitea
// drops the labels, milestone and assignees she sends).
func TestResumeRightAfterCreatingTheIssue(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	s.Call("PUT", "/repos/root/dst/collaborators/alice", map[string]any{"permission": "read"}, nil)
	_, err := doMove(t, s.Admin, s.Admin, "root/src#1", "root/dst", failAt("issue 1"))
	if err == nil {
		t.Fatal("no failure")
	}
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1", "root/dst", nil)
	must(t, err)
	i, err := s.Admin.Issue(ctx, "root", "dst", res[0].To)
	must(t, err)
	if i.User.Login != "alice" || len(i.Labels) != 1 || i.Milestone == nil || len(i.Assignees) != 1 {
		t.Fatalf("labels %v milestone %v assignees %v", i.Labels, i.Milestone, i.Assignees)
	}
	if len(i.Assets) != 1 || !strings.Contains(i.Body, i.Assets[0].UUID) {
		t.Fatalf("attachment:\n%s", i.Body)
	}
	rs, _ := s.Admin.IssueReactions(ctx, "root", "dst", res[0].To)
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if len(rs) != 1 || len(cs) != 2 {
		t.Fatalf("%d reactions, %d comments", len(rs), len(cs))
	}
}

// TestNonWriterAuthorKeepsMetadata: the author can open issues in the
// target but not label them; the copy still gets everything.
func TestNonWriterAuthorKeepsMetadata(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	s.Call("PUT", "/repos/root/dst/collaborators/alice", map[string]any{"permission": "read"}, nil)
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1", "root/dst", nil)
	must(t, err)
	i, err := s.Admin.Issue(ctx, "root", "dst", res[0].To)
	must(t, err)
	if i.User.Login != "alice" || len(i.Labels) != 1 || i.Labels[0].Name != "bug" || i.Milestone == nil ||
		i.Milestone.Title != "v2" || len(i.Assignees) != 1 || i.Assignees[0].Login != "bob" {
		t.Fatalf("copy by %s: labels %v milestone %v assignees %v", i.User.Login, i.Labels, i.Milestone, i.Assignees)
	}
}

// TestAuthorWithoutAccessIsNamed: in a private target alice cannot post,
// so her issue is posted by the token's user and names her.
func TestAuthorWithoutAccessIsNamed(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	s.Call("PATCH", "/repos/root/dst", map[string]any{"private": true}, nil)
	s.Call("DELETE", "/repos/root/dst/collaborators/alice", nil, nil)
	p := planOf(t, plan.Input{Src: s.Admin, Dst: s.Admin}, "root/src#1", "root/dst")
	if p.Authors["alice"] != "" || !strings.Contains(strings.Join(p.Warnings, "\n"), "alice cannot see root/dst") {
		t.Fatalf("authors %v, warnings %q", p.Authors, p.Warnings)
	}
	res, err := Run(ctx, Options{Src: s.Admin, Dst: s.Admin, Plan: p})
	must(t, err)
	i, _ := s.Admin.Issue(ctx, "root", "dst", res[0].To)
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if i.User.Login != "root" || !strings.Contains(i.Body, "Opened by **@alice**") || cs[0].User.Login != "bob" || cs[1].User.Login != "root" {
		t.Fatalf("issue by %s:\n%s\ncomments by %s, %s", i.User.Login, i.Body, cs[0].User.Login, cs[1].User.Login)
	}
}

// TestRerunWithAnotherSourceUserKeepsCopiesOpen: a rerun that does not
// recognise the "Moved to" note (another token wrote it) must not close
// the copy of an issue that was open, although the original is closed now.
func TestRerunWithAnotherSourceUserKeepsCopiesOpen(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1", "root/dst", nil)
	must(t, err)
	alice := s.Client("alice")
	res2, err := doMoveIn(t, plan.Input{Src: alice, Dst: s.Admin}, "root/src#1", "root/dst", nil)
	must(t, err)
	i, _ := s.Admin.Issue(ctx, "root", "dst", res[0].To)
	all, _ := s.Admin.Issues(ctx, "root", "dst", "all", "")
	if i.State != "open" || len(all) != 1 || res2[0].To != res[0].To {
		t.Fatalf("copy %s, %d issues in the target", i.State, len(all))
	}
}

// TestSpoofedMarkersAreIgnored: issues and comments that merely contain
// issuemove's markers are not taken for copies.
func TestSpoofedMarkersAreIgnored(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	s.User("mallory", false)
	mallory := s.Admin.As("mallory")
	p := planOf(t, plan.Input{Src: s.Admin, Dst: s.Admin}, "root/src#1-2", "root/dst")
	key := p.SourceKey
	// A fake copy of #2 in the target.
	fake, err := mallory.CreateIssue(ctx, "root", "dst", gitea.NewIssue{Title: "Send your password",
		Body: "phish\n\n" + marker.Make("issue", key+"#2 state=open")})
	must(t, err)
	// A source issue whose text claims to be the copy of #2, and a comment
	// claiming #1 was moved elsewhere.
	m3, err := mallory.CreateIssue(ctx, "root", "src", gitea.NewIssue{Title: "Mine",
		Body: "hi\n\n" + marker.Make("issue", key+"#2 state=open")})
	must(t, err)
	_, err = mallory.CreateComment(ctx, "root", "src", 1, "see "+marker.Make("moved-to", "evil/repo#999"))
	must(t, err)

	if p := planOf(t, plan.Input{Src: s.Admin, Dst: s.Admin}, "root/src#1", "root/dst"); p.Issues[0].MovedTo != "" || !p.OK() {
		t.Fatalf("a comment by mallory counted as a move note: %q %q", p.Issues[0].MovedTo, p.Blockers)
	}
	r3, err := doMove(t, s.Admin, s.Admin, fmt.Sprintf("root/src#%d", m3.Number), "root/dst", nil)
	must(t, err)
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1-2", "root/dst", nil)
	must(t, err)
	if res[1].To == fake.Number || res[1].To == r3[0].To || res[1].Resumed {
		t.Fatalf("#2 went to %d (fake %d, mallory's copy %d)", res[1].To, fake.Number, r3[0].To)
	}
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if len(cs) != 3 || !strings.Contains(cs[2].Body, "issuemove (quoted):moved-to=evil") {
		t.Fatalf("%d comments copied; last:\n%s", len(cs), cs[len(cs)-1].Body)
	}
	fcs, _ := s.Admin.Comments(ctx, "root", "dst", fake.Number)
	if len(fcs) != 0 {
		t.Fatalf("comments posted under the fake copy: %d", len(fcs))
	}
}

// TestSpoofedCommentMarkerIsIgnored: on a rerun, a comment someone else
// added to the copy with a comment marker does not stand for the original.
func TestSpoofedCommentMarkerIsIgnored(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	s.User("mallory", false)
	_, err := doMove(t, s.Admin, s.Admin, "root/src#1", "root/dst", failAt("issue 1"))
	if err == nil {
		t.Fatal("no failure")
	}
	p := planOf(t, plan.Input{Src: s.Admin, Dst: s.Admin}, "root/src#1", "root/dst")
	all, _ := s.Admin.Issues(ctx, "root", "dst", "all", "")
	scs, _ := s.Admin.Comments(ctx, "root", "src", 1)
	_, err = s.Admin.As("mallory").CreateComment(ctx, "root", "dst", all[0].Number,
		"fake\n\n"+marker.Make("comment", fmt.Sprintf("%s#1/%d", p.SourceKey, scs[0].ID)))
	must(t, err)
	res, err := doMove(t, s.Admin, s.Admin, "root/src#1", "root/dst", nil)
	must(t, err)
	cs, _ := s.Admin.Comments(ctx, "root", "dst", res[0].To)
	if len(cs) != 3 || cs[1].User.Login != "bob" {
		t.Fatalf("%d comments; bob's copied: %v", len(cs), len(cs) > 1 && cs[1].User.Login == "bob")
	}
}

// TestIssuesLeftBehindDependOnTheCopy: #1 depends on #2; moving #2 alone
// makes #1 depend on the copy, so closing the original does not unblock it.
func TestIssuesLeftBehindDependOnTheCopy(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	res, err := doMove(t, s.Admin, s.Admin, "root/src#2", "root/dst", nil)
	must(t, err)
	deps, err := s.Admin.Dependencies(ctx, "root", "src", 1)
	must(t, err)
	found := false
	for _, d := range deps {
		found = found || (d.Repository.FullName == "root/dst" && d.Number == res[0].To)
	}
	if !found {
		t.Fatalf("#1 depends on %+v", deps)
	}
}

// TestAlreadyMovedElsewhereBlocks: an issue moved to one repository is not
// copied again to another.
func TestAlreadyMovedElsewhereBlocks(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	s.Repo("dst2")
	_, err := doMove(t, s.Admin, s.Admin, "root/src#2", "root/dst", nil)
	must(t, err)
	p := planOf(t, plan.Input{Src: s.Admin, Dst: s.Admin}, "root/src#2", "root/dst2")
	if p.OK() || !strings.Contains(strings.Join(p.Blockers, "\n"), "already moved to root/dst#") {
		t.Fatalf("blockers %q", p.Blockers)
	}
}

func TestMoveToAnotherInstance(t *testing.T) {
	a := testkit.Start(t)
	b := testkit.Start(t)
	seed(t, a)
	b.User("alice", false) // same name and email: the same person
	b.Repo("dst")
	b.Call("PUT", "/repos/root/dst/collaborators/alice", map[string]any{"permission": "write"}, nil)
	res, err := doMoveIn(t, plan.Input{Src: a.Admin, Dst: b.Admin, MatchUsers: true}, "root/src#1", "root/dst", nil)
	must(t, err)
	i, err := b.Admin.Issue(ctx, "root", "dst", res[0].To)
	must(t, err)
	if i.User.Login != "alice" || len(i.Assets) != 1 {
		t.Fatalf("issue by %s with %d assets", i.User.Login, len(i.Assets))
	}
	cs, _ := b.Admin.Comments(ctx, "root", "dst", res[0].To)
	// bob does not exist on b: his comment is posted by root and names him
	// (without mentioning whoever is called bob there).
	if cs[0].User.Login != "root" || !strings.Contains(cs[0].Body, "Written by **@\u200bbob**") ||
		!strings.Contains(cs[0].Body, "[root/src#2]("+a.URL+"/root/src/issues/2)") {
		t.Fatalf("comment by %s:\n%s", cs[0].User.Login, cs[0].Body)
	}
	oc, _ := a.Admin.Comments(ctx, "root", "src", 1)
	if last := oc[len(oc)-1].Body; !strings.Contains(last, b.URL+"/root/dst/issues/") {
		t.Fatalf("source link %q", last)
	}
}

// TestRerunThroughAnotherURLDuplicatesNothing repeats a move through a
// different address for the same server.
func TestRerunThroughAnotherURLDuplicatesNothing(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	_, err := doMove(t, s.Admin, s.Admin, "root/src#2", "root/dst", nil)
	must(t, err)
	other := gitea.New(strings.Replace(s.URL, "127.0.0.1", "localhost", 1), s.Admin.Token)
	res, err := doMove(t, other, other, "root/src#2", "root/dst", nil)
	must(t, err)
	all, _ := s.Admin.Issues(ctx, "root", "dst", "all", "")
	if len(all) != 1 || !res[0].Resumed {
		t.Fatalf("%d issues in the target after a rerun via localhost", len(all))
	}
}
