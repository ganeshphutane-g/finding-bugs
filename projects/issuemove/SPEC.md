# issuemove: spec

## Problem

Gitea and Forgejo cannot move an issue to another repository.
[go-gitea/gitea#453](https://github.com/go-gitea/gitea/issues/453), "Move issues
between repositories", has been open since 2016 (79 👍, 37 comments, still updated
in 2026). New duplicates keep appearing, such as #39227 in September 2026.

People hit this when they:
- open an issue in the wrong repository;
- split a monorepo;
- move a project into an organisation;
- move from one instance to another.

Today the choice is to retype the issue or write a one-off script, and either way
the discussion, labels, attachments and who-said-what are lost. No maintained tool
exists: the closest are a YouTrack-to-Gitea script (2★) and a Gitea-to-GitHub mover
(0★).

## What v1 does

`issuemove [OPTIONS] SOURCE TARGET`, where SOURCE is `owner/repo#12`, `owner/repo#12-20`
or `owner/repo` with `--label`/`--state` filters, and TARGET is `owner/repo`.
Either side can be on another instance (`--source-url`, `--target-url`, a token for
each).

- **Plans first.** `--dry-run` (the default without `--yes`) lists every issue and
  what will happen to it:
  - labels and milestones it will match or create;
  - users it can and cannot keep as authors;
  - attachments, and anything it will drop with the reason.
- **Copies faithfully, through the API:**
  - title, body, every comment, labels (matched by name, created when missing, with
    colour and description), milestone (matched by title or created), assignees
    who can be assigned in the target, state (open or closed), due date;
  - attachments on the issue and on comments, with links in the text rewritten to
    the new attachments;
  - reactions;
  - dependencies (blocked-by links), pointed at the new numbers when both issues
    move together.

  Watchers are not copied: the subscription list API was unreliable on Gitea 1.24
  (empty even after a successful subscribe). Gitea subscribes everyone who writes on
  an issue, and with an admin token the participants post as themselves.
- **Keeps who wrote what.** With an admin token on the same instance, every issue,
  comment and reaction is posted as its original author (Gitea's `Sudo`, confirmed on
  Gitea 1.24). Otherwise each one starts with a short line naming the original
  author and date. Original dates cannot be set through the API (confirmed: the
  create options have no date fields), so they are always kept in that line.
- **Leaves a trail:**
  - the new issue links back to the original;
  - the original gets a comment "Moved to owner/repo#34" and is closed (or kept
    open with `--keep-open`), and can be locked (`--lock`);
  - references to other issues in the same source repository (`#5`) are rewritten
    to `owner/repo#5`, or to their new number when moved together, so links keep
    working.
- **Safe to re-run.** A journal records what was copied: a run interrupted halfway
  resumes without creating duplicates. Each new issue carries a hidden marker naming
  its source.

## Non-goals for v1

- Pull requests: their branches and reviews cannot be moved through the API.
- Keeping original timestamps natively: that needs database access.
- Projects (kanban) and time-tracking entries (maybe later).
- GitHub or GitLab as source or target.

## Tech choices

- **Go:** one binary, no dependencies beyond the standard library, talking to the
  Gitea/Forgejo API v1.
- **Tests:**
  - locally, against a real Gitea in Docker;
  - in GitHub Actions, against both Gitea and the official Forgejo image (codeberg.org
    cannot be reached from the development machine, so Forgejo runs only in CI);
  - multi-user setups, attachments and an interrupted run, on one instance and
    across two.

## Milestones

1. **Client:** the API client (pagination, errors, Sudo, multipart uploads),
   SOURCE/TARGET parsing, and the read-only plan (`--dry-run`) with the full report.
2. **Copy:** issues, comments, labels, milestones, assignees, state, attachments
   with link rewriting, reactions, authorship (Sudo or attribution line), the
   journal and resume.
3. **Finish:** the back-link, closing and locking the source, reference rewriting,
   dependencies, watchers, moving several issues at once with a mapping, and
   cross-instance moves.
4. **Release:** a CI matrix (Gitea, Forgejo), README with real output, full
   reviews, LICENSE, CHANGELOG, v1.0.0.
