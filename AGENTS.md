# elyoraa — agent notes

## Project layout

- Go module lives in `api/`: `github.com/andikapradanaarif/elyoraa`.
- Architecture and plan docs live in `docs/plan/GIT_AI_REVIEWER.md`.
- Deployment artifacts live in `api/deploy/`.

## Build / test

```bash
cd api
go build ./...
go vet ./...
go test ./...
```

`go.mod` is on `go 1.25.0` with `ghinstallation/v2.19.0`, `go-github/v88`, and `pgx/v5.9.1`. The Docker builder image is `golang:1.25-alpine`.

## Docker

```bash
docker build -t elyoraa -f api/deploy/Dockerfile api/

cd api/deploy
cp .env.example .env
# edit .env
docker compose up
```

To push the image to GHCR:

```bash
cd api
GHCR_TOKEN=your_pat ./deploy/deploy-ghcr.sh
```

The image is Debian-based because a host-provided `devin` binary is usually glibc-linked. The `devin` binary and `~/.config/devin` credentials are bind-mounted via `docker-compose.yml`.

## Local development

`api/.env` is loaded automatically by `internal/config.Load` (valo-style), so `make dev` works as soon as you fill in `.env`. Set `ENV_FILE` to use a different env file.

`make dev` fails fast if `git`, `devin`, the GitHub App private key, or `WORKSPACE_BASE` are missing or misconfigured.

## Notes

- The server needs `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, `DEVIN_BIN`, `WORKSPACE_BASE`, and `DEVIN_CONFIG_DIR`.
- `DEVIN_CONFIG_DIR` (directory, not file) is where `devin-config.json` is written at startup. This is safe for directory bind-mounts.
- `PROMPTS_DIR` (default `/app/prompts`) holds the review prompt template.
- mem0 is configured as a remote HTTP MCP server (`https://mcp.mem0.ai/mcp`) when `MEM0_API_KEY` is set. No Node.js required.
- The Docker image includes a `HEALTHCHECK` that hits `GET /health`.
- Set `DATABASE_URL` (optional) to enable the PostgreSQL webhook audit log.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue. Job outcomes (status, error, attempts, payload) are persisted in the same file's `outcomes` bucket.
- `GET /jobs` lists tracked jobs (`?status=failed` filters); `GET /jobs/{key}` shows one; `POST /jobs/retry/{key}` re-enqueues a job without a new webhook. Key format: `owner/repo/pr/headSHA`.
- Devin invocations retry transient failures: `DEVIN_MAX_RETRIES` (default 3, i.e. 4 attempts), `DEVIN_RETRY_BACKOFFS` (default `30s,2m,5m`), `DEVIN_ATTEMPT_TIMEOUT` (default 0 = `REVIEW_TIMEOUT`), `DEVIN_PREFLIGHT_CHECK` (default true, runs `devin models list` before each attempt).
- Timeout env vars: `REVIEW_TIMEOUT`, `COMMENT_TIMEOUT`, `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT` (Go duration syntax, e.g. `10m`, `30s`).
- The Devin auth spike is not yet implemented.

## Review flow

- Repositories must be pre-cloned as bare clones at `WORKSPACE_BASE/{owner}/{repo}.git`. The bot does not clone on demand; if a repo is not cloned, the review is skipped with a comment.
- Webhook events:
  - `pull_request` (`opened`, `reopened`, `ready_for_review`) — auto-review.
  - `issue_comment` — if the comment mentions the bot on a PR, trigger rereview; if on an issue, trigger issue analysis.
  - `pull_request_review` (`submitted`, `edited`) — if the review body mentions the bot, trigger rereview.
  - `pull_request_review_comment` (`created`, `edited`) — if the comment mentions the bot, trigger rereview.
  - `issues` (`opened`, `edited`, `reopened`) — if the issue body mentions the bot, trigger issue analysis.
- Draft PRs and `synchronize` pushes are ignored.
- Issue analysis (`prompts/issue.md`) summarizes the issue, suggests approaches, proposes a breakdown when the scope is too big, and reads the full comment thread (bounded to 50 comments / 48KB) so prior bot replies and human acceptance shape the response. It uses a repo worktree when the repo is pre-cloned, else a temp dir.
- When a human explicitly accepts a proposed breakdown, the reply ends with a `create_issues` JSON block; `processIssue` strips it, creates the issues via the GitHub App, and appends links under the posted comment. Labels are matched case-insensitively against repo labels; a curated vocabulary (plus the `BOT_HANDLE` tag) is auto-created when missing.
- Rereview is triggered by any mention of the bot handle (`BOT_HANDLE`, default `elyoraa`).
- Draft PRs and `synchronize` pushes are ignored.
- Rereview is triggered by a PR comment mentioning the bot handle (`BOT_HANDLE`, default `elyoraa`) with a rereview keyword (`rereview`, `re-review`, `review again`, `recheck`, `check again`).
- The Devin model is configurable via `DEVIN_MODEL` (default `swe-2-high`) and passed with `--model`.
- If the review output has no comments, the bot approves the PR; otherwise it requests changes.
- Issue analysis uses `prompts/issue.md`; PR review uses `prompts/review.md`.

## Architecture

The codebase is organized in a Valo-style layered architecture:

- `cmd/server` — Main server entrypoint.
- `cmd/migrate` — Manual migration command.
- `migration` — Versioned PostgreSQL migrations, imported by both commands.
- `internal/handler` — HTTP handlers (webhook, health, metrics).
- `internal/usecase` — Business use cases (`WebhookUsecase`, `ReviewUsecase`).
- `internal/repository` — PostgreSQL pool and the `WebhookStore` interface.
- `internal/repository/queue` — Queue backend implementations.
- `internal/repository/cache` — Git bare-clone and worktree cache.
- `internal/domain` — Shared domain types (`Review`, `WebhookEvent`).
- `internal/service/github`, `internal/dedup`, `internal/metrics`, `internal/middleware`, `internal/startup`, `internal/config` stay as supporting packages.
