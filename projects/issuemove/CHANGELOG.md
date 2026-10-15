# Changelog

## 1.0.0 — 2026-10-16

First release.

- `plan` shows, without changing anything:
  - every issue with its comments, attachments, reactions, labels and milestone;
  - labels and milestones to create;
  - who is posted as themselves and who is named in the text;
  - what blocks the move: archived repositories, pull requests, missing issues,
    unknown labels, a token that cannot write, issues already moved elsewhere.
  - It also warns about dropped reactions, re-sent mentions and private-to-public
    moves.
- `move` copies:
  - title, text and comments;
  - attachments, with their links updated;
  - labels, milestone, assignees, due date, state and reactions;
  - dependencies, both ways on one instance.
- References to other issues are rewritten. The originals get a link and are
  closed (`--keep-open`, `--lock`).
- With an admin token everything is posted as its original authors; otherwise
  each issue and comment names them.
- Across instances, authors are matched only with `--match-users`, and other
  @mentions are quietened.
- Repeating a command resumes an interrupted move without duplicates. Only
  markers posted by the token's user or the original author count.
- Tokens come only from the environment. A shared token is never sent to two
  instances, and it is dropped on redirects to another origin.
- Tested against Gitea 1.24 and Forgejo 16.
