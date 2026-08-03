# Chat + Deploy Engine Plan

## Summary

Add a multi-turn chat web service to `elyoraa` so an authenticated user can ask Devin CLI (running on the same server) to deploy GitHub repositories or run general server tasks. Chat rooms may optionally be tied to a repo, allowing conversations like "set up SSL" that have no associated codebase. Chat rooms, messages, and deployments are persisted in Postgres; the chat job queue and real-time delivery use Redis. Each room has a configurable Devin permission mode, and the project includes a web UI.

## Goals

- Multi-turn chat with Devin CLI, optionally in the context of a checked-out GitHub repo.
- Per-room permission mode: `autonomous` (sandboxed), `bypass` (dangerous but explicit), or a fixed allow-list.
- Devin can run deployment commands (`docker compose`, `kubectl`, `helm`, etc.) inside the room workspace or general server commands when no repo is attached.
- Users must log in before creating or joining rooms.
- The service serves a web UI for chat and room management.
- Postgres stores rooms, messages, users, and deployment state.
- Redis stores the chat job queue and publishes assistant responses for streaming clients.
- Reuse the existing `elyoraa` bare-clone cache and queue infrastructure.

## Non-goals (for this plan)

- Support for multiple concurrent users per room (one human user per room is fine for MVP).
- A dedicated mobile UI in the MVP.
- Replacing the PR review flow; the chat engine is an additional path.

## Background

`elyoraa` already:

- Spawns `devin` non-interactively from Go (`internal/usecase/review.go`).
- Has a `queue.Backend` interface with in-memory and BoltDB backends in `internal/repository/queue`.
- Maintains pre-cloned bare repos in `internal/repository/cache` and creates per-PR worktrees.
- Generates a locked Devin config in `DEVIN_CONFIG_DIR` to deny dangerous tools in review mode.
- Uses a layered architecture: `cmd/server`, `internal/handler`, `internal/usecase`, `internal/repository`, and `migration`.

The new chat engine builds on these pieces but needs a different permission model (to actually run deployments), an auth layer, a web UI, and a persistence layer for conversation state.

## Architecture

```
User/Client
  │
  ▼
Web UI or HTTP API
  │
  ▼
elyoraa API
  │
  ├─ Auth middleware (session / token)
  │  └─ login, logout, current user
  │
  ├─ POST /chat/rooms        → creates room, clones repo, returns room id
  ├─ POST /chat/rooms/{id}/messages
  │                          → enqueues chat job, returns 202
  ├─ GET  /chat/rooms/{id}/messages
  │                          → returns conversation history
  └─ GET  /chat/rooms/{id}/messages/{msg}/stream
                             → SSE stream of assistant response
  │
  ▼
Redis (chat queue + pub/sub)
  │
  ▼
chat worker
  │
  ├─ loads room + message history from Postgres
  ├─ prepares persistent workspace via cache.Cache
  ├─ renders prompt with conversation history
  ├─ runs Devin CLI in workspace with room permission mode
  ├─ parses response, stores assistant message in Postgres
  └─ publishes final response to Redis
  │
  ▼
Postgres (users, sessions, rooms, messages, deployments)
```

## Multi-turn strategy

Devin CLI is a terminal tool. Three options were considered for multi-turn chat:

| Option                     | Description                                                                                                                                                           | Pros                                     | Cons                                                                                                    |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| **A. Conversation prompt** | Every user message starts a fresh `devin -p` (or `devin --prompt-file`) process, but the prompt contains the full prior conversation and a persistent workspace path. | Reliable, no ACP host, works headlessly. | Each turn pays full context cost; prior tool results must be in the prompt.                             |
| **B. Resume sessions**     | First turn captures a Devin session ID; follow-ups use `devin -r <id> -p <prompt>`.                                                                                   | Native session memory.                   | CLI session ID capture and non-interactive follow-up prompts are not clearly documented; needs a spike. |
| **C. ACP over stdio**      | Spawn `devin acp` per room and speak JSON-RPC.                                                                                                                        | Proper multi-turn.                       | Heavy implementation, long-lived process per room, not designed for arbitrary web clients.              |

**Recommended approach for the MVP: A (conversation prompt).** It is the only option guaranteed to work in a headless service today. Run a parallel spike for B and C; if either works cleanly, swap the backend in a later phase.

## Database schema (Postgres)

```sql
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL UNIQUE,
    email TEXT UNIQUE,
    password_hash TEXT, -- or null if using OAuth
    provider TEXT,      -- e.g. 'local', 'github'
    external_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chat_rooms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    owner TEXT,            -- optional GitHub owner
    repo TEXT,             -- optional GitHub repo
    branch TEXT,           -- optional
    sha TEXT,              -- optional
    permission_mode TEXT NOT NULL DEFAULT 'autonomous',
    workspace_path TEXT NOT NULL UNIQUE,
    devin_session_id TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chat_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    room_id UUID NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    role TEXT NOT NULL CHECK (role IN ('user','assistant','system')),
    content TEXT NOT NULL,
    tool_calls JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    room_id UUID NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'running',
    logs TEXT,
    deployed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`chat_rooms.workspace_path` is a persistent directory on disk where the repo is checked out and kept across turns. If the room has no `owner`/`repo`, the workspace is an empty general-purpose directory (e.g. `WORKSPACE_BASE/chat/{room_id}`) so Devin can still run server-wide commands.

## New packages

- `internal/auth`
  - `handler.go`: `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`.
  - `session.go`: create, validate, and invalidate sessions.
  - `store.go`: user and session database queries.
  - `middleware.go`: require a valid session on protected routes.
- `internal/chat`
  - `room.go`: room CRUD and message history.
  - `job.go`: `ChatJob` definition and submission.
  - `worker.go`: process a `ChatJob`, run Devin, store the response.
  - `handler.go`: HTTP handlers and SSE streaming.
  - `prompt.go`: render a prompt from conversation history.
  - `store.go`: Postgres queries.
  - `config.go`: per-room Devin permission/allow-list config generation.
- `internal/ui`
  - `static/` and `index.go`: serve the chat web UI (HTML/CSS/JS) and embed assets.
- `internal/queue/redis.go`: Redis implementation of `queue.Backend`.
- `internal/redis`: shared Redis client wrapper.
- `internal/migrate` (optional): simple Go-based Postgres migration runner.

## Updated packages

- `internal/config/config.go`: add `DATABASE_URL`, `REDIS_URL`, `CHAT_QUEUE_BACKEND`, `CHAT_MAX_CONCURRENT`, `CHAT_ENABLED`, session cookie config (`SESSION_COOKIE_NAME`, `SESSION_MAX_AGE`), local auth config (`AUTH_LOCAL_ENABLED`), and OAuth config (`GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`, `GITHUB_OAUTH_CALLBACK_URL`).
- `internal/cache/cache.go`: add `AddPersistentWorkspace(ctx, owner, repo, branch, sha, roomID string) (string, func(), error)`. If `owner`/`repo` are empty, it returns an empty `WORKSPACE_BASE/chat/{roomID}` directory and skips the git clone. The `cleanup` is a no-op for persistent chat rooms.
- `internal/reviewer/reviewer.go`: extract a shared `RunDevin` helper that can be parameterized by permission mode and prompt file.
- `main.go`: initialize Postgres, Redis, chat store, chat worker, register routes.
- `internal/metrics/metrics.go`: add chat counters (rooms, messages, deployments, chat errors).
- `api/deploy/docker-compose.yml`: add `postgres` and `redis` services and a `chat-workspace` volume.
- `api/deploy/.env.example` and `api/.env.example`: add the new env vars.
- `api/go.mod`: add `github.com/jackc/pgx/v5` (or `lib/pq`), `github.com/redis/go-redis/v9`, and optionally `github.com/gorilla/websocket`.

## HTTP API

All JSON API endpoints require authentication via session cookie or bearer token.

### Auth

- `POST /auth/register` (optional for MVP)
  - Body: `{ "username": "...", "password": "..." }`
  - Creates a local user.

- `POST /auth/login`
  - Body: `{ "username": "...", "password": "..." }`
  - Returns a session cookie and optionally a bearer token.

- `POST /auth/logout`
  - Invalidates the current session.

- `GET /auth/me`
  - Returns the current user.

- `GET /auth/github` / `GET /auth/github/callback` (optional)
  - OAuth login flow using the GitHub App credentials.

### Chat

- `GET /`
  - Returns the chat web UI (`internal/ui` static assets).

- `POST /chat/rooms`
  - Body: `{ "name": "...", "owner": "..." (optional), "repo": "..." (optional), "branch": "..." (optional), "sha": "..." (optional), "permission_mode": "autonomous" }`
  - Creates a room. If `owner` and `repo` are present, the repo is cloned to `WORKSPACE_BASE/chat/{room_id}`. Otherwise the workspace is an empty general-purpose directory.

- `POST /chat/rooms/{id}/messages`
  - Body: `{ "content": "set up SSL" }`
  - Enqueues a `ChatJob` for the room. Returns `202 Accepted` with the new `message_id` and status URL.

- `GET /chat/rooms`
  - Returns the current user's rooms.

- `GET /chat/rooms/{id}/messages`
  - Returns the full conversation history.

- `GET /chat/rooms/{id}/messages/{message_id}/stream`
  - Server-Sent Events. Subscribes to the Redis channel for that message and streams the assistant response when it is ready.

- `DELETE /chat/rooms/{id}`
  - Marks the room closed. The workspace is deleted by the sweeper after a TTL.

## Worker flow per message

1. Load the room from Postgres.
2. Ensure the persistent workspace exists:
   - If `owner`/`repo` are set, clone/fetch via `cache.Cache`.
   - Otherwise, create an empty directory at `WORKSPACE_BASE/chat/{room_id}`.
3. Load all messages for the room.
4. Render a prompt file containing:
   - system instructions for the chosen permission mode,
   - the full conversation history,
   - the new user message,
   - an explicit output section telling Devin to respond with the final answer.
5. Run Devin:
   - `autonomous`: `devin --sandbox --permission-mode autonomous --config <room-config> --prompt-file <prompt>`
   - `bypass`: `devin --permission-mode bypass --config <room-config> --prompt-file <prompt>`
6. Capture `CombinedOutput`, extract the final text response.
7. Insert the assistant message into `chat_messages`.
8. If the message triggers a tracked deployment, update the `deployments` table.
9. Publish the response to the Redis channel.

## Permission modes and Devin config

The per-room permission mode is stored in `chat_rooms.permission_mode` and used to generate a fresh Devin config before each run.

### `autonomous` (default, recommended)

Use `devin --sandbox --permission-mode autonomous` with an allow-list tuned for deployment inspection and safe commands. Example:

```json
{
	"permissions": {
		"allow": [
			"read",
			"grep",
			"glob",
			"Exec(git status)",
			"Exec(git log)",
			"Exec(git diff)",
			"Exec(docker compose ps)",
			"Exec(docker compose build)",
			"Exec(docker compose up -d)",
			"Exec(kubectl get)",
			"Exec(kubectl apply --dry-run=client -f *)",
			"Exec(helm list)",
			"Exec(helm upgrade --install *)"
		],
		"deny": ["edit", "Write(**)", "Exec(rm -rf /)", "Exec(curl)", "Exec(wget)", "Fetch(**)"]
	}
}
```

### `bypass` (explicit opt-in only)

Use `devin --permission-mode bypass` with no deny list. This allows arbitrary shell commands as the service user. A room must be created with `permission_mode: bypass` and the caller must explicitly accept the risk. All `bypass` runs are logged with room ID, user, prompt, and output.

### `normal` (fixed allow-list)

Same as `autonomous` but not sandboxed and without `bypass`. Useful if `bwrap` is unavailable, but prompts that cannot be answered headlessly will fail cleanly.

## Redis usage

- **Queue:** `elyoraa:chat:jobs` (Redis list). `LPUSH` to submit, `BRPOP` in the worker.
- **Pub/Sub:** `elyoraa:chat:msg:{message_id}`. The worker publishes the final assistant response; the SSE handler subscribes and streams it.

`internal/queue/redis.go` implements `queue.Backend` using `go-redis` and is parameterized by the queue key, so it can also back the review queue if desired.

## Docker / deployment changes

The server already has Postgres and Redis running for other apps. `elyoraa` should connect to those shared services rather than running its own containers. This keeps `docker-compose.yml` minimal and avoids duplicate infrastructure.

### Production `api/deploy/docker-compose.yml`

```yaml
services:
  elyoraa:
    ...
    environment:
      DATABASE_URL: ${DATABASE_URL}
      REDIS_URL: ${REDIS_URL}
      CHAT_ENABLED: "true"
      CHAT_QUEUE_BACKEND: redis
      CHAT_MAX_CONCURRENT: "2"
      SESSION_COOKIE_NAME: "elyoraa_session"
      SESSION_MAX_AGE: "168h"
```

### Database isolation

Because other apps share the same Postgres instance:

- **Preferred:** create a dedicated `elyoraa` database on the existing server and point `DATABASE_URL` to it.
- **Alternative:** use a Postgres schema (e.g. `elyoraa`) and prefix table names, then set `search_path` in `DATABASE_URL`.

### Redis key prefixes

All Redis keys are prefixed with `elyoraa:` to avoid collisions:

- Queue: `elyoraa:chat:jobs`
- Pub/Sub: `elyoraa:chat:msg:{message_id}`
- Sessions (if stored in Redis): `elyoraa:session:{token}`

### Example `.env` for production using shared services

```bash
# Existing shared Postgres (create an 'elyoraa' database first)
DATABASE_URL=postgres://elyoraa:your-password@your-postgres-host:5432/elyoraa?sslmode=disable

# Existing shared Redis
REDIS_URL=redis://your-redis-host:6379/0

# Chat
CHAT_ENABLED=true
CHAT_QUEUE_BACKEND=redis
CHAT_MAX_CONCURRENT=2
SESSION_COOKIE_NAME=elyoraa_session
SESSION_MAX_AGE=168h
```

### Optional local dev compose

For local development without a shared Postgres/Redis, a separate `api/deploy/docker-compose.dev.yml` can include `postgres` and `redis` services.

## Implementation phases

### Phase 0: Spike

Before implementation, confirm the riskiest parts:

1. Does `devin -p --prompt-file <file>` exit cleanly and return the full response in stdout in a non-TTY container?
2. Does `devin -r <id> -p <prompt>` work for follow-up turns?
3. Can `devin acp` be driven with a minimal JSON-RPC client for multi-turn chat?
4. Postgres and Redis connectivity from inside the `elyoraa` container.

### Phase 1: MVP

1. Add `internal/migrate` with the schema migrations (users, sessions, chat_rooms, chat_messages, deployments).
2. Add `internal/auth` package: registration, login, session middleware, and `/auth/*` handlers.
3. Add `internal/chat/store` with room, message, and deployment queries.
4. Add `internal/redis` client and `internal/queue/redis.go`.
5. Add `internal/chat` types, prompt builder, worker, and HTTP handlers.
6. Add `internal/ui` with a minimal static chat UI served at `/`.
7. Update `internal/config`, `main.go`, `metrics` to wire chat, auth, and UI.
8. Add persistent workspace support (with optional repo) to `cache.Cache`.
9. Update Docker Compose and `.env.example`.
10. Add tests for auth, prompt rendering, store, Redis queue, and handler.

### Phase 2: Enhancements

- WebSocket endpoint in addition to SSE.
- Swap to native Devin session resume or ACP if the spike succeeds.
- Add WebSocket support and richer UI state.
- Deployment tracking (`deployments` table fully populated).
- Audit logging for `bypass` rooms.

### Phase 3: Scale

- Horizontal scaling: multiple `elyoraa` workers consuming the same Redis queue.
- Move room workspace tracking to Postgres so any instance can resume a room.
- Add Postgres migration runner.

## Verification

After each phase run:

```bash
cd api
go build ./...
go vet ./...
go test ./...
```

Manual end-to-end in Docker Compose:

1. `POST /auth/register` and `POST /auth/login` to get a session.
2. Open `/` and verify the chat UI loads.
3. `POST /chat/rooms` with a test repo.
4. `POST /chat/rooms/{id}/messages` with "show me the docker-compose file".
5. Wait for the assistant reply via `GET /chat/rooms/{id}/messages`.
6. `POST` "run docker compose up -d" and verify the deployment in the workspace.
7. `POST /chat/rooms` without `owner`/`repo` and ask "set up SSL"; verify Devin runs in the empty workspace.

## Open questions

1. **Multi-turn backend:** Should the MVP use the conversation-prompt approach, or do we pause to validate `devin -r`/ACP first?
2. **Auth:** Local username/password with session cookies, GitHub OAuth, or both?
3. **Deployment tool allow-list:** Should Devin be allowed to run `docker`, `kubectl`, `helm`, and/or custom deploy scripts like `deploy-ghcr.sh`?
4. **Real-time delivery:** SSE in the MVP, or do we need WebSocket from day one?
5. **Workspace lifecycle:** How long should a closed room's workspace be retained before the sweeper deletes it?
6. **UI framework:** Plain HTML/JS in `internal/ui/static/`, or a bundled frontend (e.g. React/Vue) served as static files?
