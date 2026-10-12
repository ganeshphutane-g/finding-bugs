# Ideas

Researched candidates, with evidence. Newest research first.
Scores are 1–5 (higher is better): demand, gap, feasibility, maintenance (5 = light), audience fit.

## Research round 2 — 2026-10-13 IST

### Chosen: bucketmove (project #2)

A CLI for leaving MinIO safely. It lists what a MinIO deployment uses (buckets, object
versions, delete markers, object lock, lifecycle, policies, CORS, tags), probes what
the target (Garage, SeaweedFS, RustFS, Ceph RGW, AWS S3) really supports and reports
exactly what would be lost, then copies every bucket **with all object versions in
order**, and verifies the result.

Scores: demand 5, gap 5, feasibility 4, maintenance 3, fit 5 (total 22).

Evidence:
- minio/minio (61.3k stars) is archived; maintenance mode from 2025-12-03, archived
  2026-04-25 (https://pinggy.io/blog/minio_archived_self_hosted_s3_alternatives/).
  MinIO's images are gone from Docker Hub (checked: `minio/minio` pull denied).
- Migration questions on the targets: rustfs/rustfs#98 (17 comments), #759, #2283,
  #1489, #6295 (IAM migration bug, Aug 2026); seaweedfs#7586 (replacing MinIO).
- Migration guides do it by hand with rclone and lose versions, tags and policies:
  https://jacar.es/en/migrate-minio-to-garage-with-docker/ (also: Garage has no
  versioning, object lock, bucket policies or object tags, so users need to be told
  before they lose them). Tor's TPA-RFC-96 plans MinIO → Garage.
- Existing tools: rclone (no version history), `mc mirror` (current versions only),
  repos named minio-migrate with 0–1 stars. GitHub search for version-preserving S3
  migration tools found nothing established.

Testable here: MinIO builds from source via the Go module proxy (2.5 min); Garage,
SeaweedFS and RustFS images pull from Docker Hub.

### Runners-up

- **VS Code layered settings composer** (CLI + thin extension). Still open upstream
  (#15909 ~390+ 👍, last comment 2026-07; #5595, #40233). Community PR #335281 would
  add per-OS blocks natively. Alternatives tiny/dormant. Scores 5/4/4/3/4 = 20.
- **Dead image-reference auditor** (compose/k8s/Helm images from removed sources like
  docker.io/bitnami): bitnami/charts#35256, #35351, #36268. Scores 4/4/4/3/4 = 19.
- **Ansible-core 2.19 upgrade preflight** (broken conditionals/templating). Evidence
  thinner (ansible#85849 4 👍, netlab, VyOS). Scores 3/4/4/3/3 = 17.
- **Format-preserving yq/jq-style editor** (yq#515 156 👍, jq#105 381 👍). Scores
  4/3/4/3/4 = 18; small competitors exist.
- **Home Assistant add-on → Compose converter**. Scores 4/4/3/2/3 = 16.
- **Forgejo/Woodpecker Actions portability linter**: clear gap, weak demand data (16).

### Rejected in round 2

- Terraform visualizer: now crowded with small 2026 tools; big UI effort.
- termshark successor: too large; Wireshark team may adopt; ownership questions.
- Already solved: Alertmanager ack (kthxbye), Uptime Kuma config (AutoKuma), Komodo
  import, Portainer CE, NPM export, Node20 Actions, npm trust, ingress2gateway.

## Research round 1 — 2026-10-12 IST

### Chosen: docker-mop (project #1)

A Docker CLI plugin that shows exactly what a prune would delete, and how much space
it would free, before anything is removed. It also renames and copies volumes, which
Docker cannot do.

Scores: demand 5, gap 5, feasibility 5, maintenance 4, fit 5 (total 24).

Evidence:
- moby/moby#30623, "prune --dry-run": 391 👍, 110 comments, open since 2017-02-01.
  https://github.com/moby/moby/issues/30623
- moby/moby#31154, "volume rename / copy": 576 👍, open since 2017-02-18.
  https://github.com/moby/moby/issues/31154
- Docker Forums, "Is there a way to rename volumes?":
  https://forums.docker.com/t/is-there-a-way-to-rename-volumes/4570
- Workaround guides that copy data with a throwaway container, e.g.
  https://codemia.io/knowledge-hub/path/docker_volume_rename_or_copy_operation
- Podman ships `podman volume rename`, which shows the feature is wanted and
  workable; Docker still has none.

Existing tools: only tiny repos with 0–1 stars (kirankiran1234/docker-gc-safe,
gh640/docker-volumerename, kevinveenbirkenbach/docker-volume-renamer,
stellarshenson/docker-volume-toolkit). Nothing established.

Why it is worth it: two of the most-upvoted long-open Docker requests, used by a huge
audience, solved by one small Go binary. It can be tested end to end here against a
real Docker daemon.

Main risk: the preview must match Docker's real prune rules exactly (including `-a`,
`until` and `label` filters), otherwise people will trust a wrong list. Tests must
compare the preview with what a real prune actually deletes.

### Runners-up (kept for later rounds)

- **VS Code layered settings composer** (base + per-OS + gitignored local layers).
  Five microsoft/vscode issues with 741–1,366 👍 each (#15909, #5595, #40233, #2809,
  #40239). Best alternative has 35 stars. Scores 5/4/4/3/5 = 21. Risk: Microsoft may
  build it in.
- **Terraform-visualiser successor to rover** (im2nguyen/rover, 3.3k stars, "is this
  project dead?" #143, Dec 2025). Scores 3/4/4/3/4 = 18.
- **Terraform exclude/glob targeting wrapper** (hashicorp/terraform#2253, 1,539 👍;
  #2182, 657 👍). Scores 5/3/4/3/4 = 19. Risk: encourages risky partial applies;
  OpenTofu already has `-exclude`.
- **Gmail POP/Gmailify replacement** (Gmail dropped it in Jan 2026; Ask HN thread,
  The Register article, paid services appearing). Scores 4/3/3/2/3 = 15. Risk: Google
  OAuth restricted scopes and storing mail passwords.
- **Opsgenie / Grafana OnCall → GoAlert migrator** (OnCall OSS archived 2026-03-24;
  Opsgenie support ends 2027-04-05). Scores 3/4/3/3/3 = 16.
- **termshark successor** (10k stars; maintainer has died, issue #167; broken with
  Wireshark 4.6, #169). Scores 4/5/3/2/4 = 18. Wireshark team may adopt it; handle
  with care.
- **GitHub Actions → Forgejo/Woodpecker portability linter**. Scores 3/4/4/3/3 = 17.

### Rejected

- **pyproject.toml version bumper**: crowded field and uv may ship `uv upgrade`.
- **Zed: open VS Code workspaces**: moderate demand; Zed is likely to build it in.
- **Hyprland session restore**: hyprsession (102 stars) already covers it.
- **Discord → Matrix migration**: Discord terms and member-consent problems.
- **Confluence DC → wiki importer**: demand evidence is mostly vendor blogs.
- **Heroku exit CLI**: plausible; revisit with stronger user evidence.
- **Already solved**: Codex rewind tools, cargo-chef, thefuck (pay-respects),
  Todo Tree fork, ctop alternatives, kcat alternatives, garage-webui.
