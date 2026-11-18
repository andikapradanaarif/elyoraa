# elyoraa — agent notes

## Project layout

- Go module lives in `api/`: `github.com/andikapradanaarif/elyoraa`.
- Architecture and plan docs live in `docs/plan/` (`GIT_AI_REVIEWER.md` — historical Devin-era design; `MENTION_ROUTER.md` — current mention-router/gate design).
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

The Debian image builds the user's [Crush fork](https://github.com/andikapradanaarif/crush) at pinned revision `759066acc9c0fd62dcb74bc8473e171dee9c11d2` using a separate Go 1.27.4 stage (`CRUSH_REPO` / `CRUSH_REF` build args). Compose mounts `~/.config/crush` and `~/.local/share/crush`. No bubblewrap/socat or elevated sandbox capabilities are needed.

## Local development

`api/.env` is loaded automatically by `internal/config.Load` (valo-style), so `make dev` works as soon as you fill in `.env`. Set `ENV_FILE` to use a different env file.

`make dev` fails fast if `git`, `crush`, the GitHub App private key, or `WORKSPACE_BASE` are missing or misconfigured.

## Notes

- The server needs `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, and a configured system `crush` binary (`CRUSH_BIN` overrides it).
- `CRUSH_CONFIG_DIR` and `CRUSH_AUTH_DATA_DIR` point to the trusted global Crush config and auth-data directories (defaults follow Crush XDG/CRUSH_GLOBAL_* settings). JSON and `crushrc` are loaded normally. Per-job configuration and session state live in a temporary directory.
- `PROMPTS_DIR` (default `/app/prompts`) holds the review prompt templates.
- `GIT_ASKPASS_PATH` (default `/app/git-askpass.sh`) is the credential helper for server-side git operations. Crush never receives GitHub installation tokens: the `mcp_elyoraa_sync` tool mints a token in Go and fetches only the job's registered repository.
- mem0 is configured as a remote HTTP MCP server (`https://mcp.mem0.ai/mcp`) when `MEM0_API_KEY` is set. No Node.js required.
- The Docker image includes a `HEALTHCHECK` that hits `GET /health`.
- Set `DATABASE_URL` (optional) to enable the PostgreSQL webhook audit log.
- Set `QUEUE_BACKEND=file` and `QUEUE_FILE_PATH` to use the BoltDB-backed queue. Job outcomes (status, error, attempts, payload) are persisted in the same file's `outcomes` bucket. Claimed jobs move to an `inflight` bucket rather than being deleted — a process killed mid-job leaves the row there and `OpenBoltDB` moves it back to `jobs`, so killed jobs re-run on the next boot. `QUEUE_BACKEND=postgres` (requires `DATABASE_URL`) stores queue, records, and dedup claims in Postgres — the only mode safe for multiple replicas: workers claim via `FOR UPDATE SKIP LOCKED` with a `QUEUE_JOB_LEASE` (default `REVIEW_TIMEOUT`+5m) that re-queues a dead worker's job, and supersession propagates through `job_claims.superseded` to the owning replica's watcher. Claims with no queue row older than the lease are expired as orphaned so a crashed replica's claim can't block re-mentions forever.
- `GET /jobs` lists tracked jobs (`?status=failed` filters); `GET /jobs/{key}` shows one; `POST /jobs/retry/{key}` re-enqueues a job without a new webhook. Key formats: `owner/repo/pr/headSHA` for review/split/issue jobs, `respond/owner/repo/number/triggerID` for mention-router jobs.
- Mention routing: `MENTION_ROUTER=keywords` (default) keeps deterministic routing; `=agent` turns every mention into a `respond` job — a Crush run reads the merged discussion (issue comments + reviews + inline comments, ~80 entries/64KB) and declares bounded actions (`request_review`, `request_split` with optional `create`, `request_issue_analysis`, `create_issues`, or reply-only) in a dedicated ` ```elyoraa-actions ` fence that `ReviewUsecase` validates and executes. Slash commands (`/review`, `/split`, `/accept`) bypass the agent entirely. See `docs/plan/MENTION_ROUTER.md`.
- Action authorization runs in Go, not the prompt: `request_review`/`request_split`/`request_issue_analysis` require the PR author or a COLLABORATOR+ association; `create_issues` requires COLLABORATOR+. `create_issues` also requires an outstanding proposal (a completed `split` job without `SplitCreate`, or a visible `create_issues` proposal in the discussion). Trigger identity rides on `Review.TriggerAuthor`/`TriggerAssociation`.
- Size-gate triage: `SIZE_GATE_MODE=deterministic` (default) splits oversize PRs directly; `=agent` nominates in bands — ≤1× budget reviews, 1×–3× runs a `gate` job whose Crush run judges review-vs-split from the file manifest + worktree, >3× or a truncated file list still splits directly. Malformed/failed verdicts fall back to split. `GATE_TIMEOUT` bounds only the triage pass; the gate job itself gets `REVIEW_TIMEOUT` so a `gate→review` transition isn't starved.
- Per-type Crush config: `RESPOND_MODEL`/`RESPOND_TIMEOUT` (default `REVIEW_TIMEOUT`)/`RESPOND_MAX_ACTIONS`, `GATE_MODEL`/`GATE_TIMEOUT` — models default to `CRUSH_MODEL`.
- Crush invocations retry transient failures: `CRUSH_MAX_RETRIES` (default 3, i.e. 4 attempts), `CRUSH_RETRY_BACKOFFS` (default `30s,2m,5m`), `CRUSH_ATTEMPT_TIMEOUT` (default 0 = `REVIEW_TIMEOUT`), `CRUSH_PREFLIGHT_CHECK` (default true, runs `crush --version` before each attempt).
- Timeout env vars: `REVIEW_TIMEOUT` (default 30m — bounds the whole job incl. retries + reconcile, so keep it >= `PR_REVIEW_BUDGET_MINUTES`; a startup warning fires when it isn't), `COMMENT_TIMEOUT` (default 60s), `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT` (Go duration syntax, e.g. `10m`, `30s`).
- PR size gate: `PR_SIZE_GATE` (default true), `PR_REVIEW_BUDGET_MINUTES` (default 15), `PR_LINES_PER_MINUTE` (default 100). See Review flow.

## Security / ops

- Crush runs from a trusted private job directory so repository-provided JSON/crushrc cannot execute. Its generated project policy disables native tools and guards tool calls to allow only elyoraa repository MCP operations and mem0. The Go tools enforce read-only Git argv, rooted file access, secret-path filtering, output bounds, and server-side credentialed refreshes. User global Crush configuration/hooks remain trusted code. GitHub writes still pass through `redactSecrets`.
- `ADMIN_TOKEN` gates `GET /jobs`, `GET /jobs/{key}`, `POST /jobs/retry/{key}` and the metrics route via `Authorization: Bearer`. Unset → those routes return 404.
- `MENTION_RATE_LIMIT` (default 20) / `MENTION_RATE_WINDOW` (default 1h) rate-limit mention-triggered jobs per author+repo (auto-reviews exempt). `DAILY_JOB_CAP_PER_REPO` (default 0 = off) bounds jobs started per repo per UTC day. `ELYORAA_PAUSED=true` acknowledges and drops all webhook events while `/health` stays up.
- `WEBHOOK_MAX_BYTES` (default 5 MiB) caps webhook bodies via `http.MaxBytesReader`. `AUDIT_RETENTION_DAYS` (default 30) prunes the webhook audit table daily (requires `DATABASE_URL`).
- Shutdown: `srv.Shutdown` stops new requests, then `q.Stop` rejects new submissions and waits up to `SHUTDOWN_DRAIN_TIMEOUT` (default `REVIEW_TIMEOUT`) for in-flight jobs before cancelling. Compose sets `stop_grace_period: 35m` — keep it ≥ your drain timeout — and `restart: unless-stopped` so an OOM kill (SIGKILL, no drain) doesn't leave the bot dead. At startup `ReconcileStuckJobs` fails records left in `processing`/`queued` with no surviving queue row and edits their pending comments (retry via `POST /jobs/retry/{key}`); records whose row persisted re-run on their own.
- Production: `QUEUE_BACKEND=file` + `QUEUE_FILE_PATH=/var/lib/elyoraa/queue/queue.db` (named volume in compose) survives restarts — queued AND in-flight jobs re-run on boot; `memory` loses everything, including the records `ReconcileStuckJobs` needs to fix stale "in progress" comments.
- **Replicas**: `file`/`memory` backends are single-instance only (dedup in-memory, BoltDB single-writer). With `QUEUE_BACKEND=postgres`, run N replicas — same image, shared DATABASE_URL. Each webhook still lands on one replica, but claim transactions make double-processing impossible; a replica's crashed jobs re-queue via lease expiry.
- Deploys push `ghcr.io/<owner>/elyoraa:<sha>` plus `:latest`; set `ELYORAA_IMAGE` to the SHA tag and `docker compose pull && docker compose up -d`. Rollback = redeploy the previous SHA. CI runs build/vet/test + golangci-lint (new issues only) + govulncheck (advisory); Dependabot enforces a 7-day minimum release age.

## Review flow

- Repositories must be registered manually — `WORKSPACE_BASE` is the registry. Run `git clone --bare https://github.com/{owner}/{repo}.git WORKSPACE_BASE/{owner}/{repo}.git` on the server (in Docker, clone into `WORKSPACE_HOST_PATH` on the host). A plain clone suffices: every `Fetch` self-repairs the two required refspecs (`+refs/heads/*:refs/heads/*` and `+refs/pull/*/head:refs/remotes/origin/pr/*`) before running `fetch --prune` with the installation token via `GIT_ASKPASS`, so branches and PR heads both stay current. Fetches are serialized per clone (`lockFetch`) and ref-lock races (`cannot lock ref … is at X but expected Y`) retry with backoff (250ms/1s/3s, `isFetchRace`). Webhooks for unregistered repos are acknowledged and dropped silently (audit status `dropped: repo not registered`); a job whose repo is unregistered after enqueue is `skipped`. `GIT_BASE_URL` selects the host the askpass may answer credentials for (default `github.com`) — match it to wherever the clones came from. `WORKSPACE_DISK_BUDGET_GB` (default 0 = off) optionally enables an LRU evictor that deletes the least-recently-used clone — i.e. unregisters that repo.
- The agent accesses the job's worktree through `mcp_elyoraa_read_file`, `mcp_elyoraa_list_files`, `mcp_elyoraa_search`, `mcp_elyoraa_git` (read-only argv; no shell), and `mcp_elyoraa_sync`. Git mutations and direct remote operations are unavailable. Prompts reference the sync tool as `{{.SyncCmd}}`.
- Per-repo policy: `.elyoraa.yml` on the repo's **default branch** (`enabled`, `budget_minutes`, `lines_per_minute`, `ignored_paths`, `mention_router`, `size_gate_mode`, `skip_label`). Always read from the bare clone's `HEAD` — a PR can't change its own policy. `enabled: false` drops jobs silently; the effective budget/ignored_paths are persisted on the job at enqueue.
- Opt-out label: `PR_SKIP_LABEL` (default `elyoraa:skip`) suppresses automated jobs on a labeled PR; explicit mentions override it.
- Reaction telemetry: GitHub `reaction` events on bot-authored comments increment `elyoraa_reactions_total{reaction,action}`; reactions on other users' comments are ignored.
- Webhook events:
  - `pull_request` (`opened`, `reopened`, `ready_for_review`) — auto-review.
  - `issue_comment` — if the comment mentions the bot on a PR, trigger rereview; if on an issue, trigger issue analysis.
  - `pull_request_review` (`submitted`, `edited`) — if the review body mentions the bot, trigger rereview.
  - `pull_request_review_comment` (`created`, `edited`) — if the comment mentions the bot, trigger rereview.
  - `issues` (`opened`, `edited`, `reopened`) — if the issue body mentions the bot, trigger issue analysis.
- Draft PRs and `synchronize` pushes are ignored.
- Issue analysis (`prompts/issue.md`) summarizes the issue, suggests approaches, proposes a breakdown when the scope is too big, and reads the merged discussion (issue comments + reviews + inline review comments, bounded to 80 entries / 64KB, oldest dropped first) so prior bot replies and human acceptance shape the response. It uses a repo worktree of the registered clone; a job whose clone was removed after enqueue falls back to a temp dir.
- When a human explicitly accepts a proposed breakdown, the reply ends with a `create_issues` JSON block; `processIssue` strips it, creates the issues via the GitHub App, and appends links under the posted comment. Labels are matched case-insensitively against repo labels; a curated vocabulary (plus the `BOT_HANDLE` tag) is auto-created when missing.
- Rereview is triggered by any PR comment/review mentioning the bot handle (`BOT_HANDLE`, default `elyoraa`) — in `keywords` router mode every mention enqueues a review directly; in `agent` mode a `respond` job interprets intent from the full discussion and dispatches bounded actions. The review/split keywords below matter only for the size gate.
- Status visibility: every enqueued job immediately posts a transient status comment saying "queued…" (`queuedBody`, per-type text) — the job may wait behind running work (`MAX_CONCURRENT_REVIEWS` workers), and that wait is visible. `Process` flips the same comment to the "in progress" text (`pendingBody`) on pickup; the final result then replaces it (issue/respond/split replies edit it) or deletes it (reviews). `POST /jobs/retry` refreshes or re-posts the comment the same way; a queue-full drop leaves a "couldn't queue" note on it. All carry the `elyoraa-in-progress` marker so conversation fetches filter them out.
- Reviews run in two phases so prior findings can't bias the fresh pass: (1) `prompts/review.md` — clean review, no prior context; (2) `prompts/reconcile.md` — a second Crush run that re-verifies the previous bot review's findings (latest `[bot]` review matching `BOT_HANDLE`, capped at 30) against the current code, merging still-present findings the fresh review missed and reporting `prior_findings_status` (fixed/still_present/unclear). Reconcile failures degrade to "unclear" — they never block posting the review.
- The Crush model is configurable via `CRUSH_MODEL` (optional; defaults to the model in the user's Crush config) and passed with `--model`; `jobModel`/`jobTimeout`/`attemptTimeout` select per-type overrides for `respond`/`gate` jobs.
- If the review output has no comments, the bot approves the PR; otherwise it requests changes. Output parsing is strict: a missing/unparseable ` ```json ` block is a transient failure (retried, then job failed) — never silently approved. An approval also requires non-empty `passes_completed`/`files_reviewed` audit fields.
- Issue analysis uses `prompts/issue.md`; PR review uses `prompts/review.md`; oversize split proposals use `prompts/split.md`; the mention router uses `prompts/respond.md`; size triage uses `prompts/gate.md`.
- The size gate runs at enqueue time in `WebhookUsecase.applySizeGate` (`internal/usecase/webhook.go`), so the verdict isn't delayed by queue wait. Effort estimate: `(added + deleted lines) / rate`; `changed_files > MAX_CHANGED_FILES` counts as oversize too. `pull_request`/review payloads carry the counters for free; the issue_comment path reuses its `GetPR` call; a `GetPR` fallback covers payloads missing stats. Payload counters can't see paths, so a counter-based oversize *nomination* is re-measured once via `GetPR` — generated files (`isGeneratedPath` markers: `*.lock*`, `*.gen.*`, `*.pb.*`, `generated/`, `__generated__/`, lock/sum files, minified assets, …) and `ignored_paths` are excluded — before the job is typed `split`/`gate`. Truncated payloads skip the fetch (the capped list can't rescue); a failed fetch keeps the counter verdict and process-time `refinedEstimate` still demotes a false nomination.
  - Intent is classified **before** the size check — explicit review/acceptance keywords are honored even when the fresh estimate is under budget.
  - The rate **self-calibrates per-repo first**: `calibratedLinesPerMinute` takes the median lines-per-minute of the last 30 completed review jobs for the repo from `JobTracker` (falling back to global samples), preferring the final attempt's `AttemptSeconds` over queue-plus-backoff `DurationSeconds`, clamped to [30,400]. Fewer than 5 samples → `PR_LINES_PER_MINUTE` fallback. Split/issue/gate/respond/failed jobs don't calibrate. `Process` writes the refined (generated-excluded) count back via `Tracker.Annotate` (`RefinedLines` — `Job.SizeLines` stays the enqueue base) so payload-counter inflation can't distort the rate; refinement is skipped for truncated file lists.
  - `Review.SizeLines`/`SizeEstMinutes` record the enqueue-time estimate on the job. Oversize jobs enqueue as `Type: "split"` — or `Type: "gate"` under `SIZE_GATE_MODE=agent`, which triages inline (verdict `review` continues into the normal path; `split`/malformed/failed → `processOversize`; the tracker/dedup slot are updated via `Annotate`/`UpdateJob` so a transitioned job no longer looks like a gate). `Process` re-verifies split jobs with per-file data (`refinedEstimate` rescales enqueue est × refined/raw lines, with generated files detected by path heuristics plus `.gitattributes` `linguist-generated`/`linguist-vendored` markers): still oversize → `processOversize` (`prompts/split.md`, posted as a comment, status `split_suggested`); under budget → demoted to review — **unless** `HasExplicitIntent()` (Origin `dispatched`/`accepted`): explicit intent is never demoted.
  - `Review.Origin` (`auto` < `mention_keyword` < `dispatched` < `accepted`) drives dedup supersession: a job may replace a same-key job when its origin rank is higher, or equal rank with a *different* type (human redirect) — equal-rank same-type is a duplicate and drops, and equal delivery IDs are redeliveries and always drop. A dispatched review/split supersedes a running `gate` job for the same SHA.
  - Trigger comments with review keywords (`anyway`, `full review`, `rereview`, `re-review`, `review again`, `recheck`, `check again`, …) keep the job a normal review.
  - Trigger comments with acceptance keywords (`create issues`, `break it down`, `split it`, `go ahead`, …) set `SplitCreate` on the split job **only when an outstanding proposal exists** (a completed `split` job without `SplitCreate` for the PR) — letting Crush emit a `create_issues` JSON block handled exactly like the issue-analysis flow. `createIssues` re-checks `mayCreateIssues` itself, so no caller path can bypass the authorization.
  - `respond` jobs use keys `respond/owner/repo/number/triggerID` outside the review keyspace: they never cancel a running review, an edited comment (same trigger ID, different delivery) supersedes the in-flight respond job, a redelivery (same delivery ID) drops, an edit whose key already completed drops, and they record their declared actions on `JobRecord.Actions` via `Tracker.Annotate`. Workers re-check `ctx.Err()` after `runCrush` before posting replies or executing actions, so a superseded job can't still act.

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
