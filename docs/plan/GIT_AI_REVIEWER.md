# elyoraa — AI PR Reviewer

A Go **engine** that receives GitHub webhooks from multiple repos across multiple projects and reviews each PR automatically using **Devin CLI** as the reviewer brain. Triggered by GitHub webhooks on PR open/update.

## Architecture

elyoraa is a multi-tenant webhook engine: one GitHub App installed on many repos, one webhook endpoint, many concurrent reviews.

### Repository caching & worktrees

To avoid cloning the same repo on every PR and to allow concurrent reviews of the same project without conflicts:

- **Bare clone cache** — each target repo is cloned **once** as a bare repo at `WORKSPACE_BASE/{org}/{repo}.git`. Bare repos are the natural source for `git worktree add` and avoid duplicating the default-branch working tree.
- **Token-less remote URL** — the bare clone is created with a token injected via `GIT_ASKPASS` (or `git -c http.extraheader`), **not** embedded in `remote.origin.url`. After clone, the remote URL is reset to a token-less `https://github.com/{org}/{repo}.git`. Every fetch supplies a fresh installation token via `GIT_ASKPASS` so no credential ever persists on disk in the cache.
- **Fetch on demand** — the cached clone is the _initial_ setup only. Every review fetches first to refresh refs. The cache is never trusted as fresh without a fetch.
- **Fork PR ref fetching** — a standard `git fetch --prune` only updates `refs/remotes/origin/*` for the base repo. For PRs from forks, the head commit only exists in `refs/pull/<n>/head`. **At clone time**, add `+refs/pull/*/head:refs/remotes/origin/pr/*` to the bare repo's `remote.origin.fetch` refspec, so a plain `git fetch --prune` fetches all PR heads automatically (slightly more network, but robust and simple). Every review then just runs `git fetch --prune` — no per-PR refspec needed. Without this, `git worktree add <headSHA>` fails on fork PRs. **Trade-off:** repos with thousands of open PRs will fetch many refs; if this becomes a problem, switch to per-PR fetches in phase 2.
- **Worktree per review session** — each review gets its own git worktree at `WORKSPACE_BASE/{org}/{repo}/wt-{pr}-{fullSHA12}-{nonce}`, checked out at the PR's **head SHA** (not the branch name). Full 12-char SHA (not 7) for path-debugging clarity; the `{nonce}` still guarantees uniqueness on webhook redelivery or a `synchronize` landing on the same commit. This prevents three classes of collision:
  - Two PRs sharing a head branch (fork branch reused, or contributor pushing twice).
  - A PR force-pushed mid-review (the old worktree stays valid at its SHA; the new push gets a fresh worktree).
  - Webhook redelivery or same-commit re-sync (`git worktree add` would fail on an existing path).
- **Worktree cleanup is mandatory and robust** — `AddWorktree` returns `(path, cleanupFunc, err)`; the caller defers `cleanupFunc` which is a no-op if `AddWorktree` failed. If `git worktree remove` fails because the Devin process is still holding files, cleanup kills the Devin **process group** (not just the main PID) and retries with `git worktree remove --force`. A periodic sweeper removes orphaned worktrees older than `WORKTREE_TTL_HOURS` (handles crashes).
- **Per-repo clone lock (singleflight)** — if two webhooks arrive for a brand-new repo simultaneously, only one clone runs; the second waits and reuses it. Keyed by `{org}/{repo}`.
- **Concurrency cap** — each review runs a Devin CLI session (not free on CPU/RAM). A worker pool / semaphore (`MAX_CONCURRENT_REVIEWS`) limits concurrent reviews; excess webhooks queue.
- **Idempotency / deduplication** — GitHub redelivers webhooks on 5xx/timeout, and pushes can land mid-review. Every job has a key `{owner}/{repo}/{pr}/{headSHA}`. On enqueue:
  - If a review for that key is already running → skip (log + return).
  - If a review for that PR is running but at a different SHA → cancel the in-flight review and start the new one (the old SHA is now stale). The new job keeps the existing "Review in progress…" issue comment ID so the replacement worker can edit/delete it.
    Phase 1: in-memory map. Phase 1.5/2: move dedup state into the same durable store as the queue (file-backed BoltDB/WAL or Redis) so it survives restarts and works across instances.

### Flow

````
GitHub PR webhook ──> [elyoraa engine]
   │
   ├─ verify webhook signature (HMAC SHA-256)
   ├─ parse event (pull_request: opened | synchronize | reopened)
   ├─ skip if draft or bot-authored PR
   ├─ dedup: compute job key {owner}/{repo}/{pr}/{headSHA}; skip or cancel in-flight
   ├─ enqueue review job → return 200 immediately (GitHub's 10s timeout is sacred)
   │
   └─ [worker picks up job]
      ├─ acquire per-repo singleflight lock
      ├─ if cache missing: `git clone --bare` (token via GIT_ASKPASS, not in URL) → WORKSPACE_BASE/{org}/{repo}.git
      ├─ reset remote.origin.url to token-less URL
      ├─ `git fetch --prune` (PR heads auto-fetched via clone-time refspec; fresh token via GIT_ASKPASS)
      ├─ `git worktree add wt-{pr}-{fullSHA12}-{nonce} <headSHA>` → (path, cleanupFunc, err)
      ├─ fetch PR metadata + patch (GitHub API); paginate through `pulls.ListFiles`; if patch > 100 KB or changed files > 50, include only diffstat + changed file list; if a file's `patch` is null (large file), skip inline mapping for that file
      ├─ render prompt → temp file (PR metadata + patch hunks/diffstat + mem0 keying)
      ├─ post "Review in progress…" comment at enqueue; store its issue comment ID in the job; if the job is replaced, the new worker takes over that comment
      ├─ run `devin --sandbox --permission-mode autonomous --config /app/devin-config.json --prompt-file <tmp>` in worktree
      ├─ parse Devin output (extract last ```json block: summary + comments[] with path/line/severity/suggestion; fallback to raw markdown)
      ├─ post review to PR: summary comment + inline positional comments (GitHub API)
      └─ cleanupFunc: kill Devin pgroup if needed, `git worktree remove --force`  (always, even on error)
````

### Why this shape

- **Multi-repo engine** — one GitHub App installable on many repos across projects; one webhook endpoint; one deploy.
- **Devin CLI as the reviewer** means no separate LLM API key, and Devin already has file/grep/glob tools to actually read the changed code in context (not just the diff). `devin --prompt-file <file>` runs non-interactively and prints the response — perfect for a service to shell out to.
- **Bare-clone cache + worktrees** — avoids redundant network I/O for active repos and lets concurrent reviews of the same project run in isolation without clobbering each other.
- **Go** gives a single static binary, fast webhook handling, easy deploy on your server.
- **Webhook trigger** = real-time reviews on every PR open/push.

## Headless Devin CLI authentication

`devin -p` in a container needs a Devin/Windsurf credential — the "no separate LLM API key" claim refers to the LLM provider, not to Devin's own auth. This is a first-mile blocker if not handled.

**This is the riskiest unknown in the whole design.** `devin auth login --force-manual-token-flow` is documented as "skip browser-based auth and manually paste a token" — but it still prompts for the token on stdin. In a non-interactive container, a prompt means a hang. **The spike must prove a headless auth path works before any engine code is written. If none work, the whole CLI-as-reviewer approach needs reconsidering.**

**Options to test, in priority order:**

1. **`WINDSURF_API_KEY` env var** — the CLI docs show this env is used by ACP/IDE flows. If `devin -p` reads it directly, this is the simplest path. Set in docker-compose env from a Docker secret.
2. **Pre-created credential volume** — run `devin auth login --force-manual-token-flow` once on a dev machine (interactive), copy the resulting `~/.config/devin/` files into a named volume, mount that volume into the container. Credentials persist across restarts. Most likely to work but requires a one-time interactive setup.
3. **`echo "$TOKEN" | devin auth login --force-manual-token-flow`** — piping the token via stdin may satisfy the prompt. Test it. If it works, can be done at container entrypoint with the token from a secret file.

**Whatever the method:** the credential must not be committed to git, baked into image layers, or visible in `ps`/`docker inspect`. Use Docker secrets or a gitignored `.env` bind-mounted.

## Security: sandboxed reviewer, not bypass

**Previous design used `--permission-mode bypass` (alias `dangerous`). This is unsafe for a service reviewing arbitrary PR code** — bypass auto-approves all tool calls including destructive shell commands and file writes. A malicious repo could get the reviewer to exfiltrate data or modify the host.

**Corrected approach:** `--sandbox --permission-mode autonomous` with a locked `/app/devin-config.json`. **Important:** in sandbox mode, the `edit` tool and the `Write(...)` scope still prompt because they run inside the CLI process and can't be bounded by the OS sandbox — in a non-interactive service, a prompt means a hang. The config must **explicitly deny** them:

```json
{
	"permissions": {
		"allow": [
			"read",
			"grep",
			"glob",
			"mcp__mem0__*",
			"Exec(git diff)",
			"Exec(git status)",
			"Exec(git log)",
			"Exec(git show)",
			"Exec(git rev-parse)",
			"Exec(git ls-files)"
		],
		"deny": ["edit", "Write(**)", "Exec(curl)", "Exec(wget)", "Fetch(**)"]
	}
}
```

**Network filtering:** Devin's sandbox network filtering is currently marked **unstable**. If `allowed_domains` is enabled, it must include Devin's own endpoints (`*.devin.ai`) and mem0's endpoint, or the session fails to reach the cloud. **Start with `network_mode: full` and restrict later** once the spike confirms the allowlist works.

Autonomous mode relies on OS-level sandboxing. **Docker requirements:**

- Sandbox needs `bubblewrap` (`bwrap`) and `socat` installed in the image.
- Linux only (server is Linux — fine).
- May need `SYS_ADMIN` capability or unprivileged-user-namespace tuning in Docker (`--security-opt seccomp=unconfined` or similar). Test this in the spike.

**Per-review isolation (corrected):** the previous design set `HOME`/`XDG_CONFIG_HOME`/`XDG_CACHE_HOME` all to per-review temp dirs — but **that breaks Devin auth**, because Devin's credentials live in `~/.config/devin/` (or wherever the auth volume is mounted). Corrected approach:

- **Keep `HOME` at the container's normal home directory** (e.g. `/root`) and `XDG_CONFIG_HOME` at the parent of the mounted `devin` config directory (e.g. `/root/.config` if credentials are in `/root/.config/devin`). Devin finds its user config and credentials under `XDG_CONFIG_HOME/devin/`.
- **Set only `XDG_CACHE_HOME` and `TMPDIR`** to per-review temp dirs (isolates caches and temp writes).
- **To stop the target repo's `.devin/config.local.json` from being loaded:** use the explicit `--config /app/devin-config.json` flag AND verify in the spike that it suppresses project-scope config loading. If `--config` only replaces the user config (not project config), the safe fallback is to **copy `devin-config.json` into the worktree as `.devin/config.json` before running Devin**, then delete it on cleanup. Same for the skill: copy `/app/skills/pr-review/SKILL.md` to `.devin/skills/pr-review/SKILL.md` in the worktree.

**Fallback if sandbox is unavailable on the server:** `--permission-mode normal` with a pre-approved permission list — any disallowed call fails instead of hanging on a prompt. Slower (some reviews may fail on a denied tool) but safe.

## Devin config & skill loading

**Problem:** when Go runs `devin` with `cmd.Dir` set to the worktree, Devin treats the worktree as the project and loads `.devin/config.json` and `.devin/config.local.json` from there (if they exist) — **not** from the elyoraa repo. Likewise, `.devin/skills/pr-review/SKILL.md` in elyoraa won't be discovered.

**Devin CLI has three config layers:** user (`~/.config/devin/config.json`), project (`.devin/config.json` in cwd), and local (`.devin/config.local.json`). The `--config` flag sets a config file path, but the docs don't clearly state which layer it replaces. **If it only replaces the user config, Devin could still pick up the worktree's project/local config.**

**Fix (two-pronged, verified in spike):**

1. **Primary:** pass `--config /app/devin-config.json` (elyoraa's config: mem0-mcp, permission rules) AND verify in the spike that it suppresses project-scope loading.
2. **Fallback (if `--config` doesn't suppress project config):** copy `/app/devin-config.json` into the worktree as `.devin/config.json` before running Devin, and copy `/app/skills/pr-review/SKILL.md` to `.devin/skills/pr-review/SKILL.md`. Delete both on cleanup (add to `cleanupFunc`). If the worktree already has a `.devin/config.json` or `.devin/skills/pr-review/SKILL.md`, do not overwrite them; in that case the fallback is unsafe and you must rely on `--config` and accept that the worktree's own project/local config may merge.
3. **Skill:** install as a **user/global skill** at `~/.config/devin/skills/pr-review/SKILL.md` in the container (works regardless of `--config` behavior), or inline the skill content directly into the prompt template. Don't rely on the worktree picking up elyoraa's project-scope skill.

## GitHub App permissions

**Previous scope was insufficient.** `issues:write` only covers issue/PR summary comments. Posting inline review comments via `POST /repos/{owner}/{repo}/pulls/{number}/reviews` requires `pull_requests:write`.

**Corrected scope:**

- `pull_requests:read` — fetch PR metadata + diff
- `pull_requests:write` — post review with inline positional comments
- `issues:write` — post summary comments on the PR (if kept separate from the review API)
- `contents:read` — git clone/fetch

## mem0 integration

mem0 is the **memory layer** that makes reviews get smarter over time per repo and per author.

- **Approach: mem0 MCP via Devin CLI.** elyoraa spawns `devin` with the mem0-mcp server configured in `/app/devin-config.json`. Devin calls mem0 tools directly during review — no mem0 client code in Go.
- **Conditional config at startup:** the Go entrypoint generates `/app/devin-config.json` at startup. If `MEM0_API_KEY` is unset, the `mcpServers.mem0` block is omitted entirely — making mem0 truly optional and avoiding a Devin startup failure if the mem0 server can't start.
- **Before review:** the prompt instructs Devin to search mem0 for context keyed by `{owner}/{repo}` and PR author — past issues found, repo conventions, author preferences.
- **After review:** the prompt instructs Devin to store findings back to mem0 (e.g. "repo X tends to forget input validation", "author Y prefers early returns").
- **Scope keys:** memory is namespaced by `{owner}/{repo}` (repo conventions) and `{author}` (per-author patterns) to avoid cross-project pollution.
- **Privacy guardrails:** the prompt explicitly tells Devin to store **only high-level patterns**, never raw code snippets, diffs, or secrets. Storing raw diffs in a shared memory layer could leak across repos/authors.
- **Fallback:** if mem0 MCP is unavailable, reviews proceed without memory (Devin skips the mem0 tool calls). mem0 usage is optional, not load-bearing.

## Git credentials: GIT_ASKPASS

Git calls the askpass helper twice: once for username, once for password. A minimal script baked into the container at `/app/git-askpass.sh`:

```bash
#!/bin/sh
case "$1" in
  *Username*) echo "x-access-token" ;;
  *Password*) cat "$ELYORAA_GIT_TOKEN_FILE" ;;
esac
```

Before each `git clone`/`fetch`, the Go code:

1. Mints a fresh installation token via the GitHub App JWT.
2. Writes that token to a per-review temp file, e.g. `/tmp/elyoraa/github-token-<jobid>`.
3. Sets `ELYORAA_GIT_TOKEN_FILE=<path>` and `GIT_ASKPASS=/app/git-askpass.sh` for the `git` command.

The **GitHub App private key** is the long-lived secret; keep it as a Docker secret or gitignored file and read it at startup to mint JWTs. The **short-lived installation token** is never committed, never baked into `remote.origin.url`, and never appears in `git` process arguments or `ps`/`docker inspect`.

This is safer than `git -c http.extraHeader="Authorization: token <token>"` which puts the token in the process arguments.

## Resource budget (target server: 2 CPU / 4 GB RAM / 60 GB disk)

Devin CLI is a **thin client** — LLM inference runs in Devin's cloud, not on the server. Locally each session only streams the prompt/response and executes tool calls (grep, glob, read) on the worktree. Realistic per-session footprint: ~150-300 MB resident, with short spikes during grep/glob over large codebases. Sessions are I/O-bound (waiting on cloud LLM responses), so 2 cores comfortably handle 2 concurrent reviews.

| Component                 | RAM           | Notes                             |
| ------------------------- | ------------- | --------------------------------- |
| Linux + sshd + misc       | ~300 MB       | baseline                          |
| elyoraa Go binary         | ~50-100 MB    | idle except during webhook bursts |
| Each active Devin session | ~150-300 MB   | spikes during grep/glob           |
| Bare clones + worktrees   | disk, not RAM | see disk note below               |

**Memory math at 4 GB:**

- OS + elyoraa: ~400 MB → ~3.6 GB free for reviews
- `MAX_CONCURRENT_REVIEWS=2`: ~1.8 GB per session budget → **comfortable headroom**

Default is **2**. Add 2 GB swap as an OOM safety net.

**Disk — two separate concerns:**

- **Persistent (bare clones):** `Σ bare clone size per onboarded repo` at `WORKSPACE_BASE/{org}/{repo}.git`. Grows only when a new repo is onboarded, never per-review. Needs its own **LRU eviction** (not just an alert) — when persistent clones exceed `WORKSPACE_DISK_BUDGET_GB`, evict the least-recently-reviewed repo's clone (re-cloned on next webhook if needed).
- **Transient (worktrees):** `MAX_CONCURRENT_REVIEWS × largest repo checkout size`. Bounded by the concurrency cap and reclaimed immediately after each review. The worktree sweeper (`WORKTREE_TTL_HOURS`) is only a safety net for crashes.

**Target server disk: 60 GB.** Budget: ~5 GB OS/binary/swap + ~25 GB persistent bare clones (~50 avg repos @ 500 MB) + ~2 GB transient worktree peak + ~28 GB headroom. `WORKSPACE_DISK_BUDGET_GB` (default 40) triggers LRU eviction before persistent clones eat into headroom.

## Repo layout

The Go service lives under `elyoraa/api/` with the module at `github.com/andikapradanaarif/elyoraa`.

```
elyoraa/
├── api/
│   ├── go.mod
│   ├── main.go                  # entrypoint, loads config, starts HTTP server
│   ├── internal/
│   │   ├── config/              # env loading (PORT, secrets, paths, concurrency limits)
│   │   ├── github/              # GitHub App auth, PR fetch, diff, post review (inline + summary)
│   │   ├── cache/               # bare-clone cache: singleflight clone, token-less URL, fork-ref fetch, worktree lifecycle, LRU eviction
│   │   ├── reviewer/            # orchestrates: worktree → render prompt → run devin → parse output
│   │   ├── queue/               # review queue: in-memory (phase 1) / file-backed BoltDB/WAL (phase 1.5) / Redis (phase 2)
│   │   ├── dedup/               # job key tracking + in-flight cancellation
│   │   ├── webhook/             # HTTP handler, event routing, signature verify, draft/bot skip
│   │   └── middleware/          # request ID and access logging
│   ├── prompts/
│   │   └── review.md            # review prompt template (rendered to temp file per review)
│   └── deploy/
│       ├── devin-config.json    # elyoraa's Devin config: mem0-mcp, permission allow/deny rules (baked into container at /app/)
│       ├── skills/
│       │   └── pr-review/
│       │       └── SKILL.md     # pr-review skill content (installed to ~/.config/devin/skills/pr-review/SKILL.md or inlined in prompt)
│       ├── git-askpass.sh       # GIT_ASKPASS helper for installation tokens
│       ├── Dockerfile           # multi-stage: golang build → runtime with devin + bwrap + socat
│       └── docker-compose.yml   # elyoraa service + volumes (WORKSPACE_BASE, ~/.config/devin, /tmp/elyoraa)
├── docs/plan/
│   └── GIT_AI_REVIEWER.md       # this document
├── .gitignore
└── README.md
```

## Implementation steps (in order)

1. **Scaffold Go module** — `go mod init github.com/andikapradanaarif/elyoraa`, baseline `main.go` with `net/http` server + `/health` + `/metrics` + request ID middleware.
2. **Config layer** — env vars: `PORT`, `GITHUB_WEBHOOK_SECRET`, `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` (path to a Docker secret or gitignored file), `DEVIN_BIN`, `DEVIN_CONFIG_PATH` (default `/app/devin-config.json`), `WORKSPACE_BASE`, `MAX_CONCURRENT_REVIEWS` (default **2**), `MAX_PATCH_BYTES` (default **100000**), `MAX_CHANGED_FILES` (default **50**), `WORKTREE_TTL_HOURS`, `WORKSPACE_DISK_BUDGET_GB` (default 40), `QUEUE_BACKEND` (`memory` | `file` | `redis`), `QUEUE_FILE_PATH` (path for the BoltDB/WAL queue store, phase 1.5), `REDIS_URL` (phase 2).
3. **Observability (from day one)** — structured JSON logs (request ID, job key, owner/repo, PR number, SHA, duration, status), `/health` endpoint, `/metrics` (Prometheus: queue depth, active reviews, review duration histogram, disk usage, worktree count). Cheap in Go, saves debugging time.
4. **GitHub client** (`internal/github`) — using `google/go-github` + `golang.org/x/oauth2` for GitHub App auth (installation tokens, refreshed ~10 min):
   - `VerifyWebhookSignature(payload, sig, secret)` — HMAC-SHA256
   - `GetPR(ctx, owner, repo, number)` — metadata + paginated list of changed files with patches. Pre-computed patch hunks are used for inline comment mapping. Handle `patch == null` or truncation for large files; skip inline mapping for those files and fall back to summary notes. Apply a `MAX_CHANGED_FILES` threshold (e.g. 50) beyond which only diffstat + file list is passed to the prompt.
   - `PostReview(ctx, owner, repo, number, summary, inlineComments)` — posts **both** summary comment and inline positional comments via the review API. Fallback to summary-only if inline mapping fails.
   - `InstallationToken(ctx, installationID)` — short-lived token for `git fetch`/`clone` (written to a per-review temp file and supplied via `GIT_ASKPASS`, never in remote URL or process args)
5. **Repo cache** (`internal/cache`):
   - `EnsureBareClone(ctx, owner, repo, installationID)` — singleflight per `{org}/{repo}`; clones to `WORKSPACE_BASE/{org}/{repo}.git` using `GIT_ASKPASS=/app/git-askpass.sh` (token from `ELYORAA_GIT_TOKEN_FILE` temp file); adds `+refs/pull/*/head:refs/remotes/origin/pr/*` to `remote.origin.fetch` at clone time (fork PR heads auto-fetched); resets `remote.origin.url` to token-less URL after clone.
   - `Fetch(ctx, owner, repo, installationID)` — writes fresh installation token to a per-review temp file, sets `ELYORAA_GIT_TOKEN_FILE`, runs `git fetch --prune` (PR heads auto-fetched via clone-time refspec).
   - `AddWorktree(ctx, owner, repo, prNumber, sha)` → returns `(path, cleanupFunc, err)`; worktree at `wt-{pr}-{fullSHA12}-{nonce}`. `cleanupFunc` is a no-op if `AddWorktree` failed; otherwise kills Devin pgroup + `git worktree remove --force` on failure.
   - `Sweep(ctx)` — periodic goroutine removing orphaned worktrees older than `WORKTREE_TTL_HOURS`.
   - `EvictLRU(ctx)` — when persistent clones exceed `WORKSPACE_DISK_BUDGET_GB`, evict least-recently-reviewed repo's bare clone. **Last-used tracking:** write a `last-reviewed` timestamp file into the bare repo dir on every `Fetch`. **Never evict** a bare repo with active worktrees (check `git worktree list` or an in-use counter before deleting).
6. **Dedup** (`internal/dedup`) — job keys `{owner}/{repo}/{pr}/{headSHA}` → running review (plus pending comment ID). On enqueue: skip if same key running; cancel + replace if same PR different SHA, passing the pending comment ID to the new job. **Phase 1:** in-memory map. **Phase 1.5/2:** move dedup state into the same durable store as the queue (file-backed BoltDB/WAL or Redis) so it survives restarts and works across instances.
7. **Webhook handler** (`internal/webhook`) — accept `pull_request` events where `action ∈ {opened, synchronize, reopened}`; **skip draft PRs and bot-authored PRs** (one-line checks, saves cycles); verify signature; dedup; post "Review in progress…" comment (with an `<!-- elyoraa-in-progress -->` footer and store its issue comment ID); enqueue; return `200` immediately. If a queued job is replaced by a newer SHA, pass the pending comment ID to the replacement worker.
8. **Review queue** (`internal/queue`):
   - **Phase 1 (in-memory):** buffered `chan ReviewJob` + worker pool of `MAX_CONCURRENT_REVIEWS` (default 2). Each worker wraps its work in a `defer/recover` so a panicking review (e.g., a malformed Devin response) kills only that worker, not the whole process. Log the panic with request/job metadata and run the job's `cleanupFunc`.
   - **Phase 1.5 (file-backed):** durable queue surviving restarts without Redis ops burden. **Prefer BoltDB or a simple WAL file over Badger** — Badger uses mmap + has compaction/GC overhead that's overkill for a mostly-empty queue. Mount the queue path on the same persistent volume as `WORKSPACE_BASE` and add it to the disk-budget math.
   - **Phase 2 (Redis):** `BRPOP`/`XREADGROUP` for multi-instance + horizontal scaling. Redis as docker-compose sidecar.
   - **Backpressure:** if queue full, return `200` + log drop + post "review queued, will run later" comment.
9. **Reviewer orchestrator** (`internal/reviewer`) — runs inside a worker slot:
   - `cache.Fetch` → `cache.AddWorktree` (defer `cleanupFunc`).
   - Fetch PR metadata + patch. **Prompt size / scale guard:** if the patch text > 100 KB, changed files > 50, or a file's `patch` is `null` (GitHub truncates large files), include only diffstat + changed file list in the prompt and rely on Devin's `read`/`grep` tools. For files with a `null` patch, drop any inline comments and surface them in the summary instead.
   - Render `prompts/review.md` → temp file with PR metadata + patch hunks/diffstat + mem0 keying (`{owner}/{repo}`, `{author}`).
   - **Isolation (corrected):** keep `HOME` at the container home and `XDG_CONFIG_HOME` at the parent of the mounted `devin` creds dir (e.g. `XDG_CONFIG_HOME=/root/.config` if creds are in `/root/.config/devin`); set only `XDG_CACHE_HOME` + `TMPDIR` to per-review temp dirs.
   - **Config fallback:** if spike shows `--config` doesn't suppress project-scope loading, copy `/app/devin-config.json` into the worktree as `.devin/config.json` before running (delete in `cleanupFunc`).
   - Run `devin --sandbox --permission-mode autonomous --config /app/devin-config.json --prompt-file <tmp>` with `cmd.Dir` = worktree. Capture stdout.
   - Parse output: extract last ```json block (summary + `comments[]`with`path`/`line`/`severity`/`suggestion`); fallback to raw markdown if parse fails.
10. **Post review** — `github.PostReview`: summary comment + inline positional comments mapped to diff line numbers via the patch hunks. If a file's patch is missing/too large to map, add those comments to the summary instead of an inline comment. Summary-only fallback. Edit or delete the "Review in progress…" comment using its issue comment ID stored in the job (passed to replacement workers on cancel-and-replace).
11. **Prompt design** (`prompts/review.md`) — instruct Devin to: focus on the diff, flag bugs/security/perf, avoid nitpicks, emit **strict output schema** (JSON block with `summary` + `comments[]` containing `path`/`line`/`severity`/`suggestion`), be concise, **use mem0** to look up repo/author context before reviewing and store **high-level patterns only** (never raw code/diffs/secrets) after.
12. **Devin skill** (`deploy/skills/pr-review/SKILL.md`) — review guidance + mem0 usage patterns. Copied to `~/.config/devin/skills/pr-review/SKILL.md` in the container (user-scope), or inlined into the prompt. Not project-scope (worktree is the project root).
13. **Deploy** — `deploy/Dockerfile` (multi-stage: golang build → runtime with `devin` + `bwrap` + `socat` + `/app/git-askpass.sh`), `deploy/docker-compose.yml` (elyoraa service + volumes: `WORKSPACE_BASE` for bare clones, `~/.config/devin` for Devin credentials, a writable `/tmp/elyoraa` for per-review git tokens). The **GitHub App private key** should be a Docker secret or gitignored file at a known path (e.g. `/run/secrets/github-app-private-key`); the short-lived installation tokens are written to `/tmp/elyoraa/` at runtime, not a Docker secret. Headless Devin auth handled per the auth section above. **Log redaction:** structured logs must never emit `GITHUB_APP_PRIVATE_KEY`, installation tokens, `MEM0_API_KEY`, or Devin credentials.
14. **GitHub side** — create a GitHub App scoped to `pull_requests:read` + `pull_requests:write` + `issues:write` + `contents:read`. Set webhook URL + secret. Install on target repos/projects.

## Spike (before writing the Go code)

De-risk the riskiest chain end-to-end before building the full engine. **Ordered by risk — stop and reconsider the CLI approach if step 1 fails.**

1. **Headless Devin auth in Docker (highest risk)** — test all three options: `WINDSURF_API_KEY` env, pre-created credential volume, `echo "$TOKEN" | devin auth login --force-manual-token-flow`. Confirm credentials persist in a volume and survive restart. **If none work, the whole CLI-as-reviewer approach needs reconsidering.**
2. **Sandboxed review in a worktree** — `devin --sandbox --permission-mode autonomous --config <config> --prompt-file <prompt>` against a worktree. Confirm `bwrap`/`socat` work in Docker, the permission `deny` rules (`edit`, `Write(**)`, `Exec(curl)`, `Exec(wget)`, `Fetch(**)`) enforce correctly, and the session doesn't hang on a prompt. Verify `--config` suppresses project-scope loading (if not, test the copy-into-worktree fallback). Start with `network_mode: full`; only test `allowed_domains` if time permits.
3. **Real fork-PR webhook → worktree** — receive a real `pull_request` webhook, verify signature, create a worktree for a fork PR using an installation token + clone-time `+refs/pull/*/head:refs/remotes/origin/pr/*` refspec.
4. **Post a real review with inline comments** — confirm the exact GitHub App permission set (`pull_requests:write`) works for `POST /repos/.../pulls/<n>/reviews` with positional comments, and that diff-position mapping is correct.

If the spike passes, build the engine. If sandbox doesn't work in Docker, fall back to `--permission-mode normal` + pre-approved list.

## Phases

### Phase 1 (MVP)

- Webhook handler with signature verification + draft/bot skip
- GitHub App auth + PR fetch + pre-computed patch
- Bare-clone cache (token-less URL, fork-ref fetch, singleflight) + worktree-per-review (nonce, cleanup)
- Dedup (skip/cancel on job key)
- In-memory review queue, `MAX_CONCURRENT_REVIEWS=2`
- Sandbox+autonomous Devin invocation with locked config + per-review isolation
- mem0 MCP loaded for repo/author memory (with privacy guardrails)
- Review posted as summary + inline positional comments
- Observability: structured logs, `/health`, `/metrics`

### Phase 1.5

- File-backed BoltDB/WAL queue (durable, no Redis ops)

### Phase 2

- Redis-backed queue (Redis as docker-compose sidecar) for multi-instance + horizontal scaling
- Reactions/check-status (`pending` → `commented`)
- Per-repo config file (`.elyoraa.yml`) for prompt customization
- Bare-clone LRU eviction metrics + disk budget dashboards
- mem0 memory curation tooling (review stored memories, prune stale ones)

## Decisions locked

1. **Auth to GitHub:** GitHub App (scoped to `pull_requests:read` + `pull_requests:write` + `issues:write` + `contents:read`).
2. **Review output format:** summary comment **and** inline positional comments (both, from phase 1).
3. **Deploy target:** Docker (Dockerfile + `docker-compose.yml`).
4. **Scope:** multi-repo / multi-project.
5. **Memory:** mem0 via MCP, namespaced by `{owner}/{repo}` and `{author}`, high-level patterns only. Conditional config at startup (omit `mcpServers.mem0` if `MEM0_API_KEY` unset).
6. **Server:** 2 CPU / 4 GB RAM / 60 GB disk; `MAX_CONCURRENT_REVIEWS=2`; 2 GB swap.
7. **Reviewer mode:** `--sandbox --permission-mode autonomous` with locked config (explicit `deny` for `edit`, `Write(**)`, `Exec(curl)`, `Exec(wget)`, and `Fetch(**)`). Fallback: `normal` + pre-approved/deny list if sandbox unavailable. Network: start `full`, restrict later.
8. **Devin config/skill:** explicit `--config` path + user-scope skill. Fallback: copy config into worktree as `.devin/config.json` if `--config` doesn't suppress project-scope loading (verified in spike).
9. **Git credentials:** `GIT_ASKPASS=/app/git-askpass.sh` with the installation token from a per-review temp file pointed to by `ELYORAA_GIT_TOKEN_FILE`; the GitHub App private key is a long-lived Docker secret or gitignored file; token-less remote URL (no secrets on disk or in process args).
10. **Fork PRs:** clone-time refspec `+refs/pull/*/head:refs/remotes/origin/pr/*` (auto-fetched by `git fetch --prune`). Per-PR fetches are a phase-2 optimization for repos with very many open PRs.
11. **Idempotency:** job key `{owner}/{repo}/{pr}/{headSHA}`, skip-or-cancel on conflict. Phase 1 in-memory; phase 1.5/2 in durable store with queue.
12. **Worktree paths:** `wt-{pr}-{fullSHA12}-{nonce}` (full SHA for clarity, nonce prevents collision). Cleanup via `(path, cleanupFunc, err)` pattern; `--force` + pgroup kill on failure.
13. **Per-review isolation:** keep `HOME` at the container home and `XDG_CONFIG_HOME` at the parent of the mounted `devin` creds dir; only `XDG_CACHE_HOME`/`TMPDIR` per-review.
14. **Queue:** phase 1 in-memory; phase 1.5 BoltDB/WAL (not Badger — too heavy for mostly-empty queue); phase 2 Redis sidecar.
15. **Bare-clone LRU:** `last-reviewed` timestamp file per repo; never evict repos with active worktrees.
16. **Prompt size / scale guard:** if the patch > 100 KB, changed files > 50, or a file's `patch` is `null`, pass diffstat + changed file list only; large/unmappable files are surfaced in the summary, not inline.
17. **Output schema:** last ```json block with `summary`+`comments[]`; fallback to raw markdown.
18. **Observability:** structured logs (with redaction of all secrets/tokens), `/health`, `/metrics` from day one.
19. **Pending status:** "Review in progress…" issue comment at enqueue (with `<!-- elyoraa-in-progress -->` marker), edited/deleted when done; the comment ID is part of the job and survives cancel-and-replace.
20. **Worker resilience:** each queue worker wraps its work in `defer/recover` and runs `cleanupFunc` on panic, so a single bad review cannot crash the engine.
