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

`go.mod` is pinned to `go 1.22.0` with `ghinstallation/v2.12.0` and `go-github/v66` so the `golang:1.22-alpine` builder image works. If you run `go mod tidy` with a newer Go toolchain, check that it does not bump `go` past `1.22` or re-add `go-github` v69/v88.

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

## Notes

- The server needs `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, `DEVIN_BIN`, `WORKSPACE_BASE`, and `DEVIN_CONFIG_PATH`.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue.
- The Devin auth spike is not yet implemented.
