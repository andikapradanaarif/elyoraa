> Runtime update: elyoraa now uses the installed [Crush fork](https://github.com/andikapradanaarif/crush). The historical Devin CLI/config/sandbox details below are superseded by [the current runtime notes](../../api/README.md#crush-runtime).

# Mention router — conversational front door

> **Status: implemented.** Phase A deterministic fixes, the `respond` mention
> router (`MENTION_ROUTER`), the `gate` triage (`SIZE_GATE_MODE`), and the
> post-review hardening (job origins, fenced action protocol, authorization,
> proposal gating, nomination bands, `.gitattributes` markers, per-repo
> calibration) all landed; defaults are `keywords` + `deterministic` so the
> rollout is opt-in. This doc is the design record.

A lightweight **respond agent** reads every bot mention in conversation context and *declares* what to do next; Go executes the declared actions deterministically. Heavy work (review, split analysis, issue analysis) stays in separate worker runs with their own context windows — the conversational context never contaminates a review pass.

## Summary

In `keywords` mode, mention routing is done in Go before any agent runs: `classifyOversize` substring-matches the trigger comment against keyword lists. On `de-almas/nexus#486` this produced the incident below: a human explicitly accepted a split proposal and elyoraa posted a full review instead.

With `MENTION_ROUTER=agent`, the mention path instead creates a small Crush run (the *respond* job) that sees the thread and picks an action from a fixed vocabulary — `request_review`, `request_split`, `request_issue_analysis`, `create_issues`, or just a reply. The agent declares actions in a dedicated ` ```elyoraa-actions ` fence; Go validates and executes. Nothing the agent emits can touch GitHub except through the bounded executor. The same pattern optionally backs the auto-review size gate: a manifest-level `gate` triage run decides review-vs-split on complexity signals that `lines ÷ rate` can't express (`SIZE_GATE_MODE`).

## Motivation — what went wrong on nexus#486

1. **10:58** — PR opened (~4,180 lines incl. generated contract files). `applySizeGate` used webhook payload counters → ~25 min > 15 min budget → `split` job → proposal posted. Correct.
2. **11:06–11:33** — seven sibling codegen-heavy PRs were opened and reviewed fast. `calibratedLinesPerMinute` recorded enqueue-time `SizeLines` (**including** generated files the reviewer doesn't actually read) over wall-clock duration → the calibrated rate drifted up toward the 400 lpm clamp.
3. **15:03** — `@elyoraa create the issues` → `handleIssueComment` → `GetPR` → `applySizeGate` re-measured: ~4,180 lines ÷ inflated rate ≈ under budget → `size.tooBig()` false → early return before `classifyOversize` ran → normal review job → `CHANGES_REQUESTED` at 15:11.

Three structural failures, all the same shape — **explicit human intent loses to re-measurement**:

1. `applySizeGate` consulted `classifyOversize` only *after* the `tooBig` check. Acceptance keywords were unreachable on a PR whose estimate dropped — via calibration drift, base retarget, force-push, or the payload-counters → per-file-files delta.
2. `Process` demoted `split` jobs to reviews whenever `refinedEstimate` fit budget; `SplitCreate` wasn't consulted. An accepted proposal could be silently converted to a review even when it *was* enqueued correctly.
3. `dedup.TryStart` dropped a `split`+`SplitCreate` job as a duplicate if a `split` proposal job for the same SHA was still running — the acceptance couldn't supersede its own proposal.

A second-order bug that contributed: `SizeLines` recorded for calibration included generated files (`contract/openapi.yaml`, `contract/src/generated/openapi.ts` — `isGeneratedPath`'s markers don't catch every layout), so the "review rate" inflated on codegen-heavy repos and the effective budget shrank over time.

## Goals

- `@elyoraa <anything>` behaves like a conversation: questions get answers, commands get actions, ambiguity gets a clarifying reply — not a 10-minute review.
- Explicit human intent (accept a proposal, force a review) is honored regardless of what a fresh size estimate says.
- The split decision reflects *complexity* (which files changed, how) — not just `lines ÷ rate`, which can't express that 4k lines of generated protobuf is cheaper than 600 lines of subtle concurrency.
- The action vocabulary is fixed and small; Go remains the sole executor of side effects.
- The review pass stays context-free (bias firewall preserved).
- Deterministic keyword routing remains available behind a config flag.

## Non-goals

- Live MCP write tools for the agent (`create_issue` etc.) — declared actions are strictly more auditable; revisit only if interactive reads prove insufficient.
- True multi-turn sessions — statelessness + thread injection is enough.
- Fully replacing the deterministic size gate — it stays as the *nomination* layer on the auto path; what it nominates to (`gate` triage vs `split`) is configurable (see § Agent-assisted size gate).
- Routing by a second model/provider — the router is the same Crush CLI, just a smaller prompt.

## Architecture

### Context contract — the bias firewall

Each job type gets exactly the context its job needs — and no more:

| Job type | Sees | Never sees |
|---|---|---|
| `respond` | trigger comment, **full conversation** (issue comments + review summaries + inline review comments), PR/issue metadata, size stats, prior job status, worktree (read-only, if cloned) | the rendered diff as input (may `git diff` itself only to *answer a question*, never to render a verdict) |
| `review` | diff + code (worktree at head SHA) | the thread — by design, so prior findings can't bias the fresh pass |
| `reconcile` | prior findings + fresh comments + code | the thread |
| `split` | diff stats + worktree + thread (needs it to see the acceptance) | — |
| `issue` | issue body + thread + worktree at HEAD | — |
| `gate` | file manifest (path, +/−, generated-flag), size stats, PR title/body, read-only worktree (may `git diff`/`git log` or open files to judge complexity) | the rendered diff as input, the thread (none exists on the auto path) |

The property that matters: **the agent that talks to the human is never the agent that judges the code.** Delegation happens through job enqueueing — `request_review` literally becomes a separate Crush run with a clean prompt. `TriggerComment` is only ever passed into `issue.md`, `split.md`, and `respond.md` template data — never `review.md` or `reconcile.md` (a test pins this).

### Flow

````
GitHub webhook (mention-bearing events)
   │
   ├─ verify signature, skip bots, isBotMentioned (unchanged)
   ├─ MENTION_ROUTER=agent → slash command? → deterministic path
   │                        otherwise → enqueue Type:"respond" job
   │                        keywords → deterministic path (with phase-A fixes)
   │
   └─ [worker: processRespond]
      ├─ optional worktree (same as processIssue: worktree if cloned, temp dir else)
      ├─ fetch full conversation (fetchConversation), prior job status (JobTracker)
      ├─ render prompts/respond.md → runCrush(validateRespondOutput)
      ├─ parse reply text + trailing ```elyoraa-actions block
      ├─ ctx.Err() check (drop silently if superseded while Crush ran)
      ├─ post reply as the pending comment
      └─ execute actions via bounded executor (auth + proposal checks in Go):
           • request_review    → dispatch(Type:"review", Origin: dispatched)
           • request_split     → dispatch(Type:"split",  Origin: dispatched)
           • request_split{create:true} → dispatch(Type:"split", SplitCreate, Origin: accepted)
           • request_issue_analysis → dispatch(Type:"issue", Origin: dispatched)
           • create_issues     → existing createIssues() (≤10, label resolution,
                                 COLLABORATOR+ check, visible-proposal check)
````
````
pull_request opened/reopened/ready_for_review (no mention)
   └─ SIZE_GATE_MODE=deterministic → applySizeGate → Type:"review" | "split"   (unchanged)
      SIZE_GATE_MODE=agent        → ≤1× budget → "review" (free)
                                    1×–3× budget → Type:"gate" → triage run
                                    >3× or truncated file list → "split" directly
                                    → verdict transitions the job inline → review|split
````

### The action protocol

The respond reply is normal markdown plus one optional fenced block at the very end:

````
```elyoraa-actions
{
	"actions": [
		{"type": "request_review"},
		{"type": "request_split", "create": true},
		{"type": "request_issue_analysis"},
		{"type": "create_issues", "issues": [{"title": "…", "body": "…", "labels": ["refactor"]}]}
	]
}
```
````

Only the `elyoraa-actions` fence is protocol — ordinary ` ```json ` blocks are never stripped or parsed, so illustrative JSON in a reply is safe. The reply text is everything outside that block; a well-formed block is always stripped before posting (`parseRespondActions` — stricter than `parseIssueActions`, which is kept for the issue/split prompts' `create_issues` block).

Execution rules (all in Go, all logged on the job record):

- **Authorization is enforced in code, not the prompt** — `TriggerAuthor`/`TriggerAssociation` ride on the job from the webhook payload through dispatch. `request_review`/`request_split`/`request_issue_analysis` require the PR author or a COLLABORATOR+ association (`mayDispatch`); `create_issues` requires COLLABORATOR+ (`mayCreateIssues`). The final `createIssues` choke point re-checks `mayCreateIssues` itself, so no caller path can bypass it; a skipped action surfaces a visible "I can't create issues — …" note in the reply.
- `request_review` / `request_split` enqueue a job via `dispatchJob` — dedup + pending comment + queue submit, **no `applySizeGate`, no triage** (the agent already factored size into its choice). `Origin: dispatched` marks explicit intent so `Process` never demotes it.
- `request_split` accepts `"create": true` → dispatched as `split` + `SplitCreate` + `Origin: accepted`: the diff-aware split pass re-derives the breakdown against current code and creates the issues in one run. This is the "accepted, but the proposal looks stale" path — strictly better than the respond agent authoring issues blind (it has no rendered diff).
- `create_issues` requires an **outstanding proposal**: a completed `split` job without `SplitCreate` for the PR, or a `create_issues` proposal visible in the discussion. Without one the action is skipped with a note — acceptance keywords alone can never conjure issues, in either router mode.
- `request_review` and `request_split` are mutually exclusive — first one wins, the rest are logged and dropped.
- `request_issue_analysis` is only valid on non-PR numbers; ignored on PRs.
- `create_issues` executes inline in the respond worker — the same `createIssues` path (≤ `maxCreatedIssues`, label resolution, bot tag, links appended under the reply). Each created issue body embeds `<!-- elyoraa:src=owner/repo#num job=key -->` so future dedup/idempotency checks can find bot-created issues without guessing.
- Dispatched jobs carry a **synthetic `DeliveryID`** (`{respondDelivery}:dispatch:{type}`) — never the respond job's own, or their webhook-status updates would clobber its record and confuse `BindCancel`'s delivery-ID takeover check. `dispatchJob` re-fetches the PR first — fresh `HeadSHA`, skip-with-note when the PR went draft/merged/closed since the mention.
- Unknown action types → ignored + warn log (hallucination containment). More than `RESPOND_MAX_ACTIONS` (default 4, validated ≥1) → the rest drop.
- No actions block at all → pure reply. Valid, common case (questions, acknowledgements).

### Conversation fetching — `fetchConversation`

A PR's discussion is three streams in GitHub's API; an issue's is one. `fetchConversation` merges them into a single chronological, type-tagged thread so the respond agent sees the whole conversation — including its own inline findings and replies to them:

| Stream | API | Notes |
|---|---|---|
| Issue comments | `Issues.ListComments` | The discussion surface; works for PRs and issues |
| Review summaries | `PullRequests.ListReviews` | Human/bot review bodies — approvals, "LGTM but…" |
| Inline review comments | `PullRequests.ListComments` | Code-level threads; `Line` falls back to `OriginalLine` so outdated comments don't render as `path:0` |

- Each entry renders with author, timestamp, `(bot)` marker, type tag (`comment` / `review` / `inline`), and for inline comments the `path:line` anchor so "why did you flag this?" has the finding attached.
- One shared bound (80 entries / 64 KB) across the merged stream; oldest entries drop first, always keeping the trigger comment.
- `<!-- elyoraa-in-progress -->` placeholders are filtered — so final replies are posted **without** the marker or they'd filter themselves out of every future thread.
- Reused by `processIssue` and `processOversize` in place of the old issue-comments-only fetch — the split agent equally benefits from seeing inline review replies.
- Issues have only the first stream — the function degenerates to the old behavior.
- Optional later: `Issues.ListIssueTimeline` events (force-push, base retarget, cross-references).

### Prompt design — `prompts/respond.md`

Template data: `RepoKey`, `PRNumber`, `IsPR`, `Title`, `Body`, `Author`, `HeadSHA`/`BaseSHA`, `BotHandle`, `TriggerComment`, `Conversation`, `RepoAvailable`, `Size` (`EstMinutes`, `Lines`, `Budget`, `Oversize` — `Oversize` reflects the recorded size verdict including truncation, not just minutes), `PriorJobStatus`.

Prompt outline:

1. You are elyoraa. A human mentioned you; the thread is above. Decide what they want.
2. Available actions + exact JSON schema — one ` ```elyoraa-actions ` block max, at the very end.
3. `create_issues` only on **explicit human acceptance** of a breakdown visible in the thread — never invent a proposal to accept. If the thread's proposal looks stale (PR since split/shrunk), prefer `request_split` with `"create": true`.
4. `request_review` when they ask for review/rereview or ask a question whose honest answer is "the review pass should look." `request_split` for oversize PRs where no proposal exists yet and the human wants one.
5. Just reply when it's a question, discussion, or out-of-scope request. Answer code questions by reading the worktree if available — but never emit review verdicts; that's a different pass.
6. Never claim an action happened — actions are executed by the host after the reply; say "kicking off a review" not "I reviewed".
7. Slash commands exist: if the human wants deterministic behavior they can say `@elyoraa /review`, `/split`, or `/accept` — no interpretation needed.

### Slash commands — deterministic escape hatch

In `agent` mode, `@elyoraa /review`, `@elyoraa /split`, and `@elyoraa /accept` bypass the respond agent entirely and take the deterministic path (`/accept` still requires an outstanding proposal). This gives users a guaranteed-correct path when the agent might misread, and it's the natural migration target away from keyword matching.

### Deterministic fixes (Phase A — shipped, needed in both modes)

1. **`applySizeGate` — intent before measurement.** `classifyOversize(r.TriggerComment)` runs before the size check: `oversizeCreate` → `Type:"split"`, `SplitCreate`, `Origin: accepted`, return; `oversizeReview` → return as review; else fall through to the size check. Only affects jobs with a `TriggerComment` (auto-reviews have none). `oversizeCreate` additionally requires an outstanding proposal — the latest tracker record for the PR is a completed `split` without `SplitCreate` — or everyday phrases like "breakdown" would mint issue-creation jobs on any PR.
2. **`Process` — don't demote explicit intent.** The demotion branch is guarded by `!j.HasExplicitIntent()` — `dispatched`/`accepted`-origin jobs always reach `processOversize`/`processIssue` regardless of refined size.
3. **`TryStart` — origin-ranked supersession** (see § Dedup).
4. **Calibration input.** `Process` writes the refined (generated-excluded) line count back via `Tracker.Annotate` as `JobAnnotation.RefinedLines` — `Job.SizeLines` stays the enqueue-time base so `POST /jobs/retry` rescales from the right number. Refinement is skipped for truncated file lists (the manifest would undercount). `medianRate` prefers `AttemptSeconds` — the final Crush attempt's duration — over `DurationSeconds`, so queue wait and retry backoff don't distort the rate; calibration prefers same-repo samples before global fallback.
5. **`processIssue`/`processOversize` drop the marker on their final replies** — otherwise the analysis is filtered out of every future `fetchConversation` thread.

### Agent-assisted size gate — let the agent judge complexity, not lines

`lines ÷ rate` is a weak proxy for review effort: 4,000 lines of generated protobuf is cheaper to review than 600 lines of subtle concurrency, and it was the payload-counter estimate that nominated `nexus#486` in the first place. When `SIZE_GATE_MODE=agent`, the deterministic gate stops *deciding* and starts *nominating*, in bands:

- ≤1× budget → `review` job (zero added cost — most PRs never pay for the agent);
- 1×–3× budget → `gate` triage job;
- >3× budget or a truncated file list → `split` directly — clearly-huge PRs don't need an agent to agree, and this caps triage cost.

A `gate` job is processed **inline by the worker** — it is not re-enqueued. `triageGate` renders `prompts/gate.md` — file manifest (path, +/−, generated flag), PR title/body, size stats — plus the read-only worktree, where the agent may `git diff`/`git log` or open files to judge complexity (it never sees a thread; none exists on the auto path). Crush replies with a verdict block:

```json
{"verdict": "review" | "split", "reason": "…"}
```

`review` → the same worker continues into the existing review path; `split` → `processOversize` directly with the refined size. The verdict *transitions* the job — `d.UpdateJob` syncs the dedup slot and `Tracker.Annotate` records the new type on the job record, so supersede rules and the audit trail see the post-verdict phase, not "gate". The same sync happens when a `split` job is demoted to `review`.

- **Timeout semantics:** the gate job's parent context gets `REVIEW_TIMEOUT` (a gate→review run needs the full review budget). `GATE_TIMEOUT` bounds only the triage pass via a child context: a triage deadline → deterministic fallback (split — the nomination that created the job); a *parent* cancellation → cancelled. Never silently review a nominated-oversize PR, and never starve a review that won its triage.
- Job key: the normal `owner/repo/pr/sha` — new pushes cancel it like any review job.
- Explicit-intent jobs never re-enter triage — dispatch skips `applySizeGate` entirely.
- `gate` records are excluded from calibration by the `Type != "review"` filter.
- Blind spot (same as before): an under-budget but genuinely subtle PR is never nominated. Acceptable — the review still runs — unless the nomination threshold is lowered.

### Dedup, job keys, and origins

Supersession is driven by a single field instead of ad-hoc flag combinations: `Review.Origin` ∈ `auto` (0) < `mention_keyword` (1) < `dispatched` (2) < `accepted` (3), with `IntentRank()`/`HasExplicitIntent()` helpers.

- Respond jobs live outside the `{owner}/{repo}/{pr}/{headSHA}` keyspace — two consecutive comments on the same PR/SHA must both run, and a respond job must never cancel a running review. Key: `respond/{owner}/{repo}/{number}/{triggerID}` where `triggerID` is the comment/review/issue ID from the payload.
- Same key: a job supersedes an in-flight one when its origin rank is **higher**, or **equal rank with a different type** (the human redirected: "review" then "actually split it"). Equal rank + same type is a duplicate and drops — two `accepted` jobs must never both run, or issues would be created twice.
- **Delivery-ID fencing:** equal `DeliveryID`s are redeliveries and always drop — a GitHub redelivery can't restart a job, and a stale worker's `Done()` can't clear a replacement's slot. Respond edits must carry a *new* delivery ID to supersede.
- A respond edit whose key already has a **completed** record drops outright — fixing a typo in "@elyoraa create the issues" can't create the issues a second time.
- Respond jobs skip `TryStart`'s same-PR cancel loop entirely — their `owner/repo/num` prKey would otherwise cancel running review/issue jobs. The reverse needs no rule: `respond/…` keys can never match the `owner/repo/num/` prefix.
- Dispatched `review`/`split`/`issue` jobs use the normal `JobKey` — they dedup/supersede like webhook jobs (a dispatched review supersedes a running auto-review or gate for the same SHA) — with a synthetic `DeliveryID`.
- `gate` jobs use the normal `JobKey` (one job per PR/SHA) — cancellation on new pushes comes free.

### Config additions

| Env | Default | Meaning |
|---|---|---|
| `MENTION_ROUTER` | `keywords` | `agent` enables the respond job; `keywords` keeps the deterministic path. Canary-able per deploy. |
| `RESPOND_MODEL` | `CRUSH_MODEL` | Cheaper/faster model for routing runs is allowed; routing is easy. |
| `RESPOND_TIMEOUT` | `REVIEW_TIMEOUT` | Whole-job timeout for respond runs. |
| `RESPOND_MAX_ACTIONS` | `4` | Belt on top of per-type rules; validated ≥1. |
| `SIZE_GATE_MODE` | `deterministic` | `agent` sends borderline-oversize PRs to a `gate` triage run instead of auto-splitting. |
| `GATE_MODEL` | `RESPOND_MODEL`, then `CRUSH_MODEL` | Triage is manifest-level — a cheap model suffices. |
| `GATE_TIMEOUT` | `5m` | Triage-pass budget only — the gate job itself gets `REVIEW_TIMEOUT`. |

Per-type values reach the machinery: `Process`'s timeout wrapper, `attemptTimeout()`, and `jobModel`/`crushPreflight` all select by job type.

### Failure modes & guardrails

- **Malformed/absent reply:** `validateRespondOutput` requires non-empty text; an `elyoraa-actions` block present but unparseable → transient retry. After final failure: post the stripped text reply if any, else a fallback pointing at the slash commands. Never silent.
- **Actions lost on parse failure:** the reply still posts, actions drop, user re-asks. Safer than half-executed actions.
- **`create_issues` without authorization or proposal:** enforced in Go — `mayCreateIssues` at the executor *and* inside `createIssues` itself, plus the outstanding-proposal check and the ≤10 cap. The prompt instructs too, but the code doesn't rely on it.
- **Superseded run acting anyway:** `processRespond`, `processIssue`, and `processOversize` re-check `ctx.Err()` after `runCrush` returns and before posting replies or executing actions — a job cancelled mid-run can't still comment or create issues.
- **Loop safety:** bot-authored comments are already filtered upstream; the bot's own reply can never re-trigger a respond job.
- **Token/cost bounds:** no diff in prompt, bounded thread (80 / 64 KB merged), worktree optional → respond is the cheapest run in the system.
- **Queue latency:** respond jobs share `MaxConcurrentReviews` workers (default 2) with long reviews — the snappy-reply goal stalls behind a 15-minute run. See open questions.
- **Durable queue:** respond jobs persist like others. `request_*` actions dedup by job key on retry; `create_issues` embeds a source marker in every issue it creates so a marker-search can dedup retries — the marker is written now, the search is a follow-up.

### Security

- No new GitHub App permissions — everything executes through existing client methods.
- The agent still can't write: side effects flow exclusively through the declared-action executor.
- `author_association` is enforced in Go for action-bearing mentions (`mayDispatch`/`mayCreateIssues`) — on public repos a random commenter can get a reply but cannot trigger reviews, splits, or issue creation.
- The worktree is read-only per the generated Crush job policy.
- Remaining exposure: prompt-injection can still steer *authorized* users' mentions toward wrong-but-permitted actions, and created-issue bodies carry quoted thread text — `createIssues` sanitizes `@mentions` out of issue bodies.

### Trade-offs (honest)

- **Latency:** one extra Crush run before a review starts on the mention path. Mitigated by the reply posting first — the conversation feels instant even though the review is async. Auto-review pays nothing.
- **Cost:** a small run per mention. `RESPOND_MODEL` can point at a cheaper model.
- **Non-determinism:** misrouting becomes an LLM error instead of a keyword miss. Contained by: tiny action vocabulary, thread + `PriorJobStatus` grounding, strict parse-and-retry, code-level authorization, per-action caps, every chosen action logged on `JobRecord.Actions`, slash commands as an escape hatch, and `MENTION_ROUTER=keywords` as an instant rollback.
- **Two routers to maintain** during transition — bounded by the config flag.

## Routing matrix (before → after, agent mode)

| Event | Before | After |
|---|---|---|
| PR comment mention | review job (any mention) or gated split | respond job → agent picks; `/review` `/split` `/accept` bypass |
| `pull_request_review` mention | same | same → respond |
| `pull_request_review_comment` mention | same | same → respond |
| Issue comment mention | `issue` job | respond → reply inline or `request_issue_analysis` |
| `issues` opened/edited mention | `issue` job | respond → same choice (`TriggerComment` stays empty so bot-authored issues are still dropped) |
| `pull_request` opened/reopened/ready | deterministic gate | `deterministic`: unchanged; `agent`: bands — ≤1× review, 1×–3× gate, >3×/truncated split |
| `@elyoraa create the issues` on shrunken PR | **silently reviewed (bug)** | respond → `create_issues` (if proposal visible + COLLABORATOR+), or `request_split`+`create` if stale; keywords mode requires a completed-split record |
| `@elyoraa why did you flag X?` | full review (wasteful) | reply, maybe `request_review` |

## Testing

Shipped:

- `classifyOversize` / gate ordering: table tests incl. the regression case (`"create the issues"` + under-budget estimate **with** a completed-split proposal → `split`+`SplitCreate`; without a proposal → falls through to normal sizing).
- `parseRespondActions`: `elyoraa-actions` fence only — ` ```json ` blocks are never protocol (mixed-fence case pins this); missing/malformed/empty/oversized/unknown-type cases.
- Dedup origin table: rank ordering, equal-rank-same-type drop, equal-rank-different-type supersede, delivery-ID fencing (redelivery drops), respond-edit supersession, gate supersession by dispatched jobs, respond↔review isolation.
- Authorization: `mayDispatch`/`mayCreateIssues` table; `createIssues` re-checks internally.
- Respond edit-after-completion drop via `enqueueReview`.
- Firewall: `TriggerComment` is absent from `review.md`/`reconcile.md` template data.
- Tracker: `Annotate` persists `RefinedLines`/`Actions`/`AttemptSeconds`/`Type` on memory and Bolt trackers; gate/split transitions update `JobRecord.Type`.
- Calibration: per-repo preference, `AttemptSeconds` over `DurationSeconds`, clamping, sample minimums.
- `.gitattributes` matcher: basename, slash-anchored, and `**` patterns.
- `gitattrMatcher`+`refinedEstimate`: linguist-generated files excluded from refined counts.

Still TODO:

- End-to-end replay fixture of nexus#486 (`split_suggested` record + under-budget re-measure + "create the issues" → issues created) — highest-value regression test; run whenever `respond.md` or `RESPOND_MODEL` changes.
- `Process`-level gate test (triage deadline → split fallback vs parent cancel → cancelled).
- Webhook-path tests in `agent` mode (mention → respond job).
- `MENTION_ROUTER=shadow` harness: run the respond agent alongside the keyword router, log declared actions, execute nothing — far stronger rollout evidence than a canary.
- `CreateReplyForReviewComment` — reply to inline mentions inside their review-comment thread instead of a top-level comment.
- `RESPOND_CONCURRENCY` — a dedicated respond worker pool (default 1) so replies can't stall behind 15-minute reviews.
- `createIssues` marker-search dedup — markers are written; searching them before creation survives database loss.

## Phases

- **Phase A — correctness:** the deterministic fixes. No behavior change except honoring explicit intent. **Shipped.**
- **Phase B — agent router:** respond job + prompt + executor + `MENTION_ROUTER=agent` behind the flag; default stays `keywords` until canary traffic looks clean. **Shipped.**
- **Phase B2 — agent size gate:** `gate` job + `prompts/gate.md` + `SIZE_GATE_MODE=agent` canary. **Shipped.**
- **Phase C — optional:** read-only MCP tools for the respond agent (`get_thread`, `get_pr`, `list_labels`); mem0 for conversational preferences; shadow mode + replay fixtures (see Testing TODO); drop `keywords` mode once `agent` is proven.

## Decisions locked

1. Router is a **job type**, not middleware — reuses queue/dedup/retry/timeout infra wholesale.
2. Actions are **declared in a dedicated `elyoraa-actions` fence**, executed by Go — no live write tools, no ambiguity with ordinary JSON in a reply.
3. Delegation is **enqueue, not spawn** — workers are separate Crush runs on the same queue; the bias firewall is the context contract, not a different model.
4. Determinism owns *nomination*, agents own *decisions* — the auto path keeps deterministic bands (≤1× review, 1×–3× gate, >3×/truncated split); the mention path delegates wholesale to `respond`.
5. `MENTION_ROUTER`/`SIZE_GATE_MODE` rollback flags stay until the agent modes are proven.
6. Respond runs get the worktree when available (question-answering needs code), but the prompt forbids emitting review verdicts.
7. Gate verdicts **transition** the job inline — no re-enqueue, no second pending comment; dedup slot and tracker record are updated on transition so nothing downstream sees a stale "gate" type.
8. Authorization and proposal-existence live **in Go**, never in the prompt — prompts steer, code decides.
9. Human intent is one field — `Origin` — ranked in one table in `canSupersede`, fenced by delivery IDs.

## Open questions

- Should the executor *force* `request_split`+`create` (delegation) when `Size` shows the PR changed a lot since the proposal? The `head_sha` staleness check would be stronger if proposals were stored as structured records (see below) — deferred.
- Proposals-as-data: a per-PR proposal record (`{head_sha, items, status}`) would make acceptance idempotent end-to-end and let Go render the proposal comment from stored items. Current implementation derives existence from the completed-split job record — correct but coarser. Worth it if edit/redelivery races still appear.
- ~~`MENTION_ROUTER`/`SIZE_GATE_MODE` per-repo override~~ — shipped: `.elyoraa.yml` on the default branch (`enabled`, `budget_minutes`, `lines_per_minute`, `ignored_paths`, `mention_router`, `size_gate_mode`, `skip_label`), read via `git show HEAD:` so PRs can't alter their own policy.
- Agent-gate nomination band: is 1×–3× the right range? Lowering the bottom catches subtle PRs; the >3× cap bounds triage cost.
- Should respond jobs get a dedicated worker/priority lane (`RESPOND_CONCURRENCY`)? They share `MAX_CONCURRENT_REVIEWS` workers with 10–15 min reviews, so a reply can sit behind two reviews — the "snappy" goal suffers under load.
- Ops deferred from the production-hardening pass: missed-webhook recovery (App deliveries API or open-PR reconciliation), Crush provider credentials + preflight-failure alerting, and `prometheus/client_golang` migration + queue-wait/rate-limit/alerting metrics. Shipped since: per-PR opt-out label (`PR_SKIP_LABEL`, per-repo `skip_label`), `elyoraa_reactions_total` reaction metrics, clone-on-demand with `GIT_BASE_URL`, `.elyoraa.yml` per-repo config, and `QUEUE_BACKEND=postgres` for multi-replica (queue claims via `FOR UPDATE SKIP LOCKED` + lease expiry; dedup claims + supersede flags in `job_claims`).

## Resolved in implementation

- **Reply posts before dispatch** — the reply lands immediately; dispatch outcomes (dedup-drop "already in flight", queue-full, created-issue links) are appended to the same comment.
- **Dispatched jobs re-fetch head SHA** — `dispatchJob` calls `GetPR`, refreshes `HeadSHA`/metadata, and skips with a note when the PR went draft/merged/closed since the mention.
- **`Origin` replaces `RouteLocked`** — supersession is one ranked table; `SplitCreate` remains for the split-create flow but carries no routing meaning.
- **`GateTimeout` is a phase budget** — the job gets `REVIEW_TIMEOUT`; triage runs under a 5m child context; triage-deadline → split fallback, parent-cancel → cancelled.
- **Proposals are checked, not assumed** — both keyword-acceptance and respond `create_issues` require an outstanding proposal (completed plain-`split` record or a visible proposal in the thread); `createIssues` re-verifies authorization at the choke point.
- **Mention cost controls shipped** — `MENTION_RATE_LIMIT`/`MENTION_RATE_WINDOW` cap mention-triggered jobs per author+repo (auto-reviews exempt); `DAILY_JOB_CAP_PER_REPO` bounds jobs started per repo per UTC day.
