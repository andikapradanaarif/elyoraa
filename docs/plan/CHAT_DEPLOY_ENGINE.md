# Chat + Deploy Engine Plan

## Summary

Add a multi-turn chat web service to `elyoraa` so an authenticated user can ask Devin CLI (running on the same server) to deploy GitHub repositories or run general server tasks. Chat rooms may optionally be tied to a repo, allowing conversations like "set up SSL" that have no associated codebase. Chat rooms, messages, and deployments are persisted in Postgres; the chat job queue and real-time delivery use Redis. Each room has a configurable Devin permission mode, and the project includes a web UI.

## Goals

- Multi-turn chat with Devin CLI, optionally in the context of a checked-out GitHub repo.
- Per-room permission mode: `autonomous` (sandboxed), `bypass` (dangerous but explicit), or a fixed allow-list.
- Devin can run deployment commands (`docker compose`, `kubectl`, `helm`, etc.) inside the room workspace or general server commands when no repo is attached.
- Users must register and be verified (email/invite) before they can log in.
- The service serves a web UI for chat and room management.
- Postgres stores rooms, messages, users, and deployment state.
- Redis stores the chat job queue and publishes assistant responses for streaming clients.
- Reuse the existing `elyoraa` bare-clone cache and queue infrastructure.

## Non-goals (for this plan)

- Multi-user collaboration within a single room (each room belongs to one user, but a user can have unlimited rooms/sessions).
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
  │  └─ login (rejected if not verified), logout, current user
  │
  ├─ POST /auth/register     → creates user with verified=false
  ├─ POST /auth/login        → rejects if verified=false
  ├─ POST /auth/logout       → invalidates session
  ├─ GET  /auth/me           → current user
  │
  ├─ Admin (requires admin token / first user is admin)
  │  ├─ GET  /admin/users          → list pending users
  │  ├─ POST /admin/users/{id}/verify → set verified=true
  │  └─ POST /admin/users/{id}/reject  → delete user
  │
  ├─ POST /chat/rooms        → creates room, adds repo worktree (or empty workspace), returns room id
  ├─ GET  /chat/rooms        → list user's rooms
  ├─ GET  /chat/rooms/{id}   → room details including agent_status
  ├─ POST /chat/rooms/{id}/messages
  │                          → enqueues chat job, returns 202
  ├─ GET  /chat/rooms/{id}/messages
  │                          → returns conversation history (with per-message status)
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

| Option                     | Description                                                                                                                                                                                                                                  | Pros                                                                          | Cons                                                                                                    |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| **A. Conversation prompt** | Every user message starts a fresh `devin --prompt-file <file>` process; the prompt contains the full prior conversation and a persistent workspace path. `RunDevin` must set a writable `XDG_DATA_HOME` so the Devin CLI can write its logs. | Reliable, no ACP host, works headlessly, and matches the current review flow. | Each turn pays full context cost; prior tool results must be in the prompt.                             |
| **B. Resume sessions**     | First turn captures a Devin session ID; follow-ups use `devin -r <id> -p <prompt>`.                                                                                                                                                          | Native session memory.                                                        | CLI session ID capture and non-interactive follow-up prompts are not clearly documented; needs a spike. |
| **C. ACP over stdio**      | Spawn `devin acp` per room and speak JSON-RPC.                                                                                                                                                                                               | Proper multi-turn.                                                            | Heavy implementation, long-lived process per room, not designed for arbitrary web clients.              |

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
    verified BOOLEAN NOT NULL DEFAULT false, -- admin must verify before login
    verified_at TIMESTAMPTZ,
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
    agent_status TEXT NOT NULL DEFAULT 'idle',  -- idle, processing, error, done
    agent_error TEXT,                           -- last error message if agent_status=error
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
    status TEXT NOT NULL DEFAULT 'completed',  -- pending, processing, completed, error
    error TEXT,                                -- error details if status=error
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

`chat_rooms.workspace_path` is a persistent directory on disk where the repo worktree is added and kept across turns, or an empty general-purpose directory when there is no repo. If the room has an `owner`/`repo`, the bare clone must already exist at `WORKSPACE_BASE/{owner}/{repo}.git` (the bot does not clone on demand). If the room has no `owner`/`repo`, the workspace is an empty general-purpose directory (e.g. `WORKSPACE_BASE/chat/{room_id}`) so Devin can still run server-wide commands.

## New packages

- `internal/auth`
  - `session.go`: create, validate, and invalidate sessions.
  - `password.go` (optional): bcrypt hashing for local accounts.
  - `middleware.go`: require a valid session on protected routes.
- `internal/usecase/chat`
  - `job.go`: `ChatJob` type and in-memory/Redis queue abstraction.
  - `worker.go`: process a `ChatJob`, run Devin, store the response.
  - `prompt.go`: render a prompt from conversation history.
  - `queue.go`: Redis list queue (`LPUSH` / `BRPOP`) for chat jobs, backed by `internal/redis`.
  - `config.go`: per-room Devin permission/allow-list config generation.
- `internal/repository/chat`
  - `store.go`: `ChatStore` interface and Postgres queries for rooms, messages, and deployments.
- `internal/handler/chat`
  - `handler.go`: HTTP handlers, SSE streaming, and `GET /` UI redirect.
- `internal/handler/auth`
  - `handler.go`: `POST /auth/register`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`, optional GitHub OAuth routes.
- `internal/ui`
  - `static/` and `index.go`: serve the chat web UI (`//go:embed`) from `/`.
- `internal/repository/queue/redis.go` (optional): Redis implementation of `queue.Backend` for the review queue, if the existing memory/file backends are not enough.
- `internal/redis`: shared Redis client wrapper for the chat queue and pub/sub.
- `migration/migrations/002_chat.up.sql`: versioned SQL migration for users, sessions, chat_rooms, chat_messages, deployments.

## Updated packages

- `internal/config/config.go`: add `DATABASE_URL` (already present), `REDIS_URL`, `CHAT_QUEUE_BACKEND`, `CHAT_MAX_CONCURRENT`, `CHAT_ENABLED`, `CHAT_SESSION_STORE`, session cookie config (`SESSION_COOKIE_NAME`, `SESSION_MAX_AGE`), local auth config (`AUTH_LOCAL_ENABLED`), OAuth config (`GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`, `GITHUB_OAUTH_CALLBACK_URL`), and Devin environment paths (`DEVIN_CREDS_DIR`, `DEVIN_DATA_DIR`) so `RunDevin` can point `XDG_CONFIG_HOME`/`XDG_DATA_HOME` correctly.
- `internal/repository/cache/cache.go`: add `AddChatWorkspace(ctx, owner, repo, branch, sha, roomID string) (string, func(), error)`. If `owner`/`repo` are empty, it returns an empty `WORKSPACE_BASE/chat/{roomID}` directory. If a repo is given, it adds a worktree from the pre-cloned bare repo at `WORKSPACE_BASE/{owner}/{repo}.git`; if the bare repo does not exist, it returns an error. The `cleanup` is a no-op for persistent chat rooms.
- `internal/usecase/review.go`: extract a shared `RunDevin(ctx, workspace, permissionMode, promptPath, cfg)` helper that can be parameterized and that prepares a writable `XDG_DATA_HOME` plus a credentials-visible `XDG_CONFIG_HOME`.
- `cmd/server/main.go`: initialize Postgres, Redis, chat store, chat worker pool, and register auth + chat routes.
- `internal/metrics/metrics.go`: add chat counters (rooms, messages, deployments, chat errors).
- `api/deploy/docker-compose.yml`: add a `chat-workspace` volume and no extra `postgres`/`redis` services (shared services are used in production; local dev compose may include them separately).
- `api/.env.example` and `api/deploy/.env.example`: add the new env vars.
- `api/go.mod`: add `github.com/redis/go-redis/v9` and optionally `github.com/gorilla/websocket` (`pgx/v5` is already present).

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
  - Creates a room. If `owner` and `repo` are present, the room expects a pre-cloned bare repo at `WORKSPACE_BASE/{owner}/{repo}.git` and adds a worktree at `WORKSPACE_BASE/chat/{room_id}`; the request fails with a clear error if the bare clone is missing. Otherwise the workspace is an empty general-purpose directory.

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
   - If `owner`/`repo` are set, add a worktree from the pre-cloned bare repo via `repository/cache`. If the bare clone at `WORKSPACE_BASE/{owner}/{repo}.git` does not exist, fail the message with a clear error.
   - Otherwise, create an empty directory at `WORKSPACE_BASE/chat/{room_id}`.
3. Load all messages for the room.
4. Render a prompt file containing:
   - system instructions for the chosen permission mode,
   - the full conversation history,
   - the new user message,
   - an explicit output section telling Devin to respond with the final answer.
5. Prepare a writable per-message `XDG_DATA_HOME` and point `XDG_CONFIG_HOME`/`HOME` at the mounted Devin credentials volume so `devin` can write its rolling logs and find `credentials.toml`. If credentials live in the data tree, copy `credentials.toml` from the host data mount into the per-message data directory. Run Devin:
   - `autonomous`: `devin --sandbox --permission-mode autonomous --config <room-config> --prompt-file <prompt>`
   - `bypass`: `devin --permission-mode bypass --config <room-config> --prompt-file <prompt>`
6. Capture `CombinedOutput`, extract the final text response.
7. Insert the assistant message into `chat_messages`.
8. If the message triggers a tracked deployment, update the `deployments` table.
9. Publish the response to the Redis channel.

## Permission modes and Devin config

The per-room permission mode is stored in `chat_rooms.permission_mode` and used to generate a fresh Devin config before each run.

### `autonomous` (default, recommended)

Use `devin --sandbox --permission-mode autonomous` with an allow-list tuned for deployment inspection and safe commands. Wildcards reduce churn when Devin picks specific files or flags. Example:

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
			"Exec(docker compose *)",
			"Exec(kubectl get *)",
			"Exec(kubectl apply --dry-run=client -f *)",
			"Exec(helm list *)",
			"Exec(helm upgrade --install *)"
		],
		"deny": ["edit", "Write(**)", "Exec(rm -rf *)", "Exec(curl)", "Exec(wget)", "Fetch(**)"]
	}
}
```

`kubectl apply` is constrained to `--dry-run=client` by default. A future `bypass` or explicit `apply-dry-run=false` setting can permit real mutations when the user accepts the risk.

### `bypass` (explicit opt-in only)

Use `devin --permission-mode bypass` with no deny list. This allows arbitrary shell commands as the service user. A room must be created with `permission_mode: bypass` and the caller must explicitly accept the risk. All `bypass` runs are logged with room ID, user, prompt, and output.

### `normal` (fixed allow-list)

Same as `autonomous` but not sandboxed and without `bypass`. Useful if `bwrap` is unavailable, but prompts that cannot be answered headlessly will fail cleanly.

## Redis usage

- **Queue:** `elyoraa:chat:jobs` (Redis list). `LPUSH` to submit, `BRPOP` in the worker.
- **Pub/Sub:** `elyoraa:chat:msg:{message_id}`. The worker publishes the final assistant response; the SSE handler subscribes and streams it.

`internal/usecase/chat/queue.go` implements the chat queue on top of `internal/redis`. The existing `queue.Backend` in `internal/repository/queue` remains for review jobs; a Redis-backed `queue.Backend` can be added later if the review queue also needs to be distributed.

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
    volumes:
      - chat-workspace:/var/lib/elyoraa/workspace/chat
      # Existing devin and repo mounts from the base compose remain.

volumes:
  chat-workspace:
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

# Devin environment (point at the bind-mounted credentials and writable data temp)
DEVIN_CREDS_DIR=/home/elyoraa/.config/devin
DEVIN_DATA_DIR=/home/elyoraa/.local/share/devin
```

### Optional local dev compose

For local development without a shared Postgres/Redis, a separate `api/deploy/docker-compose.dev.yml` can include `postgres` and `redis` services.

## Implementation phases

### Phase 0: Spike

Before implementation, confirm the riskiest parts:

1. Does `devin --prompt-file <file>` exit cleanly and return the full response in stdout in a non-TTY container when `XDG_DATA_HOME` is writable and `HOME`/`XDG_CONFIG_HOME` point at the credentials volume?
2. Does `devin -r <id> --prompt-file <file>` work for follow-up turns?
3. Can `devin acp` be driven with a minimal JSON-RPC client for multi-turn chat?
4. Postgres and Redis connectivity from inside the `elyoraa` container.

### Phase 1: MVP

1. Add `migration/migrations/002_chat.up.sql` and `.down.sql` for users, sessions, chat_rooms, chat_messages, deployments.
2. Add `internal/repository/chat`: `ChatStore` interface and Postgres queries.
3. Add `internal/usecase/chat`: `ChatJob`, worker, prompt builder, Redis queue, and permission-mode Devin config generation.
4. Add `internal/handler/chat`: room, message, and SSE HTTP handlers.
5. Add `internal/auth`: session management, password hashing, and middleware. Add `internal/handler/auth`: `/auth/*` handlers.
6. Add `internal/redis` client wrapper and (optionally) `internal/repository/queue/redis.go` for the review queue.
7. Add `internal/ui`: embedded static chat UI served at `/`.
8. Update `internal/config`, `cmd/server/main.go`, and `internal/metrics` to wire Redis, chat worker, auth, and UI.
9. Add `AddChatWorkspace` to `internal/repository/cache/cache.go` and update `internal/usecase/review.go` with a shared `RunDevin` helper that prepares a writable `XDG_DATA_HOME` and a credentials-visible `XDG_CONFIG_HOME`.
10. Update `api/deploy/docker-compose.yml` (add `chat-workspace` volume, no extra Postgres/Redis) and both `.env.example` files.
11. Add tests for auth, prompt rendering, chat store, Redis queue, and HTTP handlers.

### Phase 2: Enhancements

- WebSocket endpoint in addition to SSE, with richer UI state.
- Swap to native Devin session resume or ACP if the spike succeeds.
- Deployment tracking (`deployments` table fully populated).
- Audit logging for `bypass` rooms.

### Phase 3: Scale

- Horizontal scaling: multiple `elyoraa` workers consuming the same Redis queue.
- Move room workspace tracking to Postgres so any instance can resume a room.
- Backfill down migrations for all chat tables.

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
3. `POST /chat/rooms` with a test repo. Ensure the bare clone exists at `WORKSPACE_BASE/{owner}/{repo}.git` first.
4. `POST /chat/rooms/{id}/messages` with "show me the docker-compose file".
5. Wait for the assistant reply via `GET /chat/rooms/{id}/messages`.
6. `POST` "run docker compose up -d" and verify the deployment in the workspace.
7. `POST /chat/rooms` without `owner`/`repo` and ask "set up SSL"; verify Devin runs in the empty workspace.

## Open questions

1. **Multi-turn backend:** Does the conversation-prompt MVP meet latency/cost targets, or should we immediately spike `devin -r` or `devin acp`?
2. **Auth:** Local username/password with session cookies, GitHub OAuth, or both for the MVP?
3. **Deployment tool allow-list:** Should Devin be allowed to run `docker`, `kubectl`, `helm`, and/or custom deploy scripts like `deploy-ghcr.sh`?
4. **Workspace lifecycle:** How long should a closed room's workspace be retained before the sweeper deletes it?
5. **UI framework:** Plain HTML/JS in `internal/ui/static/`, or a bundled frontend (e.g. React/Vue) served as static files?
