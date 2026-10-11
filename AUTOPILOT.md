# Autopilot playbook

Instructions for the daily build run. Every run starts from a fresh machine with no
memory of earlier runs; everything it needs to know lives in this repo.
Read this whole file before doing anything else.

Files:

- `AUTOPILOT.md`: this playbook. The owner edits it to change the rules.
- `autopilot/STATE.md`: the current project, its phase and the exact next step. Source of truth.
- `autopilot/IDEAS.md`: researched ideas with evidence, chosen and rejected.
- `autopilot/LOG.md`: one entry per run, newest first.

## Goal

Build original open-source projects for Ganesh Phutane (GitHub: `ganeshphutane-g`),
one at a time. Each project must solve a real problem that people have shown, with
evidence, that they want solved. Finish a project before starting the next. If one is
finished early in a run, start researching the next in the same run.

"Worth it" means: someone would actually install and keep using this. A smaller
project that works well beats a large one that is half done.

## Identity and credit (strict)

- Before the first commit, run in every repo you touch:
  `git config user.name "Ganesh Phutane"` and
  `git config user.email "124124767+ganeshphutane-g@users.noreply.github.com"`.
- No `Co-Authored-By` lines, no "Generated with" footers, no session links, and no
  mention of Claude, Anthropic, AI tools, model names or Spheretree in commits, branch
  names, READMEs, code comments, docs, changelogs or repo descriptions.
- LICENSE files name "Ganesh Phutane" as the copyright holder.

## Hard rules

- Everything is original work. Dependencies are fine through normal package managers,
  with their licences respected. Never copy code from another repo; if you must adapt a
  snippet, keep its licence notice and say where it came from.
- Do not touch anyone else's repositories: no issues, PRs, comments, stars or forks.
- New repos are created **private**. The owner decides when to make one public.
  Never change a repo's visibility, delete a repo, force-push or rewrite history.
- Do not publish to package registries, post on social media or contact anyone.
- Do not build anything for spam, scraping against a site's terms, evading detection,
  or collecting personal data.

## Every run, in order

1. Clone this repo if it is not already on disk:
   `git clone https://github.com/ganeshphutane-g/finding-bugs`. Set the git identity.
2. Read `autopilot/STATE.md` and the latest entries in `autopilot/LOG.md`.
3. Continue from the "Next step" in STATE. Do not restart or redo finished work.
4. Push early and often: after every finished milestone and at least every hour of
   work. The run can stop without warning when usage runs out; anything not pushed
   is lost. Update `autopilot/STATE.md` in the same push so the next run knows
   exactly where to pick up.
5. Before stopping, add a LOG entry: date (IST), what was done, what is next, any
   blocker that needs the owner. Push.

## Phases of a project

### 1. Research (phase: `research`)

Collect real demand signals, with links, using web search and GitHub search:

- Feature requests on popular repos with many 👍 that stay open for a long time or
  were closed as "won't fix".
- Popular projects that are archived or abandoned but still have users and open issues.
- "Is there a tool that…?" questions on Reddit, Hacker News, Stack Overflow and forums.
- Paid tools people complain about, where a free or self-hosted version is wanted.

List at least 5 candidates in `autopilot/IDEAS.md`. Score each from 1 to 5 on:
evidence of demand, gap against existing tools (name them), feasibility for one
developer as a first version (aim for about 1–2 weeks of daily runs), maintenance
burden, and audience fit with GitHub users (developers, self-hosters, power users).

Pick the best. An idea is allowed only if it has at least 3 independent demand
sources and a clear reason it beats existing tools. If none passes, keep researching
next run rather than building something weak. Record why the others were rejected.

### 2. Spec (phase: `spec`)

Choose a short, available repo name. Create the repo `ganeshphutane-g/<name>` as
private, then request push access to it. If creating or pushing fails, build under
`projects/<name>/` in this repo instead and note it as a blocker for the owner.

Write `SPEC.md` in the project: the problem, who it is for, the evidence (links),
MVP scope, non-goals, tech choices and why, and milestones. Each milestone should be
small enough to finish and push within one run.

### 3. Build (phase: `build`)

Work milestone by milestone. Each milestone has tests and passes them before it is
pushed. Keep the code simple and readable. Add a GitHub Actions workflow that runs
lint and tests.

### 4. Finish (phase: `finish`)

A project is done only when all of these are true:

- every MVP milestone is done and the tests pass locally and in CI
- the app or tool has been run end to end and actually works as the README says
- the README has: what it is, why it exists, install, quick start, examples and a roadmap
- LICENSE (MIT unless the stack demands otherwise), CHANGELOG and a `v1.0.0` tag exist
- a full review of the code for bugs and security issues has been done and the
  findings fixed

Then set the project to `done` in STATE, record a short summary of what was built and
what the owner should check before making it public, and go back to Research for the
next project.
