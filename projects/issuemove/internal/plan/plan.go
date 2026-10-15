// Package plan works out what moving issues would do, without changing
// anything.
package plan

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/marker"
	"github.com/ganeshphutane-g/issuemove/internal/ref"
)

// Input selects what to move.
type Input struct {
	Src, Dst *gitea.Client
	Source   ref.Source
	Target   ref.Repo
	State    string // with no numbers: open, closed or all
	Labels   string // with no numbers: comma-separated label filter

	KeepOpen bool // leave the originals open
	Lock     bool // lock the originals
	// MatchUsers, on another instance, posts as the target account with the
	// same name and email (otherwise authors are named in the text).
	MatchUsers bool
	// Progress, when set, is told how many issues have been read.
	Progress func(done, total int)
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
	Blocks      []gitea.Dependency         `json:"blocks,omitempty"`           // issues that depend on this one
	MovedTo     string                     `json:"already_moved_to,omitempty"` // by an earlier run
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
	// SourceUser and TargetUser are the tokens' users: only text they (or
	// the original authors) posted is trusted to carry markers.
	SourceUser string `json:"source_user"`
	TargetUser string `json:"target_user"`
	KeepOpen   bool   `json:"keep_open"`
	Lock       bool   `json:"lock"`

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
		Authors: map[string]string{}, KeepOpen: in.KeepOpen, Lock: in.Lock}
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
	p.SourceUser, p.TargetUser = srcMe.Login, dstMe.Login

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

	if srcRepo.Private && !dstRepo.Private {
		p.warn("%s is private and %s is not: the moved issues, comments and attachments become visible to everyone who can see %s", p.Source, p.Target, p.Target)
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
		if in.Labels != "" {
			// Gitea ignores label names it does not know, which would
			// select every issue.
			known := map[string]bool{}
			ls, err := in.Src.Labels(ctx, p.Source.Owner, p.Source.Name)
			if err != nil {
				return nil, err
			}
			if org, err := in.Src.OrgLabels(ctx, p.Source.Owner); err == nil {
				ls = append(ls, org...)
			}
			for _, l := range ls {
				known[strings.ToLower(l.Name)] = true
			}
			for _, name := range strings.Split(in.Labels, ",") {
				if name = strings.TrimSpace(name); !known[strings.ToLower(name)] {
					p.block("%s has no label %q", p.Source, name)
				}
			}
			if !p.OK() {
				return p, nil
			}
		}
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
	for i, is := range issues {
		if in.Progress != nil {
			in.Progress(i+1, len(issues))
		}
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
			if to, ok := MoveNote(c, p.SourceUser); ok {
				ip.MovedTo = to
			}
		}
		if ip.DependsOn, err = in.Src.Dependencies(ctx, p.Source.Owner, p.Source.Name, is.Number); err != nil && !gitea.IsNotFound(err) {
			return nil, err
		}
		if ip.Blocks, err = in.Src.Blocks(ctx, p.Source.Owner, p.Source.Name, is.Number); err != nil && !gitea.IsNotFound(err) {
			return nil, err
		}
		p.Issues = append(p.Issues, ip)
	}
	needsWrite := len(p.NewLabels) > 0 || len(p.NewMilestones) > 0
	for _, ip := range p.Issues {
		is := ip.Issue
		needsWrite = needsWrite || len(is.Labels) > 0 || is.Milestone != nil || len(is.Assignees) > 0 || is.Due != nil
	}
	if needsWrite && !canWriteDst {
		p.block("the target token (%s) cannot write to %s, so it cannot set labels, milestones, assignees or due dates; use a token with write access", dstMe.Login, p.Target)
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
		// On another instance the same name may be someone else: only on
		// request, and with the same (real) email.
		if !in.MatchUsers || u.Email == "" || noReply(u.Email) {
			continue
		}
		tu, err := in.Dst.User(ctx, login)
		if err == nil && strings.EqualFold(tu.Email, u.Email) {
			p.Authors[login] = tu.Login
		}
	}
	// Someone who cannot see the target cannot post there: their text is
	// posted by the token's user and names them.
	canPost := map[string]bool{}
	for login, as := range p.Authors {
		if as == "" || strings.EqualFold(as, dstMe.Login) {
			continue
		}
		ok, seen := canPost[as]
		if !seen {
			_, err := in.Dst.As(as).Repo(ctx, p.Target.Owner, p.Target.Name)
			ok = err == nil
			canPost[as] = ok
			if !ok {
				p.warn("%s cannot see %s, so what they wrote is posted by %s and names them", as, p.Target, dstMe.Login)
			}
		}
		if !ok {
			p.Authors[login] = ""
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
	} else if !p.SameInstance && !in.MatchUsers {
		p.warn("on another instance a name can belong to someone else, so authors are named in the text; --match-users posts as the accounts with the same name and email")
	}
	dropped, mentions := 0, false
	for _, ip := range p.Issues {
		rs := ip.Reactions
		for _, r := range ip.CommentReac {
			rs = append(rs, r...)
		}
		for _, r := range rs {
			if p.Authors[r.User.Login] == "" {
				dropped++
			}
		}
		mentions = mentions || mentionLike.MatchString(ip.Issue.Body)
		for _, c := range ip.Comments {
			mentions = mentions || mentionLike.MatchString(c.Body)
		}
	}
	if dropped > 0 {
		p.warn("%d reaction(s) are not copied: a reaction can only be added as its own user, which needs an admin token (and, on another instance, the same account there)", dropped)
	}
	if mentions {
		p.warn("people @mentioned in the moved text are notified again when the copies are posted")
	}
	for _, ip := range p.Issues {
		if ip.MovedTo != "" {
			if strings.HasPrefix(strings.ToLower(ip.MovedTo), strings.ToLower(p.Target.String())+"#") {
				p.warn("%s#%d was already moved to %s by an earlier run; that copy is finished, not duplicated", p.Source, ip.Issue.Number, ip.MovedTo)
			} else {
				p.block("%s#%d was already moved to %s; move that issue instead", p.Source, ip.Issue.Number, ip.MovedTo)
			}
		}
		if len(ip.Dropped) > 0 {
			p.warn("%s#%d: assignees %s cannot be assigned in %s; they are named in the text", p.Source, ip.Issue.Number, strings.Join(ip.Dropped, ", "), p.Target)
		}
	}
	return p, nil
}

var mentionLike = regexp.MustCompile(`(^|[\s(\[>"'*_~])@[0-9A-Za-z_-]`)

// noReply reports an address that is not a real mailbox (Gitea's
// placeholder for hidden emails), which anyone could claim elsewhere.
func noReply(email string) bool {
	at := strings.LastIndex(email, "@")
	return at < 0 || strings.Contains(strings.ToLower(email[at:]), "noreply")
}

// MoveNote reads the "Moved to" note issuemove leaves on an original: a
// comment by the source token's user ending in a moved-to marker. It
// returns where the issue went.
func MoveNote(c gitea.Comment, sourceUser string) (string, bool) {
	if !strings.EqualFold(c.User.Login, sourceUser) {
		return "", false
	}
	return marker.Last(c.Body, "moved-to")
}

// addUser records a user, keeping the most complete record: user objects
// inside reactions, for instance, come without an email.
func addUser(users map[string]gitea.User, u gitea.User) {
	if have, ok := users[u.Login]; ok && (have.Email != "" || u.Email == "") {
		return
	}
	users[u.Login] = u
}
