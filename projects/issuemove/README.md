# issuemove

Move issues from one Gitea or Forgejo repository to another, on the same instance
or across two. The comments, labels, milestones, assignees, attachments, reactions,
dependencies and the names of who wrote what all come along. Each original is
closed with a link to its new place.

Gitea has no way to move an issue.
[go-gitea/gitea#453](https://github.com/go-gitea/gitea/issues/453), "Move issues
between repositories", has been open since 2016 (79 👍), and new duplicates keep
appearing. Until now the choice was to retype the issue or write a one-off script.
Either way the discussion, attachments and authorship got lost.

## What it looks like

`plan` reads everything and changes nothing:

```console
$ export ISSUEMOVE_URL=https://git.example.org ISSUEMOVE_TOKEN_FILE=~/.config/issuemove-token
$ issuemove plan --label docs acme/website acme/docs
Move 2 issue(s) from acme/website to acme/docs (on the same instance)
  #1     open     Install guide skips the database step (1 comment(s); 1 attachment(s), 23 B; 1 reaction(s); labels docs; milestone Docs 2.0)
  #2     open     Explain the database settings (0 comment(s); labels docs)
Labels to create in acme/docs: docs
Milestones to create in acme/docs: Docs 2.0
Posted as their original authors: dev, maria
Original dates are kept in the text; the originals get a link to the new issues and are closed.

Nothing blocks the move. Nothing has been changed.
```

`move` shows the same plan, asks, and does it:

```console
$ issuemove move --label docs acme/website acme/docs
...
Move 2 issue(s) to acme/docs? [y/N] y
created label docs
created milestone Docs 2.0
acme/website#1 -> acme/docs#1 Install guide skips the database step
acme/website#2 -> acme/docs#2 Explain the database settings

Moved 2 issue(s) to acme/docs; the originals have a link to their new place and are closed.
```

acme/docs#1 is opened by maria, as the original was. It has the label `docs`, the
milestone `Docs 2.0`, the assignee dev and dev's 👍. Its text is:

```markdown
> Moved from [acme/website#1](https://git.example.org/acme/website/issues/1), opened 2026-10-15 19:52 UTC.

Step 3 says to run `migrate`, but the guide never says to create the database first.
```

dev's comment is posted by dev, with the screenshot copied and its link updated.
In #2, "Follow-up to #1" points to the new #1. The original acme/website#1 is closed
with a comment saying "Moved to acme/docs#1."

The output above is from a real run against Gitea 1.24.7. Running the same command
again copies nothing more: the plan says each issue was already moved, and
`move` reports "acme/website#1 was moved to acme/docs#1 before".

## Install

```sh
make build        # or, once published: go install github.com/ganeshphutane-g/issuemove@latest
```

issuemove is one binary with no dependencies. It talks to the Gitea/Forgejo API
(v1) and is tested against Gitea 1.24 and Forgejo 16.

## Usage

```
issuemove plan|move [OPTIONS] SOURCE TARGET
```

SOURCE can name issues in any of these forms:
- `owner/repo#12`
- `owner/repo#12,15`
- `owner/repo#12-20`
- `owner/repo` with `--state open|closed|all` (default open) and/or
  `--label a,b`. Unknown label names are refused rather than ignored.

TARGET is `owner/repo`.

| Option | |
|---|---|
| `--url URL` | the instance (or `$ISSUEMOVE_URL`) |
| `--source-url`, `--target-url` | when the two repositories are on different instances |
| `--keep-open` | leave the originals open (they still get the link) |
| `--lock` | lock the originals' conversation |
| `--match-users` | on another instance, post as the accounts with the same name and email |
| `--json` | print the plan as JSON (`plan` only; emails are left out) |
| `-y`, `--yes` | do not ask before moving |

**Tokens** are read from the environment, never from the command line:
- `ISSUEMOVE_SOURCE_TOKEN` and `ISSUEMOVE_TARGET_TOKEN`, or their `_FILE`
  variants that name a file holding the token;
- within one instance, `ISSUEMOVE_TOKEN` or `ISSUEMOVE_TOKEN_FILE` serves for both.

When the instances differ, the shared token is refused, so a token never reaches a
server it was not made for.

**Exit codes:**

| Code | Meaning |
|---|---|
| 0 | done |
| 1 | an error, or you answered no |
| 2 | bad usage |
| 3 | the plan is blocked |
| 130 | interrupted |

## Who appears as the author

- **An admin token on the target**, same instance: every issue, comment and reaction
  is posted as its original author, through Gitea's `Sudo`. Someone who cannot see
  the target repository, such as a private one they are not a member of, is the
  exception: what they wrote is posted by the token's user and starts with a line
  naming them. The plan lists these people.
- **Any other token**: everything is posted by the token's user, and each issue and
  comment starts with "Opened by @maria on 2026-10-15 19:52 UTC in acme/website#1".
  Reactions cannot be attributed in text, so they are left out; the plan says how
  many.
- **Another instance**: the same name can belong to someone else. Authors are
  therefore named in the text unless you pass `--match-users`. With it, an account
  with the same name and the same real email is posted as; placeholder `noreply`
  emails never match. Other @mentions are quietened with a zero-width space so
  strangers are not notified.

The original dates cannot be set through the API, so they are always kept in the
text.

## What is copied

- title and text, with references rewritten:
  - `#5` becomes `#<new number>` when #5 moves too, or `owner/repo#5` (a link on
    another instance) when it stays;
  - code and `[text](#anchor)` links are left alone.
- every comment, in order;
- attachments on the issue and on comments, with their links pointed at the copies;
- labels, matched by name or created with the same colour and description, including
  organisation labels;
- the milestone, matched by title or created;
- assignees who can be assigned in the target;
- the due date and the open/closed state;
- reactions;
- dependencies:
  - "depends on" links point at the new numbers when both issues move;
  - an issue left behind that depended on a moved one now depends on its copy, so
    closing the original does not unblock it (same instance only).

Labels, milestone and assignees are set by the token's user after the issue exists.
Gitea silently drops them when the author cannot write to the target.

Not copied: pull requests (their branches and reviews cannot be moved through the
API), watchers (Gitea subscribes everyone who writes on an issue), projects and
tracked time.

## Safe to repeat

Every copied issue and comment ends with a hidden marker naming its source, and the
original gets a marked "Moved to" note. If a run stops halfway (network, Ctrl-C, a
server error), run the same command again. It finds what was copied and finishes it
(attachments, labels, links, reactions, references) without duplicating anything.
Copies whose original already has the note are left alone, because people may have
worked on them since.

Markers only count in text posted by the token's user or by the original author.
Marker-like text in copied content is defused. Someone who plants a fake copy or a
fake "Moved to" note cannot divert a move. An issue already moved to a different
repository is refused rather than copied twice.

## Limits

- Each attachment is held in memory while it is copied.
- Requests are made one at a time: moving a few thousand issues takes minutes. The
  plan shows progress on large batches.
- People @mentioned in moved text are notified again when the copies are posted; the
  plan warns when that applies.
- Moving from a private repository to a public one makes everything public; the plan
  warns about it.

## Roadmap

- A file to map users between instances by hand, for accounts whose names or
  emails differ.
- Faster large moves: several requests at once, and attachments streamed instead of
  held in memory.
- Projects (kanban boards) and tracked time.
- GitHub and GitLab as a source.

## Development

```sh
make test         # unit tests
make test-server  # also against a real Gitea in Docker (ISSUEMOVE_IMAGE picks another)
```

CI runs the whole suite against Gitea 1.24 and Forgejo 16.

## License

MIT
