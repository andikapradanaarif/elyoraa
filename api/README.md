# elyoraa — AI PR Reviewer (API)

Go webhook engine that receives GitHub `pull_request` and `issue_comment` events and reviews PRs using the Crush CLI through bounded repository tools.

## Quick start

```bash
cd api
cp .env.example .env
# edit .env with your GitHub App credentials and secret

make dev
```

For local development, `make dev` runs the server directly with `go run`.

## Migrations

Migrations are applied automatically when the server starts. To run them manually:

```bash
# Apply all pending migrations (DATABASE_URL required)
make migrate-up

# Revert the last applied migration (DATABASE_URL required)
make migrate-down
```

The server listens on `PORT` (default `8080`) and exposes:

- `POST /webhook/gh` — GitHub webhook endpoint. Listens for:
  - `pull_request` (`opened`, `reopened`, `ready_for_review`) — auto-review.
  - `issue_comment` — bot mention on PR triggers rereview; on issue triggers analysis.
  - `pull_request_review` / `pull_request_review_comment` — bot mention triggers rereview.
  - `issues` (`opened`, `edited`, `reopened`) — bot mention in issue body triggers analysis.
- `GET /health` — liveness.
- `GET /metrics` — Prometheus-style metrics.

## Docker

### Local compose build

```bash
cd api/deploy
cp .env.example .env
# edit .env

# Create the private-key secret file directory
mkdir -p secrets
# Place your GitHub App private key at the path you set in .env

docker compose up --build
```

### Push to GHCR

```bash
cd api
./deploy/deploy-ghcr.sh
# or with no cache:
# GHCR_TOKEN=your_pat GITHUB_USER=your-username ./deploy/deploy-ghcr.sh --no-cache
```

## Requirements

- A GitHub App with the `pull_requests:read`, `pull_requests:write`, and repository contents permissions.
- The GitHub App private key.
- The [Crush fork](https://github.com/andikapradanaarif/crush), installed as `crush` (or set `CRUSH_BIN`).
- Your configured `~/.config/crush` and `~/.local/share/crush` directories, including any files sourced by your trusted `crushrc`.
- `git` for server-side repository operations.

## Project layout

```
api/
├── cmd/
│   ├── server/             # main server entrypoint
│   └── migrate/            # manual migration command
├── internal/
│   ├── config/             # env config
│   ├── handler/            # HTTP handlers (webhook, health, metrics)
│   ├── usecase/            # business logic (webhook, review orchestration)
│   ├── repository/         # PostgreSQL pool, webhook store
│   ├── repository/queue/   # in-memory (default) or BoltDB file-backed worker queue
│   ├── repository/cache/   # bare-clone cache + worktrees
│   ├── domain/             # shared domain types (Review, WebhookEvent)
│   ├── service/github/     # GitHub App auth, PR fetch, review posting
│   ├── dedup/              # in-flight job deduplication
│   ├── metrics/            # Prometheus text metrics
│   ├── middleware/         # request ID / access logging
│   └── startup/            # startup validation
├── migration/              # versioned PostgreSQL migrations and runner
├── prompts/                # Crush prompt templates (review, reconcile, split, issue, respond, gate)
└── deploy/                 # Dockerfile, compose, askpass, GHCR push script
```

## Notes

- **Repositories must be registered manually** — `WORKSPACE_BASE` is the registry. Run `git clone --bare https://github.com/{owner}/{repo}.git WORKSPACE_BASE/{owner}/{repo}.git` on the server (in Docker, clone into `WORKSPACE_HOST_PATH` on the host); a plain clone suffices since the required fetch refspecs self-repair on first use. Every job refreshes the clone with `git fetch --prune` — branches (`refs/heads/*`) and PR heads (`refs/remotes/origin/pr/*`) both stay current. Webhooks for unregistered repos are dropped silently; a job whose repo is unregistered after enqueue is skipped.
- **Repository access is enforced in Go:** Crush gets MCP tools to read files, list files, search text, inspect Git history, and refresh refs. Arbitrary shell execution, editing, remote Git commands, and unrestricted file tools are unavailable. Path traversal, symlinks, `.git`, env files, and private-key files are blocked by the repository reader.
- Per-attempt Crush data and temporary files are isolated. GitHub installation tokens stay in the server; `mcp_elyoraa_sync` refreshes only the job's registered repository.
- Set `DATABASE_URL` to enable PostgreSQL webhook audit logging.
- The server automatically loads `.env` at startup (or the file in `ENV_FILE`) so you do not have to export every variable to the shell.
- `CRUSH_MODEL` is optional. When set, it is passed as `--model`; otherwise Crush uses the model in your configuration.
- `BOT_HANDLE` (default `elyoraa`) is used to detect rereview requests in comments.
- The bot reviews open, non-draft PRs once. It does not re-review on later pushes unless the author mentions the bot (e.g., `@elyoraa rereview`).
- If the bot finds no issues it approves the PR; otherwise it requests changes.
- The server fails fast at startup if `git`, `crush`, the GitHub App private key, or `WORKSPACE_BASE` are missing or misconfigured.
- Migrations live in `migration/migrations` and are applied automatically at startup.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue.

## Crush runtime

Local development uses the system `crush` binary. The integration targets
[andikapradanaarif/crush](https://github.com/andikapradanaarif/crush), currently
`759066acc9c0fd62dcb74bc8473e171dee9c11d2`. Docker builds that fork at the same
pinned revision (`CRUSH_REPO` / `CRUSH_REF` build arguments), using Go 1.27.4
for Crush; the API keeps its Go 1.25 builder.

The runner invokes `crush run --quiet`, passes prompts over stdin, and reads
stdout separately from stderr. `CRUSH_MODEL`, `RESPOND_MODEL`, and `GATE_MODEL`
can override model selection. Retry controls are `CRUSH_MAX_RETRIES`,
`CRUSH_RETRY_BACKOFFS`, `CRUSH_ATTEMPT_TIMEOUT`, and `CRUSH_PREFLIGHT_CHECK`
(the preflight checks `crush --version`; the run validates provider access).

Your trusted global JSON and `crushrc` configuration load normally from
`CRUSH_CONFIG_DIR` (default `CRUSH_GLOBAL_CONFIG`, then
`$XDG_CONFIG_HOME/crush` or `~/.config/crush`) and `CRUSH_AUTH_DATA_DIR`
(default `CRUSH_GLOBAL_DATA`, then `$XDG_DATA_HOME/crush` or
`~/.local/share/crush`). This preserves custom providers, models, and auth
setup. Crush runs in a private job directory with a generated project policy,
so configuration from the reviewed repository is never loaded. Native tools
are disabled, and a `PreToolUse` guard permits only the elyoraa repository
tools and mem0. Automatic project indexing and cross-session notebook hydration
are disabled for job isolation; in-session context management remains available.
Global hooks remain trusted executable configuration.

For Compose, replace the old Devin mounts with `CRUSH_CONFIG_HOST_PATH` and
`CRUSH_DATA_HOST_PATH`, pointing at your host's Crush config/data directories.
Paths must be absolute. Any files referenced by your `crushrc` must also exist
inside the container. Remove old `DEVIN_*` variables; use the `CRUSH_*`
variables in the example env files. Devin sandbox and permission-mode flags
have no Crush equivalent here. The container no longer needs bubblewrap,
socat, `SYS_ADMIN`, or an unconfined seccomp profile.
