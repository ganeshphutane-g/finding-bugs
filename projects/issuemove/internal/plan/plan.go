// Package plan works out what moving issues would do, without changing
// anything.
package plan

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/ref"
)

// Input selects what to move.
type Input struct {
	Src, Dst *gitea.Client
	Source   ref.Source
	Target   ref.Repo
	State    string // with no numbers: open, closed or all
	Labels   string // with no numbers: comma-separated label filter
}

// Issue is one issue to move.
type Issue struct {
	Issue       gitea.Issue                `json:"issue"`
	Comments    []gitea.Comment            `json:"comments"`
	Reactions   []gitea.Reaction           `json:"reactions"`
	CommentReac map[int64][]gitea.Reaction `json:"comment_reactions,omitempty"`
	Attachments int                        `json:"attachments"`
	Bytes       int64                      `json:"attachment_bytes"`
	Assignees   []string                   `json:"assignees_kept"`
	Dropped     []string                   `json:"assignees_dropped,omitempty"`
	DependsOn   []gitea.Dependency         `json:"depends_on,omitempty"`
	MovedTo     string                     `json:"already_moved_to,omitempty"` // from an earlier run
}

// Plan is the whole move.
type Plan struct {
	Source       ref.Repo `json:"source"`
	Target       ref.Repo `json:"target"`
	SameInstance bool     `json:"same_instance"`
	// SourceKey identifies the source repository in markers: the server's
	// own host name (from its root URL) and the repository's ID, so a
	// second run finds earlier copies whatever URL is used and after a
	// rename.
	SourceKey string  `json:"source_key"`
	AsAuthors bool    `json:"as_original_authors"` // Sudo is possible
	Issues    []Issue `json:"issues"`

	LabelMap      map[string]int64  `json:"label_map"`        // name -> existing target label id
	NewLabels     []gitea.Label     `json:"labels_to_create"` // missing in the target
	MilestoneMap  map[string]int64  `json:"milestone_map"`
	NewMilestones []gitea.Milestone `json:"milestones_to_create"`
	// Authors maps each source user to the target user content is posted as
	// ("" when it is attributed in the text instead).
	Authors map[string]string `json:"authors"`

	Blockers []string `json:"blockers"`
	Warnings []string `json:"warnings"`
}

// OK reports whether nothing blocks the move.
func (p *Plan) OK() bool { return len(p.Blockers) == 0 }

func (p *Plan) block(f string, a ...any) { p.Blockers = append(p.Blockers, fmt.Sprintf(f, a...)) }
func (p *Plan) warn(f string, a ...any)  { p.Warnings = append(p.Warnings, fmt.Sprintf(f, a...)) }

func sameInstance(a, b *gitea.Client) bool {
	ua, err1 := url.Parse(a.BaseURL)
	ub, err2 := url.Parse(b.BaseURL)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.Host, ub.Host) && strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/")
}

// Build makes the plan. An error means something could not be read.
func Build(ctx context.Context, in Input) (*Plan, error) {
	p := &Plan{Source: ref.Repo{Owner: in.Source.Owner, Name: in.Source.Repo}, Target: in.Target,
		SameInstance: sameInstance(in.Src, in.Dst), LabelMap: map[string]int64{}, MilestoneMap: map[string]int64{},
		Authors: map[string]string{}}
	if p.SameInstance && strings.EqualFold(p.Source.String(), p.Target.String()) {
		return nil, fmt.Errorf("the source and target are the same repository")
	}

	srcRepo, err := in.Src.Repo(ctx, p.Source.Owner, p.Source.Name)
	if err != nil {
		return nil, fmt.Errorf("source repository %s: %w", p.Source, err)
	}
	host := srcRepo.HTMLURL
	if u, err := url.Parse(srcRepo.HTMLURL); err == nil && u.Host != "" {
		host = u.Host
	}
	p.SourceKey = fmt.Sprintf("%s/repo-%d", host, srcRepo.ID)
	dstRepo, err := in.Dst.Repo(ctx, p.Target.Owner, p.Target.Name)
	if err != nil {
		return nil, fmt.Errorf("target repository %s: %w", p.Target, err)
	}
	dstMe, err := in.Dst.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("the target token: %w", err)
	}
	srcMe, err := in.Src.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("the source token: %w", err)
	}
	p.AsAuthors = dstMe.IsAdmin

	switch {
	case !dstRepo.HasIssues:
		p.block("issues are turned off in %s", p.Target)
	case dstRepo.Archived:
		p.block("%s is archived", p.Target)
	}
	if srcRepo.Archived {
		p.block("%s is archived, so the original issues cannot be closed with a link; unarchive it first", p.Source)
	}
	canWriteDst := dstRepo.Permissions.Push || dstRepo.Permissions.Admin || dstMe.IsAdmin
	canWriteSrc := srcRepo.Permissions.Push || srcRepo.Permissions.Admin || srcMe.IsAdmin
	if !canWriteSrc {
		p.block("the source token (%s) cannot write to %s, so it cannot close the originals; use a token with write access", srcMe.Login, p.Source)
	}

	// The issues.
	var issues []gitea.Issue
	if len(in.Source.Numbers) > 0 {
		for _, n := range in.Source.Numbers {
			is, err := in.Src.Issue(ctx, p.Source.Owner, p.Source.Name, n)
			if gitea.IsNotFound(err) {
				p.block("%s#%d does not exist", p.Source, n)
				continue
			}
			if err != nil {
				return nil, err
			}
			if is.PullRequest != nil {
				p.block("%s#%d is a pull request; issuemove moves issues only", p.Source, n)
				continue
			}
			issues = append(issues, *is)
		}
	} else {
		state := in.State
		if state == "" {
			state = "open"
		}
		if issues, err = in.Src.Issues(ctx, p.Source.Owner, p.Source.Name, state, in.Labels); err != nil {
			return nil, err
		}
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	}
	if len(issues) == 0 && len(p.Blockers) == 0 {
		p.block("no issues match")
	}

	// What the target has.
	dstLabels := map[string]gitea.Label{}
	labels, err := in.Dst.Labels(ctx, p.Target.Owner, p.Target.Name)
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		dstLabels[strings.ToLower(l.Name)] = l
	}
	if org, err := in.Dst.OrgLabels(ctx, p.Target.Owner); err == nil {
		for _, l := range org {
			if _, ok := dstLabels[strings.ToLower(l.Name)]; !ok {
				dstLabels[strings.ToLower(l.Name)] = l
			}
		}
	}
	dstMiles := map[string]gitea.Milestone{}
	miles, err := in.Dst.Milestones(ctx, p.Target.Owner, p.Target.Name)
	if err != nil {
		return nil, err
	}
	for _, m := range miles {
		dstMiles[m.Title] = m
	}
	assignable := map[string]bool{}
	if as, err := in.Dst.Assignees(ctx, p.Target.Owner, p.Target.Name); err == nil {
		for _, u := range as {
			assignable[strings.ToLower(u.Login)] = true
		}
	}

	users := map[string]gitea.User{}
	newLabel := map[string]bool{}
	newMile := map[string]bool{}
	for _, is := range issues {
		ip := Issue{Issue: is, CommentReac: map[int64][]gitea.Reaction{}}
		addUser(users, is.User)
		if ip.Comments, err = in.Src.Comments(ctx, p.Source.Owner, p.Source.Name, is.Number); err != nil {
			return nil, err
		}
		if ip.Reactions, err = in.Src.IssueReactions(ctx, p.Source.Owner, p.Source.Name, is.Number); err != nil {
			return nil, err
		}
		for _, r := range ip.Reactions {
			addUser(users, r.User)
		}
		for _, a := range is.Assets {
			ip.Attachments++
			ip.Bytes += a.Size
		}
		for _, c := range ip.Comments {
			addUser(users, c.User)
			for _, a := range c.Assets {
				ip.Attachments++
				ip.Bytes += a.Size
			}
			rs, err := in.Src.CommentReactions(ctx, p.Source.Owner, p.Source.Name, c.ID)
			if err != nil {
				return nil, err
			}
			if len(rs) > 0 {
				ip.CommentReac[c.ID] = rs
			}
			for _, r := range rs {
				addUser(users, r.User)
			}
		}
		for _, l := range is.Labels {
			if t, ok := dstLabels[strings.ToLower(l.Name)]; ok {
				p.LabelMap[l.Name] = t.ID
			} else if !newLabel[strings.ToLower(l.Name)] {
				newLabel[strings.ToLower(l.Name)] = true
				p.NewLabels = append(p.NewLabels, l)
			}
		}
		if m := is.Milestone; m != nil {
			if t, ok := dstMiles[m.Title]; ok {
				p.MilestoneMap[m.Title] = t.ID
			} else if !newMile[m.Title] {
				newMile[m.Title] = true
				p.NewMilestones = append(p.NewMilestones, *m)
			}
		}
		for _, a := range is.Assignees {
			addUser(users, a)
		}
		for _, c := range ip.Comments {
			if i := strings.Index(c.Body, "<!-- issuemove:moved-to="); i >= 0 {
				rest := c.Body[i+len("<!-- issuemove:moved-to="):]
				if end := strings.Index(rest, " -->"); end > 0 {
					ip.MovedTo = rest[:end]
				}
			}
		}
		if ip.DependsOn, err = in.Src.Dependencies(ctx, p.Source.Owner, p.Source.Name, is.Number); err != nil && !gitea.IsNotFound(err) {
			return nil, err
		}
		p.Issues = append(p.Issues, ip)
	}
	if (len(p.NewLabels) > 0 || len(p.NewMilestones) > 0) && !canWriteDst {
		p.block("the target token (%s) cannot create labels or milestones in %s; create them first or use a token with write access", dstMe.Login, p.Target)
	}

	// Authors: posted as themselves when possible, else named in the text.
	for login, u := range users {
		p.Authors[login] = ""
		if !p.AsAuthors || u.ID <= 0 {
			continue // no admin token, or a deleted user ("Ghost")
		}
		if p.SameInstance {
			p.Authors[login] = login
			continue
		}
		// On another instance the same name may be someone else: also
		// require the same email.
		tu, err := in.Dst.User(ctx, login)
		if err == nil && u.Email != "" && strings.EqualFold(tu.Email, u.Email) {
			p.Authors[login] = tu.Login
		}
	}
	// Assignees: only someone known to be the same account on the target,
	// and assignable there.
	for i := range p.Issues {
		ip := &p.Issues[i]
		for _, a := range ip.Issue.Assignees {
			target := a.Login
			if !p.SameInstance {
				target = p.Authors[a.Login]
			}
			if target != "" && assignable[strings.ToLower(target)] {
				ip.Assignees = append(ip.Assignees, target)
			} else {
				ip.Dropped = append(ip.Dropped, a.Login)
			}
		}
	}
	if !p.AsAuthors {
		p.warn("the target token is not a site admin, so everything is posted as %s, with the original author and date at the top of each issue and comment", dstMe.Login)
	}
	for _, ip := range p.Issues {
		if ip.MovedTo != "" {
			p.warn("%s#%d was already moved to %s by an earlier run; that copy is finished, not duplicated", p.Source, ip.Issue.Number, ip.MovedTo)
		}
		if len(ip.Dropped) > 0 {
			p.warn("%s#%d: assignees %s cannot be assigned in %s; they are named in the text", p.Source, ip.Issue.Number, strings.Join(ip.Dropped, ", "), p.Target)
		}
	}
	return p, nil
}

// addUser records a user, keeping the most complete record: user objects
// inside reactions, for instance, come without an email.
func addUser(users map[string]gitea.User, u gitea.User) {
	if have, ok := users[u.Login]; ok && (have.Email != "" || u.Email == "") {
		return
	}
	users[u.Login] = u
}
