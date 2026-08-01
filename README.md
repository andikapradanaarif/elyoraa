# elyoraa

AI PR Reviewer — a Go engine that receives GitHub `pull_request` webhooks and reviews each PR with the Devin CLI in a sandboxed worktree.

- **Architecture:** see [`docs/plan/ARCHITECTURE.md`](docs/plan/ARCHITECTURE.md).
- **Implementation:** see [`api/README.md`](api/README.md).
- **Runbook:** copy `api/deploy/.env.example` to `api/deploy/.env`, fill in your GitHub App credentials, and run `docker compose up --build` from `api/deploy/`.

## Status

Phase 1 MVP is implemented under `api/`:

- In-memory review queue with worker panic recovery.
- Bare-clone cache + per-review worktrees.
- GitHub App auth, webhook verification, PR fetch, and review posting.
- Prompt rendering, Devin CLI runner, and JSON output parser.
- Prometheus-style `/metrics`, `/health`, and `/webhook` endpoints.

Phase 1.5/2 (file-backed/Redis queue and full observability) are not yet implemented.
