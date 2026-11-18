# elyoraa

Self-hosted GitHub App that reviews pull requests — and triages issues and
bot mentions — with an AI agent running behind a hard tool boundary.

elyoraa receives GitHub webhooks, fans work out to a durable job queue, and
drives the [Crush CLI](https://github.com/andikapradanaarif/crush) (a fork of
[charmbracelet/crush](https://github.com/charmbracelet/crush)) as the review
agent. The agent never gets a shell, write access, or credentials: every
repository operation goes through a small set of read-only MCP tools
implemented — and enforced — in Go.

## What it does

- **Automatic PR reviews** on `opened`, `reopened`, and `ready_for_review`.
  Approves clean PRs, requests changes with inline comments otherwise.
  Output parsing is strict — a malformed review is retried, never silently
  approved.
- **Two-phase review.** A fresh review pass runs with no prior context,
  then a reconcile pass re-verifies the previous bot review's findings
  against the current code (`fixed` / `still_present` / `unclear`), so stale
  findings don't re-raise and prior context can't bias the fresh pass.
- **Size gate.** Oversize PRs get a structured split proposal instead of a
  shallow review — measured against a per-repo, self-calibrating
  lines-per-minute budget that excludes generated files and repo-configured
  `ignored_paths`. `SIZE_GATE_MODE=agent` optionally lets an agent choose
  review-vs-split for borderline cases.
- **Issue analysis.** `@elyoraa` on an issue summarizes it, suggests
  approaches, and proposes a work breakdown when the scope is too big — a
  human acceptance turns the proposal into real GitHub issues with labels.
- **Mention routing.** `MENTION_ROUTER=keywords` (default) is deterministic;
  `=agent` runs a `respond` job that reads the merged discussion and
  declares bounded actions (`request_review`, `request_split`,
  `request_issue_analysis`, `create_issues`, or reply-only). Slash commands
  (`/review`, `/split`, `/accept`) bypass the router entirely.
- **Status comments.** Every job posts a transient comment at enqueue,
  flips it to in-progress on pickup, and replaces it with the result — so
  queue waits and retries are visible on the PR.
- **Per-repo policy.** `.elyoraa.yml` on a repo's *default branch* controls
  enable/disable, budget, ignored paths, router mode, size-gate mode, and a
  skip label — a PR can't change its own policy.
- **Operational guardrails.** Per-author/repo mention rate limits, daily
  per-repo job caps, a global pause switch (`ELYORAA_PAUSED`), an
  `elyoraa:skip` opt-out label, and job dedup.

## Architecture

```
GitHub webhook
   │  (HMAC verify, dedup, rate limit, size gate, .elyoraa.yml policy)
   ▼
Job queue ─────────────── memory │ BoltDB file │ Postgres (multi-replica)
   │
   ▼
Worker ──► bare clone fetch (installation token, GIT_ASKPASS)
   │       worktree for the PR head
   ▼
Per-job trusted dir: generated crush.json
   │   • all native Crush tools disabled
   │   • PreToolUse hook allows only mcp_elyoraa_* / mcp_mem0_*
   ▼
crush run --quiet (prompt on stdin, stdout = agent text only)
   │
   ▼
Loopback MCP server (per-run bearer token):
   read_file · list_files · search · git (read-only argv) · sync
   │
   ▼
Review posted via the GitHub App (output passes redactSecrets)
```

Layered Go layout: `handler > usecase > repository`. See
[`api/README.md`](api/README.md) for the implementation details and
[`docs/plan/`](docs/plan/) for the design notes.

## Security model

- **No shell, no writes, no network for the agent.** Native Crush tools are
  disabled and a `PreToolUse` hook denies anything that isn't an elyoraa
  repository tool or mem0 — new native tools in future Crush builds fail
  closed against the same guard.
- **Read-only tools, enforced in Go.** The `git` tool takes argv (no shell)
  with a command/flag allowlist; file reads are rooted at the worktree with
  symlink, `.git`, env-file, and private-key filtering; all tool output is
  bounded.
- **Tokens never reach the agent.** GitHub App installation tokens are
  minted server-side only; the `sync` tool refreshes just the job's
  registered repo. Fetches use a `GIT_ASKPASS` helper locked to
  `GIT_BASE_URL`.
- **Untrusted repos can't execute config.** Crush runs from a private,
  per-job directory — a reviewed repo's `crush.json`/`crushrc` is never
  loaded. Only the operator's trusted global config applies.
- **Action authorization in Go.** Agent-declared actions
  (`request_review`, `create_issues`, …) are validated server-side against
  the trigger's PR-author/COLLABORATOR association — the prompt can't
  escalate privileges.
- **Ops endpoints are gated.** `GET /jobs*`, `POST /jobs/retry/{key}`, and
  `/metrics` require `Authorization: Bearer $ADMIN_TOKEN` (404 when unset).
  Webhook bodies are size-capped; everything written back to GitHub passes
  secret redaction.

## Quick start

### Requirements

- A **GitHub App** with pull-request and issues read/write, contents, and
  webhook permissions, plus its private key.
- The [Crush fork](https://github.com/andikapradanaarif/crush) installed as
  `crush` (or set `CRUSH_BIN`) with configured providers —
  `~/.config/crush` and `~/.local/share/crush`.
- `git`, and (optionally) Docker for deployment, Postgres for the audit
  log / HA queue.

### 1. Register repositories

`WORKSPACE_BASE` is the registry — webhooks for unregistered repos are
dropped silently. Clone each repo you want reviewed:

```bash
git clone --bare https://github.com/{owner}/{repo}.git \
  $WORKSPACE_BASE/{owner}/{repo}.git
```

The required fetch refspecs self-repair on first use; every job refreshes
the clone (branches and PR heads) before reviewing.

### 2. Run locally

```bash
cd api
cp .env.example .env   # fill in GITHUB_APP_ID, private key path, webhook secret
make dev               # fails fast on missing git/crush/key/workspace
```

The server auto-loads `.env` (or `ENV_FILE`). `go build`, `go vet`, and
`go test` all run cleanly from `api/`.

### 3. Deploy with Docker

```bash
cd api/deploy
cp .env.example .env   # WORKSPACE_HOST_PATH, CRUSH_*_HOST_PATH, app creds
docker compose up --build
```

The image builds elyoraa **and** the pinned Crush fork (`CRUSH_REPO` /
`CRUSH_REF` build args) — no host crush binary, bubblewrap, or elevated
sandbox capabilities needed.

### 4. Push and roll out via GHCR

```bash
cd api
GHCR_TOKEN=<pat-with-write:packages> ./deploy/deploy-ghcr.sh
```

Deploys pull `ghcr.io/<owner>/elyoraa:<sha>` (or `:latest`). Set
`ELYORAA_IMAGE` to the SHA tag, then `docker compose pull &&
docker compose up -d`. Rollback = redeploy the previous SHA. A
`deploy-ghcr` GitHub Actions workflow automates this (optionally
SSH-deploys with `deploy_remote=true`).

## Configuration

All settings come from environment variables — see
[`api/.env.example`](api/.env.example) and
[`api/deploy/.env.example`](api/deploy/.env.example) for the annotated
list. Highlights:

| Variable | Default | Purpose |
|---|---|---|
| `GITHUB_APP_ID` / `GITHUB_APP_PRIVATE_KEY` / `GITHUB_WEBHOOK_SECRET` | — | Required GitHub App credentials |
| `WORKSPACE_BASE` | `/var/lib/elyoraa/workspace` | Bare-clone registry |
| `CRUSH_BIN` / `CRUSH_MODEL` | `crush` / unset | Agent binary / model override (`RESPOND_MODEL`, `GATE_MODEL` per-type) |
| `QUEUE_BACKEND` | `memory` | `file` (BoltDB, survives restarts) or `postgres` (multi-replica) |
| `DATABASE_URL` | unset | Postgres audit log (+ `postgres` queue backend) |
| `MENTION_ROUTER` / `SIZE_GATE_MODE` | `keywords` / `deterministic` | `agent` variants use LLM routing/triage |
| `REVIEW_TIMEOUT` | `30m` | Whole-job budget (keep ≥ `PR_REVIEW_BUDGET_MINUTES`) |
| `PR_REVIEW_BUDGET_MINUTES` / `PR_LINES_PER_MINUTE` | `15` / `100` | Size-gate budget and fallback rate |
| `ADMIN_TOKEN` | unset | Required for `/jobs*` and `/metrics` |
| `ELYORAA_PAUSED` | `false` | Acknowledge-and-drop kill switch for all events |

## Operations

- `POST /webhook/gh` — GitHub webhook endpoint.
- `GET /health` — liveness (stays up while paused).
- `GET /jobs`, `GET /jobs/{key}`, `POST /jobs/retry/{key}` — job tracking
  and re-enqueue (admin-gated).
- `GET /metrics` — Prometheus-style counters, including
  `elyoraa_reactions_total` for +1/-1 reactions on bot comments.

Jobs are deduplicated by key, retried with backoff on transient Crush
failures, and reconciled at startup (`processing`/`queued` records with no
surviving queue row are failed visibly). Shutdown drains in-flight jobs up
to `SHUTDOWN_DRAIN_TIMEOUT`; with `QUEUE_BACKEND=file` or `postgres`,
killed jobs re-run on boot. With `postgres`, N replicas are safe — workers
claim via `FOR UPDATE SKIP LOCKED` with lease-based requeue.

## Project layout

```
├── api/               # the Go service (module github.com/andikapradanaarif/elyoraa)
│   ├── cmd/           # server + migrate entrypoints
│   ├── internal/      # config, handler, usecase, repository, service, queue, cache
│   ├── migration/     # versioned Postgres migrations
│   ├── prompts/       # Crush prompt templates
│   └── deploy/        # Dockerfile, compose, git-askpass, GHCR script
├── docs/plan/         # design docs (reviewer, mention router, chat deploy engine)
└── AGENTS.md          # contributor/agent notes (build, deploy, internals)
```

## License

Not yet specified — see `LICENSE` when added.
