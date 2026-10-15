package gitea

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// User is an account.
type User struct {
	ID      int64  `json:"id"`
	Login   string `json:"login"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
}

// Repo is a repository.
type Repo struct {
	ID          int64  `json:"id"`
	FullName    string `json:"full_name"`
	Owner       User   `json:"owner"`
	Name        string `json:"name"`
	HasIssues   bool   `json:"has_issues"`
	Archived    bool   `json:"archived"`
	Permissions struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
}

// Label is an issue label.
type Label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Exclusive   bool   `json:"exclusive"`
	IsArchived  bool   `json:"is_archived"`
}

// Milestone is a milestone.
type Milestone struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"`
	DueOn       *time.Time `json:"due_on"`
}

// Attachment is a file on an issue or comment.
type Attachment struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	UUID        string    `json:"uuid"`
	DownloadURL string    `json:"browser_download_url"`
	Created     time.Time `json:"created_at"`
}

// Issue is an issue (pull requests are filtered out by the caller).
type Issue struct {
	ID          int64        `json:"id"`
	Number      int64        `json:"number"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	User        User         `json:"user"`
	Labels      []Label      `json:"labels"`
	Milestone   *Milestone   `json:"milestone"`
	Assignees   []User       `json:"assignees"`
	State       string       `json:"state"`
	IsLocked    bool         `json:"is_locked"`
	Comments    int          `json:"comments"`
	Created     time.Time    `json:"created_at"`
	Updated     time.Time    `json:"updated_at"`
	Closed      *time.Time   `json:"closed_at"`
	Due         *time.Time   `json:"due_date"`
	HTMLURL     string       `json:"html_url"`
	PullRequest *struct{}    `json:"pull_request"`
	Assets      []Attachment `json:"assets"`
}

// Comment is an issue comment.
type Comment struct {
	ID      int64        `json:"id"`
	Body    string       `json:"body"`
	User    User         `json:"user"`
	Created time.Time    `json:"created_at"`
	HTMLURL string       `json:"html_url"`
	Assets  []Attachment `json:"assets"`
}

// Reaction is an emoji reaction.
type Reaction struct {
	User    User   `json:"user"`
	Content string `json:"content"`
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// Version is the server's version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := c.get(ctx, "/version", nil, &v)
	return v.Version, err
}

// Me is the token's user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	err := c.get(ctx, "/user", nil, &u)
	return &u, err
}

// User looks up an account by name.
func (c *Client) User(ctx context.Context, login string) (*User, error) {
	var u User
	err := c.get(ctx, "/users/"+url.PathEscape(login), nil, &u)
	return &u, err
}

// Repo reads a repository.
func (c *Client) Repo(ctx context.Context, owner, repo string) (*Repo, error) {
	var r Repo
	err := c.get(ctx, repoPath(owner, repo), nil, &r)
	return &r, err
}

// Issue reads one issue.
func (c *Client) Issue(ctx context.Context, owner, repo string, n int64) (*Issue, error) {
	var i Issue
	err := c.get(ctx, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10), nil, &i)
	return &i, err
}

// Issues lists issues (not pull requests) with a state (open, closed, all)
// and optional comma-separated labels.
func (c *Client) Issues(ctx context.Context, owner, repo, state, labels string) ([]Issue, error) {
	q := url.Values{"state": {state}, "type": {"issues"}}
	if labels != "" {
		q.Set("labels", labels)
	}
	return list[Issue](ctx, c, repoPath(owner, repo)+"/issues", q)
}

// Comments lists an issue's comments.
func (c *Client) Comments(ctx context.Context, owner, repo string, n int64) ([]Comment, error) {
	return list[Comment](ctx, c, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/comments", nil)
}

// IssueReactions lists reactions on an issue.
func (c *Client) IssueReactions(ctx context.Context, owner, repo string, n int64) ([]Reaction, error) {
	return list[Reaction](ctx, c, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/reactions", nil)
}

// CommentReactions lists reactions on a comment.
func (c *Client) CommentReactions(ctx context.Context, owner, repo string, id int64) ([]Reaction, error) {
	var out []Reaction
	err := c.get(ctx, repoPath(owner, repo)+"/issues/comments/"+strconv.FormatInt(id, 10)+"/reactions", nil, &out)
	return out, err
}

// Labels lists a repository's labels.
func (c *Client) Labels(ctx context.Context, owner, repo string) ([]Label, error) {
	return list[Label](ctx, c, repoPath(owner, repo)+"/labels", nil)
}

// OrgLabels lists an organisation's labels (an error if owner is a user).
func (c *Client) OrgLabels(ctx context.Context, org string) ([]Label, error) {
	return list[Label](ctx, c, "/orgs/"+url.PathEscape(org)+"/labels", nil)
}

// Milestones lists a repository's milestones, open and closed.
func (c *Client) Milestones(ctx context.Context, owner, repo string) ([]Milestone, error) {
	return list[Milestone](ctx, c, repoPath(owner, repo)+"/milestones", url.Values{"state": {"all"}})
}

// Assignees lists users who can be assigned in a repository.
func (c *Client) Assignees(ctx context.Context, owner, repo string) ([]User, error) {
	var out []User
	err := c.get(ctx, repoPath(owner, repo)+"/assignees", nil, &out)
	return out, err
}

// CreateLabel adds a label to a repository.
func (c *Client) CreateLabel(ctx context.Context, owner, repo string, l Label) (*Label, error) {
	var out Label
	err := c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/labels", map[string]any{
		"name": l.Name, "color": l.Color, "description": l.Description, "exclusive": l.Exclusive,
	}, &out)
	return &out, err
}

// CreateMilestone adds a milestone.
func (c *Client) CreateMilestone(ctx context.Context, owner, repo string, m Milestone) (*Milestone, error) {
	in := map[string]any{"title": m.Title, "description": m.Description}
	if m.DueOn != nil {
		in["due_on"] = m.DueOn
	}
	var out Milestone
	if err := c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/milestones", in, &out); err != nil {
		return nil, err
	}
	if m.State == "closed" {
		err := c.send(ctx, http.MethodPatch, repoPath(owner, repo)+"/milestones/"+strconv.FormatInt(out.ID, 10), map[string]any{"state": "closed"}, &out)
		return &out, err
	}
	return &out, nil
}

// NewIssue is what CreateIssue sends.
type NewIssue struct {
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Labels    []int64    `json:"labels,omitempty"`
	Milestone int64      `json:"milestone,omitempty"`
	Assignees []string   `json:"assignees,omitempty"`
	Closed    bool       `json:"closed,omitempty"`
	Due       *time.Time `json:"due_date,omitempty"`
}

// CreateIssue opens an issue.
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, in NewIssue) (*Issue, error) {
	var out Issue
	err := c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/issues", in, &out)
	return &out, err
}

// EditIssue changes fields of an issue (body, state, ...).
func (c *Client) EditIssue(ctx context.Context, owner, repo string, n int64, fields map[string]any) (*Issue, error) {
	var out Issue
	err := c.send(ctx, http.MethodPatch, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10), fields, &out)
	return &out, err
}

// CreateComment adds a comment.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, n int64, body string) (*Comment, error) {
	var out Comment
	err := c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/comments", map[string]any{"body": body}, &out)
	return &out, err
}

// EditComment changes a comment's text.
func (c *Client) EditComment(ctx context.Context, owner, repo string, id int64, body string) error {
	return c.send(ctx, http.MethodPatch, repoPath(owner, repo)+"/issues/comments/"+strconv.FormatInt(id, 10), map[string]any{"body": body}, nil)
}

// React adds a reaction to an issue (comment 0) or a comment.
func (c *Client) React(ctx context.Context, owner, repo string, n, comment int64, content string) error {
	p := repoPath(owner, repo) + "/issues/" + strconv.FormatInt(n, 10) + "/reactions"
	if comment != 0 {
		p = repoPath(owner, repo) + "/issues/comments/" + strconv.FormatInt(comment, 10) + "/reactions"
	}
	return c.send(ctx, http.MethodPost, p, map[string]any{"content": content}, nil)
}

// AttachToIssue uploads a file to an issue; AttachToComment to a comment.
func (c *Client) AttachToIssue(ctx context.Context, owner, repo string, n int64, name string, content io.Reader) (*Attachment, error) {
	var out Attachment
	err := c.upload(ctx, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/assets", "attachment", name, content, &out)
	return &out, err
}

// AttachToComment uploads a file to a comment.
func (c *Client) AttachToComment(ctx context.Context, owner, repo string, id int64, name string, content io.Reader) (*Attachment, error) {
	var out Attachment
	err := c.upload(ctx, repoPath(owner, repo)+"/issues/comments/"+strconv.FormatInt(id, 10)+"/assets", "attachment", name, content, &out)
	return &out, err
}

// Lock locks an issue's conversation.
func (c *Client) Lock(ctx context.Context, owner, repo string, n int64) error {
	return c.send(ctx, http.MethodPut, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/lock", map[string]any{"lock_reason": "resolved"}, nil)
}

// String helps logging.
func (i *Issue) String() string { return fmt.Sprintf("#%d %s", i.Number, i.Title) }

// Dependency is an issue another one depends on, as listed by the API.
type Dependency struct {
	Number     int64 `json:"number"`
	Repository struct {
		Owner    string `json:"owner"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Dependencies lists the issues an issue depends on (is blocked by).
func (c *Client) Dependencies(ctx context.Context, owner, repo string, n int64) ([]Dependency, error) {
	return list[Dependency](ctx, c, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/dependencies", nil)
}

// AddDependency makes issue n depend on depOwner/depRepo#dep.
func (c *Client) AddDependency(ctx context.Context, owner, repo string, n int64, depOwner, depRepo string, dep int64) error {
	return c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/dependencies",
		map[string]any{"owner": depOwner, "repo": depRepo, "index": dep}, nil)
}

// Subscribers lists an issue's watchers.
func (c *Client) Subscribers(ctx context.Context, owner, repo string, n int64) ([]User, error) {
	return list[User](ctx, c, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/subscriptions", nil)
}

// Subscribe makes user watch an issue.
func (c *Client) Subscribe(ctx context.Context, owner, repo string, n int64, user string) error {
	return c.send(ctx, http.MethodPut, repoPath(owner, repo)+"/issues/"+strconv.FormatInt(n, 10)+"/subscriptions/"+url.PathEscape(user), nil, nil)
}
