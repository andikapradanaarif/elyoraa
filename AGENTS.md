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

- The server needs `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, `DEVIN_BIN`, `WORKSPACE_BASE`, and `DEVIN_CONFIG_PATH`.
- Set `DATABASE_URL` (optional) to enable the PostgreSQL webhook audit log.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue.
- The Devin auth spike is not yet implemented.

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
