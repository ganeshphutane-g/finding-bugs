// Package move copies planned issues to their target and closes the
// originals with a link. Every copied issue and comment carries a hidden
// marker naming its source, so a run that stops halfway can be repeated:
// what was copied is found and skipped, and nothing is duplicated.
package move

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/plan"
)

// Options configure a move.
type Options struct {
	Src, Dst *gitea.Client
	Plan     *plan.Plan
	KeepOpen bool // leave the originals open
	Lock     bool // lock the originals' conversation
	Log      func(string)

	// hook, in tests, runs after each issue and comment is created and can
	// fail the run there.
	hook func(what string) error
}

// Result is one moved issue.
type Result struct {
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	URL     string `json:"url"`
	Resumed bool   `json:"resumed,omitempty"`
}

type mover struct {
	o       Options
	p       *plan.Plan
	srcKey  string // host/owner/repo, for markers
	labels  map[string]int64
	miles   map[string]int64
	moved   map[int64]int64 // source number -> target number
	results []Result
	// texts to fix once every number is known: issue or comment, with the
	// original text and who wrote it.
	fixups []fixup
}

type fixup struct {
	issue   int64 // target issue
	comment int64 // target comment (0: the issue body)
	as      string
	render  func() string
}

func (m *mover) log(f string, a ...any) {
	if m.o.Log != nil {
		m.o.Log(fmt.Sprintf(f, a...))
	}
}

// Run moves the planned issues and returns where each went.
func Run(ctx context.Context, o Options) ([]Result, error) {
	p := o.Plan
	if !p.OK() {
		return nil, fmt.Errorf("the plan has problems to solve first: %s", strings.Join(p.Blockers, "; "))
	}
	u, err := url.Parse(o.Src.BaseURL)
	if err != nil {
		return nil, err
	}
	m := &mover{o: o, p: p, srcKey: u.Host + "/" + p.Source.String(),
		labels: map[string]int64{}, miles: map[string]int64{}, moved: map[int64]int64{}}
	for k, v := range p.LabelMap {
		m.labels[strings.ToLower(k)] = v
	}
	for k, v := range p.MilestoneMap {
		m.miles[k] = v
	}
	if err := m.prepareTarget(ctx); err != nil {
		return nil, err
	}
	existing, err := m.existingCopies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range p.Issues {
		if err := m.moveIssue(ctx, &p.Issues[i], existing); err != nil {
			return m.results, fmt.Errorf("%s#%d: %w", p.Source, p.Issues[i].Issue.Number, err)
		}
	}
	// References to issues moved later in the batch now have their numbers.
	for _, f := range m.fixups {
		c := m.as(f.as)
		text := f.render()
		var err error
		if f.comment == 0 {
			_, err = c.EditIssue(ctx, p.Target.Owner, p.Target.Name, f.issue, map[string]any{"body": text})
		} else {
			err = c.EditComment(ctx, p.Target.Owner, p.Target.Name, f.comment, text)
		}
		if err != nil {
			return m.results, fmt.Errorf("updating references in %s#%d: %w", p.Target, f.issue, err)
		}
	}
	for i := range p.Issues {
		if err := m.closeOriginal(ctx, &p.Issues[i]); err != nil {
			return m.results, fmt.Errorf("%s#%d: %w", p.Source, p.Issues[i].Issue.Number, err)
		}
	}
	return m.results, nil
}

// as is the target client acting as a source user's account there, or as
// the token's own user.
func (m *mover) as(target string) *gitea.Client {
	if target == "" {
		return m.o.Dst
	}
	return m.o.Dst.As(target)
}

func (m *mover) prepareTarget(ctx context.Context) error {
	t := m.p.Target
	for _, l := range m.p.NewLabels {
		created, err := m.o.Dst.CreateLabel(ctx, t.Owner, t.Name, l)
		if err != nil {
			return fmt.Errorf("creating label %q: %w", l.Name, err)
		}
		m.labels[strings.ToLower(l.Name)] = created.ID
		m.log("created label %s", l.Name)
	}
	for _, ms := range m.p.NewMilestones {
		created, err := m.o.Dst.CreateMilestone(ctx, t.Owner, t.Name, ms)
		if err != nil {
			return fmt.Errorf("creating milestone %q: %w", ms.Title, err)
		}
		m.miles[ms.Title] = created.ID
		m.log("created milestone %s", ms.Title)
	}
	return nil
}

// existingCopies finds target issues an earlier run created (by marker).
func (m *mover) existingCopies(ctx context.Context) (map[int64]gitea.Issue, error) {
	all, err := m.o.Dst.Issues(ctx, m.p.Target.Owner, m.p.Target.Name, "all", "")
	if err != nil {
		return nil, err
	}
	found := map[int64]gitea.Issue{}
	prefix := "<!-- issuemove:issue=" + m.srcKey + "#"
	for _, is := range all {
		i := strings.Index(is.Body, prefix)
		if i < 0 {
			continue
		}
		rest := is.Body[i+len(prefix):]
		end := strings.Index(rest, " -->")
		if end < 0 {
			continue
		}
		n, err := strconv.ParseInt(rest[:end], 10, 64)
		if err == nil {
			found[n] = is
		}
	}
	return found, nil
}

func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// header names the original author and date at the top of moved text.
func (m *mover) header(kind string, author gitea.User, at time.Time, link string, postedAs string) string {
	if postedAs != "" {
		if kind == "issue" {
			return fmt.Sprintf("> Moved from [%s](%s), opened %s.\n\n", m.sourceName(), link, when(at))
		}
		return fmt.Sprintf("> Written %s.\n\n", when(at))
	}
	who := author.Login
	if author.ID <= 0 || who == "" {
		who = "a deleted user"
	} else {
		who = "**@" + who + "**"
	}
	verb := "Opened"
	if kind == "comment" {
		verb = "Written"
	}
	return fmt.Sprintf("> %s by %s on %s in [%s](%s).\n\n", verb, who, when(at), m.sourceName(), link)
}

func (m *mover) sourceName() string { return m.p.Source.String() }

// sourceRef is how a reference to another source issue is written in the
// target: owner/repo#n on the same instance, a link otherwise.
func (m *mover) sourceRef(n int64) string {
	if m.p.SameInstance {
		return fmt.Sprintf("%s#%d", m.p.Source, n)
	}
	return fmt.Sprintf("[%s#%d](%s/%s/issues/%d)", m.p.Source, n, m.o.Src.BaseURL, m.p.Source, n)
}

// refsToLater reports whether text refers to a batch issue not yet moved.
func (m *mover) refsToLater(text string) bool {
	for _, ip := range m.p.Issues {
		n := ip.Issue.Number
		if _, done := m.moved[n]; !done && strings.Contains(text, "#"+strconv.FormatInt(n, 10)) {
			return true
		}
	}
	return false
}

func (m *mover) moveIssue(ctx context.Context, ip *plan.Issue, existing map[int64]gitea.Issue) error {
	is := ip.Issue
	t := m.p.Target
	author := m.p.Authors[is.User.Login]
	c := m.as(author)

	var labelIDs []int64
	for _, l := range is.Labels {
		if id, ok := m.labels[strings.ToLower(l.Name)]; ok {
			labelIDs = append(labelIDs, id)
		}
	}
	var mile int64
	if is.Milestone != nil {
		mile = m.miles[is.Milestone.Title]
	}

	// The attachments are uploaded after the issue exists; their links in
	// the text are then pointed at the copies.
	uuids := map[string]string{}
	render := func(text string) string {
		return replaceUUIDs(rewriteRefs(text, m.moved, m.sourceRef), uuids)
	}
	issueText := func() string {
		body := m.header("issue", is.User, is.Created, is.HTMLURL, author) + render(is.Body)
		if len(ip.Dropped) > 0 {
			body += "\n\n> Assigned to " + strings.Join(ip.Dropped, ", ") + " in the original."
		}
		return body + "\n\n" + marker("issue", m.srcKey+"#"+strconv.FormatInt(is.Number, 10))
	}

	target, resumed := existing[is.Number]
	if !resumed {
		created, err := c.CreateIssue(ctx, t.Owner, t.Name, gitea.NewIssue{
			Title: is.Title, Body: issueText(), Labels: labelIDs, Milestone: mile,
			Assignees: ip.Assignees, Due: is.Due,
		})
		if err != nil {
			return fmt.Errorf("creating the issue: %w", err)
		}
		target = *created
		if err := m.copyAttachments(ctx, c, is.Assets, uuids, func(name string, data []byte) (*gitea.Attachment, error) {
			return c.AttachToIssue(ctx, t.Owner, t.Name, target.Number, name, bytes.NewReader(data))
		}); err != nil {
			return err
		}
		if len(uuids) > 0 {
			if _, err := c.EditIssue(ctx, t.Owner, t.Name, target.Number, map[string]any{"body": issueText()}); err != nil {
				return fmt.Errorf("linking attachments: %w", err)
			}
		}
		m.log("%s#%d -> %s#%d %s", m.p.Source, is.Number, t, target.Number, is.Title)
		if m.o.hook != nil {
			if err := m.o.hook(fmt.Sprintf("issue %d", is.Number)); err != nil {
				return err
			}
		}
	} else {
		m.log("%s#%d was already copied to %s#%d; finishing it", m.p.Source, is.Number, t, target.Number)
		// An earlier run may have stopped before the attachments.
		if missing := missingAssets(is.Assets, target.Assets); len(missing) > 0 {
			for _, a := range target.Assets {
				for _, sa := range is.Assets {
					if a.Name == sa.Name {
						uuids[sa.UUID] = a.UUID
					}
				}
			}
			if err := m.copyAttachments(ctx, c, missing, uuids, func(name string, data []byte) (*gitea.Attachment, error) {
				return c.AttachToIssue(ctx, t.Owner, t.Name, target.Number, name, bytes.NewReader(data))
			}); err != nil {
				return err
			}
			if _, err := c.EditIssue(ctx, t.Owner, t.Name, target.Number, map[string]any{"body": issueText()}); err != nil {
				return fmt.Errorf("linking attachments: %w", err)
			}
		}
	}
	m.moved[is.Number] = target.Number
	if m.refsToLater(is.Body) {
		m.fixups = append(m.fixups, fixup{issue: target.Number, as: author, render: issueText})
	}

	// Reactions on the issue (only with an admin token: as their users).
	if !resumed {
		m.react(ctx, ip.Reactions, func(rc *gitea.Client, content string) error {
			return rc.React(ctx, t.Owner, t.Name, target.Number, 0, content)
		})
	}

	// Comments, skipping those an earlier run copied.
	done := map[int64]gitea.Comment{}
	if resumed {
		copied, err := m.o.Dst.Comments(ctx, t.Owner, t.Name, target.Number)
		if err != nil {
			return err
		}
		for _, cm := range copied {
			for _, sc := range ip.Comments {
				if hasMarker(cm.Body, "comment", strconv.FormatInt(sc.ID, 10)) {
					done[sc.ID] = cm
				}
			}
		}
	}
	for _, sc := range ip.Comments {
		sc := sc
		if strings.Contains(sc.Body, "<!-- issuemove:moved-to=") {
			continue // issuemove's own note on an original
		}
		cauthor := m.p.Authors[sc.User.Login]
		cc := m.as(cauthor)
		cuuids := map[string]string{}
		commentText := func() string {
			return m.header("comment", sc.User, sc.Created, sc.HTMLURL, cauthor) +
				replaceUUIDs(rewriteRefs(sc.Body, m.moved, m.sourceRef), cuuids) +
				"\n\n" + marker("comment", strconv.FormatInt(sc.ID, 10))
		}
		if prev, ok := done[sc.ID]; ok {
			// Copied before; finish its attachments if the run stopped there.
			missing := missingAssets(sc.Assets, prev.Assets)
			if len(missing) == 0 {
				continue
			}
			for _, a := range prev.Assets {
				for _, sa := range sc.Assets {
					if a.Name == sa.Name {
						cuuids[sa.UUID] = a.UUID
					}
				}
			}
			if err := m.copyAttachments(ctx, cc, missing, cuuids, func(name string, data []byte) (*gitea.Attachment, error) {
				return cc.AttachToComment(ctx, t.Owner, t.Name, prev.ID, name, bytes.NewReader(data))
			}); err != nil {
				return err
			}
			if err := cc.EditComment(ctx, t.Owner, t.Name, prev.ID, commentText()); err != nil {
				return fmt.Errorf("linking attachments: %w", err)
			}
			continue
		}
		created, err := cc.CreateComment(ctx, t.Owner, t.Name, target.Number, commentText())
		if err != nil {
			return fmt.Errorf("copying comment %d: %w", sc.ID, err)
		}
		if err := m.copyAttachments(ctx, cc, sc.Assets, cuuids, func(name string, data []byte) (*gitea.Attachment, error) {
			return cc.AttachToComment(ctx, t.Owner, t.Name, created.ID, name, bytes.NewReader(data))
		}); err != nil {
			return err
		}
		if len(cuuids) > 0 {
			if err := cc.EditComment(ctx, t.Owner, t.Name, created.ID, commentText()); err != nil {
				return fmt.Errorf("linking attachments: %w", err)
			}
		}
		if m.refsToLater(sc.Body) {
			m.fixups = append(m.fixups, fixup{issue: target.Number, comment: created.ID, as: cauthor, render: commentText})
		}
		if m.o.hook != nil {
			if err := m.o.hook(fmt.Sprintf("comment %d", sc.ID)); err != nil {
				return err
			}
		}
		m.react(ctx, ip.CommentReac[sc.ID], func(rc *gitea.Client, content string) error {
			return rc.React(ctx, t.Owner, t.Name, target.Number, created.ID, content)
		})
	}

	if is.State == "closed" && target.State != "closed" {
		if _, err := m.o.Dst.EditIssue(ctx, t.Owner, t.Name, target.Number, map[string]any{"state": "closed"}); err != nil {
			return fmt.Errorf("closing the copy: %w", err)
		}
	}
	m.results = append(m.results, Result{From: is.Number, To: target.Number, URL: target.HTMLURL, Resumed: resumed})
	return nil
}

// copyAttachments downloads each attachment and uploads it with upload,
// recording old -> new UUIDs.
func (m *mover) copyAttachments(ctx context.Context, c *gitea.Client, assets []gitea.Attachment, uuids map[string]string,
	upload func(name string, data []byte) (*gitea.Attachment, error)) error {
	for _, a := range assets {
		data, err := m.o.Src.DownloadAttachment(ctx, a.UUID)
		if err != nil {
			return err
		}
		if int64(len(data)) != a.Size {
			return fmt.Errorf("attachment %s: got %d bytes, expected %d", a.Name, len(data), a.Size)
		}
		nw, err := upload(a.Name, data)
		if err != nil {
			return fmt.Errorf("uploading attachment %s: %w", a.Name, err)
		}
		uuids[a.UUID] = nw.UUID
	}
	return nil
}

// react adds reactions as their users, when they can be posted as them.
// Reactions cannot be attributed in text, so the others are left out (the
// plan says so).
func (m *mover) react(ctx context.Context, rs []gitea.Reaction, add func(*gitea.Client, string) error) {
	for _, r := range rs {
		who := m.p.Authors[r.User.Login]
		if who == "" {
			continue
		}
		if err := add(m.o.Dst.As(who), r.Content); err != nil {
			m.log("  could not add %s's %s reaction: %v", r.User.Login, r.Content, err)
		}
	}
}

// closeOriginal leaves a link on the original and closes (and locks) it.
func (m *mover) closeOriginal(ctx context.Context, ip *plan.Issue) error {
	is := ip.Issue
	s, t := m.p.Source, m.p.Target
	to := m.moved[is.Number]
	key := m.p.Target.String() + "#" + strconv.FormatInt(to, 10)
	comments, err := m.o.Src.Comments(ctx, s.Owner, s.Name, is.Number)
	if err != nil {
		return err
	}
	noted := false
	for _, c := range comments {
		noted = noted || hasMarker(c.Body, "moved-to", key)
	}
	if !noted {
		link := fmt.Sprintf("%s#%d", t, to)
		if !m.p.SameInstance {
			link = fmt.Sprintf("[%s#%d](%s/%s/issues/%d)", t, to, m.o.Dst.BaseURL, t, to)
		}
		if _, err := m.o.Src.CreateComment(ctx, s.Owner, s.Name, is.Number,
			"Moved to "+link+".\n\n"+marker("moved-to", key)); err != nil {
			return fmt.Errorf("leaving the link: %w", err)
		}
	}
	if !m.o.KeepOpen && is.State != "closed" {
		if _, err := m.o.Src.EditIssue(ctx, s.Owner, s.Name, is.Number, map[string]any{"state": "closed"}); err != nil {
			return fmt.Errorf("closing the original: %w", err)
		}
	}
	if m.o.Lock && !is.IsLocked {
		if err := m.o.Src.Lock(ctx, s.Owner, s.Name, is.Number); err != nil {
			return fmt.Errorf("locking the original: %w", err)
		}
	}
	return nil
}

// missingAssets lists source attachments the copy does not have yet
// (matched by name, as many as there are of each name).
func missingAssets(src, have []gitea.Attachment) []gitea.Attachment {
	count := map[string]int{}
	for _, a := range have {
		count[a.Name]++
	}
	var missing []gitea.Attachment
	for _, a := range src {
		if count[a.Name] > 0 {
			count[a.Name]--
			continue
		}
		missing = append(missing, a)
	}
	return missing
}
