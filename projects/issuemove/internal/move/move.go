// Package move copies planned issues to their target and closes the
// originals with a link. Every copied issue and comment ends with a hidden
// marker naming its source, so a run that stops halfway can be repeated:
// what was copied is found and finished, and nothing is duplicated.
//
// Markers are trusted only in text posted by the token's user or by the
// original author, and marker-like notes in copied text are defused, so
// nobody can make issuemove take their issue for a copy.
package move

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/marker"
	"github.com/ganeshphutane-g/issuemove/internal/plan"
)

// Options configure a move. What to do with the originals (keep open,
// lock) is part of the plan.
type Options struct {
	Src, Dst *gitea.Client
	Plan     *plan.Plan
	Log      func(string)

	// hook, in tests, runs right after each issue and comment is created
	// and can fail the run there.
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
	labels  map[string]int64
	miles   map[string]int64
	copies  map[int64]copied // source number -> copy found from an earlier run
	moved   map[int64]int64  // source number -> target number
	results []Result
	// texts to fix once every number is known
	fixups []fixup
}

// copied is a copy an earlier run made, and the state its original had.
type copied struct {
	gitea.Issue
	state string
}

type fixup struct {
	issue   int64 // target issue
	comment int64 // target comment (0: the issue's title and body)
	editor  *gitea.Client
	title   func() string
	render  func() string
	written string
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
	m := &mover{o: o, p: p, labels: map[string]int64{}, miles: map[string]int64{}, moved: map[int64]int64{}}
	for k, v := range p.LabelMap {
		m.labels[strings.ToLower(k)] = v
	}
	for k, v := range p.MilestoneMap {
		m.miles[k] = v
	}
	if err := m.prepareTarget(ctx); err != nil {
		return nil, err
	}
	var err error
	if m.copies, err = m.existingCopies(ctx); err != nil {
		return nil, err
	}
	for n, c := range m.copies {
		m.moved[n] = c.Number
	}
	for i := range p.Issues {
		if err := m.moveIssue(ctx, &p.Issues[i]); err != nil {
			return m.results, fmt.Errorf("%s#%d: %w", p.Source, p.Issues[i].Issue.Number, err)
		}
	}
	// References to issues moved later in the batch now have their numbers.
	for _, f := range m.fixups {
		text := f.render()
		if text == f.written {
			continue
		}
		var err error
		if f.comment == 0 {
			_, err = f.editor.EditIssue(ctx, p.Target.Owner, p.Target.Name, f.issue, map[string]any{"title": f.title(), "body": text})
		} else {
			err = f.editor.EditComment(ctx, p.Target.Owner, p.Target.Name, f.comment, text)
		}
		if err != nil {
			return m.results, fmt.Errorf("updating references in %s#%d: %w", p.Target, f.issue, err)
		}
	}
	for i := range p.Issues {
		m.links(ctx, &p.Issues[i])
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
	if target == "" || strings.EqualFold(target, m.p.TargetUser) {
		return m.o.Dst
	}
	return m.o.Dst.As(target)
}

// trusted reports whether text posted by poster may carry a marker for
// something written by author (a source login).
func (m *mover) trusted(poster, author string) bool {
	if strings.EqualFold(poster, m.p.TargetUser) {
		return true
	}
	as := m.p.Authors[author]
	return as != "" && strings.EqualFold(poster, as)
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

// existingCopies finds target issues an earlier run created: issues whose
// body ends in a marker for a planned issue, posted by the token's user or
// that issue's author.
func (m *mover) existingCopies(ctx context.Context) (map[int64]copied, error) {
	authors := map[int64]string{}
	for _, ip := range m.p.Issues {
		authors[ip.Issue.Number] = ip.Issue.User.Login
	}
	all, err := m.o.Dst.Issues(ctx, m.p.Target.Owner, m.p.Target.Name, "all", "")
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Number < all[j].Number })
	found := map[int64]copied{}
	for _, is := range all {
		v, ok := marker.Last(is.Body, "issue")
		if !ok {
			continue
		}
		key, state, _ := strings.Cut(v, " state=")
		rest, ok := strings.CutPrefix(key, m.p.SourceKey+"#")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			continue
		}
		author, planned := authors[n]
		if _, seen := found[n]; seen || !planned || !m.trusted(is.User.Login, author) {
			continue
		}
		found[n] = copied{Issue: is, state: state}
	}
	return found, nil
}

func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// header names the original author and date at the top of moved text.
func (m *mover) header(kind string, author gitea.User, at time.Time, link string, postedAs string) string {
	if postedAs != "" {
		if kind == "issue" {
			return fmt.Sprintf("> Moved from [%s](%s), opened %s.\n\n", m.p.Source.String()+"#"+issueNumber(link), link, when(at))
		}
		return fmt.Sprintf("> Written %s.\n\n", when(at))
	}
	who := author.Login
	switch {
	case author.ID <= 0 || who == "":
		who = "a deleted user"
	case m.p.SameInstance:
		who = "**@" + who + "**"
	default:
		// The same name on this instance may be someone else: no mention.
		who = "**@​" + who + "**"
	}
	verb := "Opened"
	if kind == "comment" {
		verb = "Written"
	}
	name := m.p.Source.String()
	if kind == "issue" {
		name += "#" + issueNumber(link)
	}
	return fmt.Sprintf("> %s by %s on %s in [%s](%s).\n\n", verb, who, when(at), name, link)
}

// sourceRef is how a reference to another source issue is written in the
// target: owner/repo#n on the same instance, a link otherwise.
func (m *mover) sourceRef(n int64) string {
	if m.p.SameInstance {
		return fmt.Sprintf("%s#%d", m.p.Source, n)
	}
	return fmt.Sprintf("[%s#%d](%s/%s/issues/%d)", m.p.Source, n, m.o.Src.BaseURL, m.p.Source, n)
}

// render turns someone's text into its moved form: marker-like notes
// defused, references pointed at the right place, mentions of people who
// may not be the same on another instance quietened, attachment links
// pointed at the copies.
func (m *mover) render(text string, uuids map[string]string) string {
	text = outsideCode(marker.Defuse(text), func(s string) string {
		s = rewriteRefs(s, m.moved, m.sourceRef)
		if !m.p.SameInstance {
			s = quietMentions(s, func(login string) bool { return m.p.Authors[login] != "" })
		}
		return s
	})
	return replaceUUIDs(text, uuids)
}

// refsToLater reports whether text may refer to a batch issue not yet moved.
func (m *mover) refsToLater(text string) bool {
	for _, ip := range m.p.Issues {
		n := ip.Issue.Number
		if _, done := m.moved[n]; !done && strings.Contains(text, "#"+strconv.FormatInt(n, 10)) {
			return true
		}
	}
	return false
}

func (m *mover) moveIssue(ctx context.Context, ip *plan.Issue) error {
	is := ip.Issue
	t := m.p.Target
	author := m.p.Authors[is.User.Login]
	prev, resumed := m.copies[is.Number]

	// A copy whose original already has the link is finished: people may
	// have worked on it since, so it is left as it is.
	if resumed && strings.EqualFold(ip.MovedTo, fmt.Sprintf("%s#%d", t, prev.Number)) {
		m.log("%s#%d was moved to %s#%d before", m.p.Source, is.Number, t, prev.Number)
		m.results = append(m.results, Result{From: is.Number, To: prev.Number, URL: prev.HTMLURL, Resumed: true})
		return nil
	}

	state := is.State
	if resumed && prev.state != "" {
		state = prev.state // the original may have been closed since
	}
	uuids := map[string]string{}
	title := func() string {
		return outsideCode(is.Title, func(s string) string { return rewriteRefs(s, m.moved, m.sourceRef) })
	}
	issueText := func() string {
		body := m.header("issue", is.User, is.Created, is.HTMLURL, author) + m.render(is.Body, uuids)
		if len(ip.Dropped) > 0 {
			body += "\n\n> Assigned to " + strings.Join(ip.Dropped, ", ") + " in the original."
		}
		return body + "\n\n" + marker.Make("issue", fmt.Sprintf("%s#%d state=%s", m.p.SourceKey, is.Number, state))
	}
	labelIDs, mile := m.metadata(is)

	var target gitea.Issue
	var editor *gitea.Client
	if !resumed {
		// Labels, milestone and assignees are only kept here when the
		// author can write to the target; setMetadata makes sure of them.
		editor = m.as(author)
		created, err := editor.CreateIssue(ctx, t.Owner, t.Name, gitea.NewIssue{
			Title: title(), Body: issueText(), Labels: labelIDs, Milestone: mile,
			Assignees: ip.Assignees, Due: is.Due,
		})
		if err != nil {
			return fmt.Errorf("creating the issue: %w", err)
		}
		target = *created
		m.log("%s#%d -> %s#%d %s", m.p.Source, is.Number, t, target.Number, is.Title)
		if m.o.hook != nil {
			if err := m.o.hook(fmt.Sprintf("issue %d", is.Number)); err != nil {
				return err
			}
		}
	} else {
		target = prev.Issue
		editor = m.as(target.User.Login)
		m.log("%s#%d was partly copied to %s#%d; finishing it", m.p.Source, is.Number, t, target.Number)
	}
	m.moved[is.Number] = target.Number

	if err := m.attach(ctx, is.Assets, target.Assets, uuids, func(name string, data []byte) (*gitea.Attachment, error) {
		return m.o.Dst.AttachToIssue(ctx, t.Owner, t.Name, target.Number, name, bytes.NewReader(data))
	}); err != nil {
		return err
	}
	if err := m.setMetadata(ctx, target, ip, labelIDs, mile); err != nil {
		return err
	}
	written := target.Body
	if text := issueText(); text != target.Body || title() != target.Title {
		if _, err := editor.EditIssue(ctx, t.Owner, t.Name, target.Number, map[string]any{"title": title(), "body": text}); err != nil {
			return fmt.Errorf("updating the copy: %w", err)
		}
		written = text
	}
	if m.refsToLater(is.Title + "\n" + is.Body) {
		m.fixups = append(m.fixups, fixup{issue: target.Number, editor: editor, title: title, render: issueText, written: written})
	}

	var have []gitea.Reaction
	if resumed {
		var err error
		if have, err = m.o.Dst.IssueReactions(ctx, t.Owner, t.Name, target.Number); err != nil {
			return err
		}
	}
	m.react(ip.Reactions, have, func(rc *gitea.Client, content string) error {
		return rc.React(ctx, t.Owner, t.Name, target.Number, 0, content)
	})

	if err := m.moveComments(ctx, ip, target.Number, resumed); err != nil {
		return err
	}

	if state == "closed" && target.State != "closed" {
		if _, err := m.o.Dst.EditIssue(ctx, t.Owner, t.Name, target.Number, map[string]any{"state": "closed"}); err != nil {
			return fmt.Errorf("closing the copy: %w", err)
		}
	}
	m.results = append(m.results, Result{From: is.Number, To: target.Number, URL: target.HTMLURL, Resumed: resumed})
	return nil
}

// metadata is the target's label IDs and milestone for an issue.
func (m *mover) metadata(is gitea.Issue) ([]int64, int64) {
	var ids []int64
	for _, l := range is.Labels {
		if id, ok := m.labels[strings.ToLower(l.Name)]; ok {
			ids = append(ids, id)
		}
	}
	var mile int64
	if is.Milestone != nil {
		mile = m.miles[is.Milestone.Title]
	}
	return ids, mile
}

// setMetadata gives the copy its labels, milestone, assignees and due date
// as the token's user. Gitea drops them silently when the issue is created
// by someone who cannot write to the repository.
func (m *mover) setMetadata(ctx context.Context, target gitea.Issue, ip *plan.Issue, labelIDs []int64, mile int64) error {
	t := m.p.Target
	have := map[int64]bool{}
	for _, l := range target.Labels {
		have[l.ID] = true
	}
	same := len(have) == len(labelIDs)
	for _, id := range labelIDs {
		same = same && have[id]
	}
	if !same && len(labelIDs) > 0 {
		if err := m.o.Dst.SetLabels(ctx, t.Owner, t.Name, target.Number, labelIDs); err != nil {
			return fmt.Errorf("setting labels: %w", err)
		}
	}
	fields := map[string]any{}
	if mile != 0 && (target.Milestone == nil || target.Milestone.ID != mile) {
		fields["milestone"] = mile
	}
	assigned := map[string]bool{}
	for _, a := range target.Assignees {
		assigned[strings.ToLower(a.Login)] = true
	}
	for _, a := range ip.Assignees {
		if !assigned[strings.ToLower(a)] {
			fields["assignees"] = ip.Assignees
		}
	}
	if ip.Issue.Due != nil && target.Due == nil {
		fields["due_date"] = ip.Issue.Due
	}
	if len(fields) > 0 {
		if _, err := m.o.Dst.EditIssue(ctx, t.Owner, t.Name, target.Number, fields); err != nil {
			return fmt.Errorf("setting the milestone, assignees or due date: %w", err)
		}
	}
	return nil
}

func (m *mover) moveComments(ctx context.Context, ip *plan.Issue, to int64, resumed bool) error {
	t := m.p.Target
	byID := map[int64]gitea.Comment{}
	for _, sc := range ip.Comments {
		byID[sc.ID] = sc
	}
	// Comments an earlier run copied: they end in a marker for this
	// source issue and were posted by the token's user or the author.
	done := map[int64]gitea.Comment{}
	if resumed {
		copies, err := m.o.Dst.Comments(ctx, t.Owner, t.Name, to)
		if err != nil {
			return err
		}
		prefix := fmt.Sprintf("%s#%d/", m.p.SourceKey, ip.Issue.Number)
		for _, cm := range copies {
			v, ok := marker.Last(cm.Body, "comment")
			if !ok || !strings.HasPrefix(v, prefix) {
				continue
			}
			id, err := strconv.ParseInt(strings.TrimPrefix(v, prefix), 10, 64)
			sc, planned := byID[id]
			if _, seen := done[id]; err != nil || seen || !planned || !m.trusted(cm.User.Login, sc.User.Login) {
				continue
			}
			done[id] = cm
		}
	}
	for _, sc := range ip.Comments {
		sc := sc
		if _, note := plan.MoveNote(sc, m.p.SourceUser); note {
			continue // issuemove's own note on an original
		}
		cauthor := m.p.Authors[sc.User.Login]
		cuuids := map[string]string{}
		commentText := func() string {
			return m.header("comment", sc.User, sc.Created, sc.HTMLURL, cauthor) + m.render(sc.Body, cuuids) +
				"\n\n" + marker.Make("comment", fmt.Sprintf("%s#%d/%d", m.p.SourceKey, ip.Issue.Number, sc.ID))
		}
		prev, again := done[sc.ID]
		editor := m.as(cauthor)
		if again {
			editor = m.as(prev.User.Login)
		} else {
			created, err := editor.CreateComment(ctx, t.Owner, t.Name, to, commentText())
			if err != nil {
				return fmt.Errorf("copying comment %d: %w", sc.ID, err)
			}
			prev = *created
			if m.o.hook != nil {
				if err := m.o.hook(fmt.Sprintf("comment %d", sc.ID)); err != nil {
					return err
				}
			}
		}
		if err := m.attach(ctx, sc.Assets, prev.Assets, cuuids, func(name string, data []byte) (*gitea.Attachment, error) {
			return m.o.Dst.AttachToComment(ctx, t.Owner, t.Name, prev.ID, name, bytes.NewReader(data))
		}); err != nil {
			return err
		}
		written := prev.Body
		if text := commentText(); text != prev.Body {
			if err := editor.EditComment(ctx, t.Owner, t.Name, prev.ID, text); err != nil {
				return fmt.Errorf("updating comment %d: %w", prev.ID, err)
			}
			written = text
		}
		if m.refsToLater(sc.Body) {
			m.fixups = append(m.fixups, fixup{issue: to, comment: prev.ID, editor: editor, render: commentText, written: written})
		}
		var have []gitea.Reaction
		if again {
			var err error
			if have, err = m.o.Dst.CommentReactions(ctx, t.Owner, t.Name, prev.ID); err != nil {
				return err
			}
		}
		m.react(ip.CommentReac[sc.ID], have, func(rc *gitea.Client, content string) error {
			return rc.React(ctx, t.Owner, t.Name, to, prev.ID, content)
		})
	}
	return nil
}

// attach makes the copy's attachments match the source's: those already
// there (matched by name) are reused, the rest downloaded and uploaded.
// uuids records old -> new UUIDs for the links in the text.
func (m *mover) attach(ctx context.Context, src, have []gitea.Attachment, uuids map[string]string,
	upload func(name string, data []byte) (*gitea.Attachment, error)) error {
	byName := map[string][]gitea.Attachment{}
	for _, a := range have {
		byName[a.Name] = append(byName[a.Name], a)
	}
	for _, a := range src {
		if there := byName[a.Name]; len(there) > 0 {
			uuids[a.UUID] = there[0].UUID
			byName[a.Name] = there[1:]
			continue
		}
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

// react adds the reactions the copy does not have yet, as their users.
// Reactions cannot be attributed in text, so those whose users cannot be
// posted as are left out (the plan says how many).
func (m *mover) react(want, have []gitea.Reaction, add func(*gitea.Client, string) error) {
	there := map[string]bool{}
	for _, r := range have {
		there[strings.ToLower(r.User.Login)+" "+r.Content] = true
	}
	for _, r := range want {
		who := m.p.Authors[r.User.Login]
		if who == "" || there[strings.ToLower(who)+" "+r.Content] {
			continue
		}
		if err := add(m.as(who), r.Content); err != nil {
			m.log("  could not add %s's %s reaction: %v", r.User.Login, r.Content, err)
		}
	}
}

// closeOriginal leaves a link on the original and closes (and locks) it.
func (m *mover) closeOriginal(ctx context.Context, ip *plan.Issue) error {
	is := ip.Issue
	s, t := m.p.Source, m.p.Target
	to := m.moved[is.Number]
	key := fmt.Sprintf("%s#%d", t, to)
	comments, err := m.o.Src.Comments(ctx, s.Owner, s.Name, is.Number)
	if err != nil {
		return err
	}
	noted := false
	for _, c := range comments {
		v, ok := plan.MoveNote(c, m.p.SourceUser)
		noted = noted || (ok && v == key)
	}
	if !noted {
		link := key
		if !m.p.SameInstance {
			link = fmt.Sprintf("[%s](%s/%s/issues/%d)", key, m.o.Dst.BaseURL, t, to)
		}
		if _, err := m.o.Src.CreateComment(ctx, s.Owner, s.Name, is.Number,
			"Moved to "+link+".\n\n"+marker.Make("moved-to", key)); err != nil {
			return fmt.Errorf("leaving the link: %w", err)
		}
	}
	if !m.p.KeepOpen && is.State != "closed" {
		if _, err := m.o.Src.EditIssue(ctx, s.Owner, s.Name, is.Number, map[string]any{"state": "closed"}); err != nil {
			return fmt.Errorf("closing the original: %w", err)
		}
	}
	if m.p.Lock && !is.IsLocked {
		if err := m.o.Src.Lock(ctx, s.Owner, s.Name, is.Number); err != nil {
			return fmt.Errorf("locking the original: %w", err)
		}
	}
	return nil
}

// links carries dependencies once every issue has its new number: what the
// copy depends on, and (on the same instance) issues left behind that
// depended on the original now depend on the copy too. Failures are logged,
// not fatal: the text keeps the history. (Watchers are not copied: Gitea
// subscribes everyone who writes on an issue, and with an admin token the
// participants write as themselves.)
func (m *mover) links(ctx context.Context, ip *plan.Issue) {
	t := m.p.Target
	to := m.moved[ip.Issue.Number]
	key := func(owner, repo string, n int64) string {
		return strings.ToLower(owner+"/"+repo) + "#" + strconv.FormatInt(n, 10)
	}
	have := map[string]bool{}
	if deps, err := m.o.Dst.Dependencies(ctx, t.Owner, t.Name, to); err == nil {
		for _, d := range deps {
			owner, repo := m.repoOf(d)
			have[key(owner, repo, d.Number)] = true
		}
	}
	for _, d := range ip.DependsOn {
		owner, repo := m.repoOf(d)
		n := d.Number
		if strings.EqualFold(owner+"/"+repo, m.p.Source.String()) {
			if nw, ok := m.moved[n]; ok {
				owner, repo, n = t.Owner, t.Name, nw
			} else if !m.p.SameInstance {
				m.log("  %s#%d depended on %s/%s#%d on the other instance; not carried", t, to, owner, repo, n)
				continue
			}
		}
		if have[key(owner, repo, n)] {
			continue
		}
		if err := m.o.Dst.AddDependency(ctx, t.Owner, t.Name, to, owner, repo, n); err != nil {
			m.log("  could not make %s#%d depend on %s/%s#%d: %v", t, to, owner, repo, n, err)
		}
	}
	for _, b := range ip.Blocks {
		owner, repo := m.repoOf(b)
		if _, together := m.moved[b.Number]; together && strings.EqualFold(owner+"/"+repo, m.p.Source.String()) {
			continue // its own dependencies are carried above
		}
		if !m.p.SameInstance {
			m.log("  %s/%s#%d depended on %s#%d, which moved to another instance; not carried", owner, repo, b.Number, m.p.Source, ip.Issue.Number)
			continue
		}
		deps, err := m.o.Src.Dependencies(ctx, owner, repo, b.Number)
		already := false
		for _, d := range deps {
			o, r := m.repoOf(d)
			already = already || key(o, r, d.Number) == key(t.Owner, t.Name, to)
		}
		if err == nil && already {
			continue
		}
		if err := m.o.Src.AddDependency(ctx, owner, repo, b.Number, t.Owner, t.Name, to); err != nil {
			m.log("  could not make %s/%s#%d depend on %s#%d: %v", owner, repo, b.Number, t, to, err)
		}
	}
}

// repoOf is the owner and name of a dependency's repository (the source
// when the server leaves it out).
func (m *mover) repoOf(d gitea.Dependency) (string, string) {
	if owner, repo, ok := strings.Cut(d.Repository.FullName, "/"); ok {
		return owner, repo
	}
	if d.Repository.Owner != "" && d.Repository.Name != "" {
		return d.Repository.Owner, d.Repository.Name
	}
	return m.p.Source.Owner, m.p.Source.Name
}

// issueNumber is the number at the end of an issue's web address.
func issueNumber(link string) string {
	i := strings.LastIndex(link, "/")
	if i < 0 {
		return ""
	}
	n := link[i+1:]
	if j := strings.IndexAny(n, "#?"); j >= 0 {
		n = n[:j]
	}
	return n
}
