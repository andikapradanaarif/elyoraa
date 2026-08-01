# elyoraa — AI PR Reviewer (API)

Go webhook engine that receives GitHub `pull_request` and `issue_comment` events and reviews PRs using the Devin CLI in a sandboxed worktree.

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

- `POST /webhook` — GitHub webhook endpoint. Listens for `pull_request` (`opened`, `reopened`, `ready_for_review`) and `issue_comment` (to request a rereview by mentioning the bot).
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
- `devin` binary and a persistent `~/.config/devin` credentials volume.
- `git` and `bubblewrap`/`socat` in the container for the Devin sandbox.

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
├── prompts/review.md       # Devin prompt template
└── deploy/                 # Dockerfile, compose, askpass, skill
```

## Notes

- **Repositories must be pre-cloned** as bare clones at `WORKSPACE_BASE/{owner}/{repo}.git`. The bot will not clone repos on demand. If a repo is not cloned, the review is skipped with a comment. To register a repo:
  ```bash
  git clone --bare https://github.com/{owner}/{repo}.git {WORKSPACE_BASE}/{owner}/{repo}.git
  git -C {WORKSPACE_BASE}/{owner}/{repo}.git config --local --add \
    remote.origin.fetch "+refs/pull/*/head:refs/remotes/origin/pr/*"
  ```
- The `devin-config.json` is generated at startup to the path in `DEVIN_CONFIG_PATH`.
- Short-lived GitHub installation tokens are written to `ELYORAA_TOKEN_DIR` (default `/tmp/elyoraa`) and supplied to git via `GIT_ASKPASS`.
- Per-review `XDG_CACHE_HOME` and `TMPDIR` are isolated; `HOME` and `XDG_CONFIG_HOME` must point at the Devin credentials volume.
- Set `DATABASE_URL` to enable PostgreSQL webhook audit logging.
- The server automatically loads `.env` at startup (or the file in `ENV_FILE`) so you do not have to export every variable to the shell.
- `DEVIN_MODEL` (default `swe-1.7`) is passed to the `devin` CLI with `--model`.
- `BOT_HANDLE` (default `elyoraa`) is used to detect rereview requests in comments.
- The bot reviews open, non-draft PRs once. It does not re-review on later pushes unless the author mentions the bot (e.g., `@elyoraa rereview`).
- If the bot finds no issues it approves the PR; otherwise it requests changes.
- The server fails fast at startup if `git`, `devin`, the GitHub App private key, or `WORKSPACE_BASE` are missing or misconfigured.
- Migrations live in `migration/migrations` and are applied automatically at startup.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue.
