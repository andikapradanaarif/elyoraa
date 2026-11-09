# elyoraa

AI PR Reviewer — a Go engine that receives GitHub `pull_request` webhooks and reviews each PR with the Crush CLI using bounded repository tools.

- **Architecture:** see [`docs/plan/GIT_AI_REVIEWER.md`](docs/plan/GIT_AI_REVIEWER.md).
- **Implementation:** see [`api/README.md`](api/README.md).
- **Runbook:** copy `api/deploy/.env.example` to `api/deploy/.env`, fill in your GitHub App credentials, and run `docker compose up --build` from `api/deploy/`.

## Status

Phase 1 MVP is implemented under `api/`:

- In-memory review queue with worker panic recovery.
- Bare-clone cache + per-review worktrees.
- GitHub App auth, webhook verification, PR fetch, and review posting.
- Prompt rendering, Crush CLI runner, and JSON output parser.
- Prometheus-style `/metrics`, `/health`, and `/webhook` endpoints; `GET /jobs` lists job outcomes and `POST /jobs/retry/{key}` re-enqueues a failed review.
- Optional PostgreSQL webhook audit log.
- Optional BoltDB-backed queue (`QUEUE_BACKEND=file`).
- Layered `handler > usecase > repository` architecture.
