# elyoraa — AI PR Reviewer (API)

Go webhook engine that receives GitHub `pull_request` events and reviews them using the Devin CLI in a sandboxed worktree.

## Quick start

```bash
cd api
cp .env.example .env
# edit .env with your GitHub App credentials and secret

go build -o elyoraa .
./elyoraa
```

The server listens on `PORT` (default `8080`) and exposes:

- `POST /webhook` — GitHub webhook endpoint.
- `GET /health` — liveness.
- `GET /metrics` — Prometheus-style metrics.

## Docker

```bash
cd api/deploy
cp .env.example .env
# edit .env

# Create the private-key secret file directory
mkdir -p secrets
# Place your GitHub App private key at the path you set in .env

docker compose up --build
```

## Requirements

- A GitHub App with the `pull_requests:read`, `pull_requests:write`, and repository contents permissions.
- The GitHub App private key.
- `devin` binary and a persistent `~/.config/devin` credentials volume.
- `git` and `bubblewrap`/`socat` in the container for the Devin sandbox.

## Project layout

```
api/
├── main.go                 # entrypoint
├── internal/
│   ├── config/             # env config
│   ├── github/             # GitHub App auth, PR fetch, review posting
│   ├── cache/              # bare-clone cache + worktrees
│   ├── dedup/              # in-flight job deduplication
│   ├── queue/              # in-memory (default) or BoltDB file-backed worker queue
│   ├── webhook/            # HTTP handler
│   ├── reviewer/           # Devin prompt/review orchestrator
│   └── metrics/            # Prometheus text metrics
├── prompts/review.md       # Devin prompt template
└── deploy/                 # Dockerfile, compose, askpass, skill
```

## Notes

- The `devin-config.json` is generated at startup to the path in `DEVIN_CONFIG_PATH`.
- Short-lived GitHub installation tokens are written to `ELYORAA_TOKEN_DIR` (default `/tmp/elyoraa`) and supplied to git via `GIT_ASKPASS`.
- Per-review `XDG_CACHE_HOME` and `TMPDIR` are isolated; `HOME` and `XDG_CONFIG_HOME` must point at the Devin credentials volume.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue.
