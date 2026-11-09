package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
)

// conversationMax* bound how much of a PR/issue discussion is injected into
// the respond prompt. The merged conversation drops the oldest entries first.
const (
	conversationMaxComments = 80
	conversationMaxBytes    = 64 * 1024
)

// ConversationEntry is one item in a PR/issue discussion, merged from the
// three GitHub discussion streams (issue comments, reviews, inline review
// comments) so the respond agent sees one chronology.
type ConversationEntry struct {
	Author    string
	IsBot     bool
	Kind      string // "comment" | "review" | "inline"
	Path      string // inline only
	Line      int    // inline only
	State     string // review only (APPROVED, CHANGES_REQUESTED, …)
	InReplyTo int64  // inline only — parent comment ID when a reply
	Body      string
	CreatedAt time.Time
}

// fetchConversation merges the three GitHub discussion streams into one
// chronological, bounded thread. Failures per-stream are non-fatal — a
// partial thread is better than none.
func (r *ReviewUsecase) fetchConversation(ctx context.Context, j *domain.Review) []ConversationEntry {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CommentTimeout)
	defer cancel()

	var out []ConversationEntry
	skip := func(body string) bool { return strings.Contains(body, inProgressMarker) }

	comments, err := r.gh.ListIssueComments(cctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, 200, 256*1024)
	if err != nil {
		slog.Warn("conversation: list issue comments failed", "job", j.JobKey, "error", err)
	}
	for _, c := range comments {
		if skip(c.Body) {
			continue
		}
		out = append(out, ConversationEntry{Author: c.Author, IsBot: c.IsBot, Kind: "comment", Body: c.Body, CreatedAt: c.CreatedAt})
	}

	if j.IsPR() {
		if reviews, err := r.gh.ListReviewSummaries(cctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber); err != nil {
			slog.Warn("conversation: list reviews failed", "job", j.JobKey, "error", err)
		} else {
			for _, rv := range reviews {
				if skip(rv.Body) {
					continue
				}
				out = append(out, ConversationEntry{Author: rv.Author, IsBot: rv.IsBot, Kind: "review", State: rv.State, Body: rv.Body, CreatedAt: rv.SubmittedAt})
			}
		}
		if inline, err := r.gh.ListPullReviewComments(cctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber); err != nil {
			slog.Warn("conversation: list review comments failed", "job", j.JobKey, "error", err)
		} else {
			for _, cm := range inline {
				if skip(cm.Body) {
					continue
				}
				out = append(out, ConversationEntry{Author: cm.Author, IsBot: cm.IsBot, Kind: "inline", Path: cm.Path, Line: cm.Line, InReplyTo: cm.InReplyTo, Body: cm.Body, CreatedAt: cm.CreatedAt})
			}
		}
	}

	sort.SliceStable(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })

	// Bound: drop the oldest entries first — the trigger is always the latest.
	for len(out) > conversationMaxComments {
		out = out[1:]
	}
	for conversationBytes(out) > conversationMaxBytes && len(out) > 1 {
		out = out[1:]
	}
	return out
}

func conversationBytes(entries []ConversationEntry) int {
	n := 0
	for _, e := range entries {
		n += len(e.Body)
	}
	return n
}

// actionsBlockRegex matches the dedicated protocol fence. Only
// ```elyoraa-actions blocks are protocol — ordinary ```json blocks in a
// reply are illustrative text and are never stripped or parsed.
var actionsBlockRegex = regexp.MustCompile("(?s)```elyoraa-actions\\s*\n(.*?)\\s*```")

// respondAction is one declared action in a respond job's actions block.
type respondAction struct {
	Type   string                 `json:"type"`
	Create bool                   `json:"create,omitempty"`
	Issues []domain.ProposedIssue `json:"issues,omitempty"`
}

// respondActions is the trailing actions block of a respond reply.
type respondActions struct {
	Actions []respondAction `json:"actions"`
}

// parseRespondActions extracts the protocol block from a respond reply. The
// dedicated ```elyoraa-actions fence is the ONLY protocol — a present but
// unparseable one is an error (the run is retried). Ordinary ```json blocks
// are illustrative text and are never parsed, stripped, or executed — an
// example payload that happens to contain an "actions" key can never fire.
func parseRespondActions(out string) (respondActions, string, error) {
	var acts respondActions
	matches := actionsBlockRegex.FindAllStringSubmatchIndex(out, -1)
	if len(matches) == 0 {
		return acts, out, nil
	}
	last := matches[len(matches)-1]
	if err := json.Unmarshal([]byte(out[last[2]:last[3]]), &acts); err != nil {
		return respondActions{}, out, fmt.Errorf("unparseable actions block: %w", err)
	}
	return acts, strings.TrimSpace(out[:last[0]] + out[last[1]:]), nil
}

// validateRespondOutput rejects runs whose declared action block can't be
// parsed — a malformed block probably truncated a real action — and runs
// with no visible reply text at all.
func validateRespondOutput(out string) error {
	_, text, err := parseRespondActions(out)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty reply text")
	}
	return nil
}

// respondSize is the size-estimate context shown to the respond agent.
type respondSize struct {
	EstMinutes int
	Lines      int
	Budget     int
	Oversize   bool
}

func (r *ReviewUsecase) respondSize(j *domain.Review) respondSize {
	budget := jobBudget(r.cfg, *j)
	return respondSize{
		EstMinutes: j.SizeEstMinutes,
		Lines:      j.SizeLines,
		Budget:     budget,
		// Recorded at enqueue: covers the truncated/MAX_CHANGED_FILES case
		// tooBig knows about, not just the minute estimate.
		Oversize: j.SizeOversize || (budget > 0 && j.SizeEstMinutes > budget),
	}
}

// priorJobStatus returns the most recent non-respond job record for this
// PR/issue, so the respond agent knows e.g. a split proposal is outstanding.
// Records are most-recently-updated first.
func (r *ReviewUsecase) priorJobStatus(ctx context.Context, j *domain.Review) *queue.JobRecord {
	if r.tracker == nil {
		return nil
	}
	for _, rec := range r.tracker.List(ctx) {
		if rec.Owner == j.Owner && rec.Repo == j.Repo && rec.PRNumber == j.PRNumber &&
			rec.Type != "respond" && rec.JobKey != j.JobKey {
			return &rec
		}
	}
	return nil
}

func (r *ReviewUsecase) renderRespondPrompt(j *domain.Review, convo []ConversationEntry, prior *queue.JobRecord) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "respond.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"Owner":          j.Owner,
		"Repo":           j.Repo,
		"RepoKey":        fmt.Sprintf("%s/%s", j.Owner, j.Repo),
		"PRNumber":       j.PRNumber,
		"IsPR":           j.IsPR(),
		"Title":          j.Title,
		"Body":           j.Body,
		"Author":         j.Author,
		"HeadSHA":        j.HeadSHA,
		"BaseSHA":        j.BaseSHA,
		"BotHandle":      r.cfg.BotHandle,
		"TriggerComment": j.TriggerComment,
		"Conversation":   convo,
		"RepoAvailable":  j.RepoAvailable,
		"SyncCmd":        r.syncCmd(),
		"Size":           r.respondSize(j),
		"PriorJob":       prior,
		"MaxIssues":      maxCreatedIssues,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// processRespond handles a mention-routing job: read the discussion, let
// Crush decide what the human wants, post the reply, then execute the
// declared actions. Actions run after the reply posts so the human sees a
// fast answer first.
func (r *ReviewUsecase) processRespond(ctx context.Context, j *domain.Review) (attempts int, err error) {
	attempts = 1

	if j.PendingCommentID != 0 {
		// Flip the enqueue-time "queued" comment to the running state.
		r.updatePending(ctx, j, pendingText(j, jobBudget(r.cfg, *j)))
	} else {
		pendingCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		id, perr := r.gh.PostPendingComment(pendingCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber,
			pendingBody(j, jobBudget(r.cfg, *j)))
		cancel()
		if perr == nil {
			j.PendingCommentID = id
		}
	}

	// Optional worktree: PRs check out the head commit, issues use HEAD —
	// same shape as processIssue.
	workdir := ""
	var cleanup func()
	token, tokenErr := r.gh.InstallationToken(ctx, j.InstallationID)
	if tokenErr == nil {
		if fetchErr := r.cache.Fetch(ctx, j.Owner, j.Repo, token); fetchErr == nil {
			ref := j.HeadSHA
			if ref == "" {
				ref = "HEAD"
			}
			wt, cl, wtErr := r.cache.AddWorktree(ctx, j.Owner, j.Repo, j.PRNumber, ref)
			if wtErr == nil {
				workdir, cleanup = wt, cl
				j.RepoAvailable = true
			}
		}
	}
	if workdir == "" {
		tmpDir, err := os.MkdirTemp("", "elyoraa-respond-*")
		if err != nil {
			r.postRespondReply(ctx, j, "I couldn't process that — internal error.")
			r.markFailed(ctx, *j, "mkdir temp")
			return attempts, fmt.Errorf("mkdir temp: %w", err)
		}
		workdir = tmpDir
		cleanup = func() { os.RemoveAll(tmpDir) }
	}
	defer cleanup()

	convo := r.fetchConversation(ctx, j)
	prior := r.priorJobStatus(ctx, j)

	prompt, err := r.renderRespondPrompt(j, convo, prior)
	if err != nil {
		r.postRespondReply(ctx, j, "I couldn't process that — internal error.")
		r.markFailed(ctx, *j, "render respond prompt")
		return attempts, fmt.Errorf("render respond prompt: %w", err)
	}

	out, attempts, _, err := r.runCrush(ctx, j, workdir, prompt, validateRespondOutput)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), j, "Mention cancelled.")
			r.markStatus(ctx, *j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.postRespondReply(ctx, j,
			"I couldn't process that — try `review`, `split`, or `create the issues` and I'll act on it directly.")
		r.markFailed(ctx, *j, "run crush respond")
		return attempts, fmt.Errorf("run crush respond: %w", err)
	}

	// A run superseded after runCrush returned must not post or act — the
	// newer mention owns the conversation now.
	if ctx.Err() != nil {
		r.updatePending(context.Background(), j, "Mention cancelled.")
		r.markStatus(ctx, *j, "cancelled")
		return attempts, queue.ErrJobCancelled
	}

	acts, reply, perr := parseRespondActions(out)
	if perr != nil || strings.TrimSpace(reply) == "" {
		// validateRespondOutput already passed, so this shouldn't happen —
		// degrade to a safe visible reply rather than posting nothing.
		reply = "I couldn't process that — try `/review`, `/split`, or `/accept`."
	}

	r.postRespondReply(ctx, j, reply)

	if ctx.Err() != nil {
		r.updatePending(context.Background(), j, "Mention cancelled.")
		r.markStatus(ctx, *j, "cancelled")
		return attempts, queue.ErrJobCancelled
	}
	r.executeRespondActions(ctx, j, acts, reply)

	r.markStatus(ctx, *j, "completed")
	return attempts, nil
}

// postRespondReply publishes the final reply — no in-progress marker, so the
// reply stays visible to later conversation fetches.
func (r *ReviewUsecase) postRespondReply(ctx context.Context, j *domain.Review, body string) {
	if j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, body)
		cancel()
		return
	}
	postCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
	id, err := r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, body)
	cancel()
	if err == nil {
		j.PendingCommentID = id
	}
}

// executeRespondActions runs the declared actions against the fixed dispatch
// table. Dispatch results and created-issue links are appended to the reply;
// the chosen action types are annotated onto the job record for audit.
func (r *ReviewUsecase) executeRespondActions(ctx context.Context, j *domain.Review, acts respondActions, reply string) {
	if len(acts.Actions) == 0 {
		return
	}
	if len(acts.Actions) > r.cfg.RespondMaxActions {
		slog.Warn("respond: too many declared actions, truncating", "job", j.JobKey, "count", len(acts.Actions))
		acts.Actions = acts.Actions[:r.cfg.RespondMaxActions]
	}

	var names, notes []string
	dispatched := false // request_review/request_split are mutually exclusive

	for _, a := range acts.Actions {
		if r.metrics != nil {
			r.metrics.IncRespondAction(a.Type)
		}
		switch a.Type {
		case "request_review", "request_split":
			if !j.IsPR() {
				notes = append(notes, fmt.Sprintf("\n- `%s` skipped — that only applies to pull requests.", a.Type))
				continue
			}
			if !mayDispatch(j) {
				notes = append(notes, fmt.Sprintf("\n- `%s` skipped — only repository collaborators or the PR author can request work.", a.Type))
				continue
			}
			// create:true accepts an existing-but-stale proposal — it
			// requires the same proposal-existence + permission checks as
			// the keyword acceptance path. Without a proposal the action
			// degrades to a fresh split proposal.
			if a.Type == "request_split" && a.Create {
				if !mayCreateIssues(j.TriggerAssociation) {
					notes = append(notes, "\n- `request_split` issue creation skipped — only repository collaborators can create issues.")
					a.Create = false
				} else if !splitProposalOutstanding(r.tracker, ctx, j) {
					slog.Info("respond: split-create without outstanding proposal, demoting to propose", "job", j.JobKey)
					a.Create = false
				}
			}
			if dispatched {
				slog.Info("respond: dropping extra dispatch action", "job", j.JobKey, "type", a.Type)
				continue
			}
			dispatched = true
			names = append(names, a.Type)
			if note := r.dispatchJob(ctx, j, a); note != "" {
				notes = append(notes, note)
			}
		case "request_issue_analysis":
			if j.IsPR() {
				notes = append(notes, "\n- `request_issue_analysis` skipped — that only applies to issues.")
				continue
			}
			if !mayDispatch(j) {
				notes = append(notes, "\n- `request_issue_analysis` skipped — only repository collaborators or the issue author can request work.")
				continue
			}
			names = append(names, a.Type)
			if note := r.dispatchJob(ctx, j, a); note != "" {
				notes = append(notes, note)
			}
		case "create_issues":
			if len(a.Issues) == 0 {
				slog.Warn("respond: create_issues with empty issues list", "job", j.JobKey)
				continue
			}
			if !mayCreateIssues(j.TriggerAssociation) {
				notes = append(notes, "\n- `create_issues` skipped — only repository collaborators can create issues.")
				continue
			}
			// Issues are only created in acceptance of something the bot
			// actually proposed. On a PR that means a split-proposal record;
			// on an issue, a completed analysis/respond reply that contained
			// the breakdown.
			if j.IsPR() {
				if !splitProposalOutstanding(r.tracker, ctx, j) {
					notes = append(notes, "\n- `create_issues` skipped — no split proposal is on record for this PR.")
					continue
				}
			} else if !priorBotReply(r.tracker, ctx, j) {
				notes = append(notes, "\n- `create_issues` skipped — there's no earlier proposal from me on this issue.")
				continue
			}
			names = append(names, a.Type)
			if created := r.createIssues(ctx, j, a.Issues); len(created) > 0 {
				var sb strings.Builder
				sb.WriteString("\n\n**Created issues:**\n")
				for _, ci := range created {
					sb.WriteString(fmt.Sprintf("- %s — %s\n", ci.URL, ci.Title))
				}
				notes = append(notes, sb.String())
			}
		default:
			slog.Warn("respond: ignoring unknown action", "job", j.JobKey, "type", a.Type)
		}
	}

	if len(notes) > 0 && j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, reply+strings.Join(notes, ""))
		cancel()
	}

	if len(names) > 0 {
		if r.tracker != nil {
			r.tracker.Annotate(ctx, j.JobKey, queue.JobAnnotation{Actions: names})
		}
		slog.Info("respond: actions executed", "job", j.JobKey, "actions", names)
	}
}

// dispatchJob enqueues a follow-up job declared by the respond agent. The new
// job uses the normal job keyspace — it dedups/supersedes exactly like a
// webhook-enqueued job — with a synthetic delivery ID so it doesn't clobber
// the respond job's webhook record or confuse the BindCancel takeover check
// (which needs both IDs non-empty). Returns a user-facing note on skip.
func (r *ReviewUsecase) dispatchJob(ctx context.Context, parent *domain.Review, a respondAction) string {
	if r.q == nil {
		return fmt.Sprintf("\n- `%s` skipped — dispatch is not available on this server.", a.Type)
	}

	job := domain.Review{
		Owner:              parent.Owner,
		Repo:               parent.Repo,
		PRNumber:           parent.PRNumber,
		InstallationID:     parent.InstallationID,
		Author:             parent.Author,
		Title:              parent.Title,
		Body:               parent.Body,
		HeadSHA:            parent.HeadSHA,
		BaseSHA:            parent.BaseSHA,
		TriggerComment:     parent.TriggerComment,
		TriggerAuthor:      parent.TriggerAuthor,
		TriggerAssociation: parent.TriggerAssociation,
		Origin:             domain.OriginDispatched,
		SizeEstMinutes:     parent.SizeEstMinutes,
		SizeLines:          parent.SizeLines,
		EnqueuedAt:         time.Now(),
	}
	switch a.Type {
	case "request_review":
		job.Type = "review"
	case "request_split":
		job.Type = "split"
		job.SplitCreate = a.Create
		if a.Create {
			// An agent-declared create IS the human's acceptance — rank it so
			// it supersedes the outstanding proposal and resists demotion.
			job.Origin = domain.OriginAccepted
		}
	case "request_issue_analysis":
		job.Type = "issue"
		job.HeadSHA, job.BaseSHA = "", ""
	default:
		return ""
	}
	job.DeliveryID = parent.DeliveryID + ":dispatch:" + a.Type
	job.ParentKey = parent.JobKey
	job.JobKey = dedup.JobKey(job.Owner, job.Repo, job.PRNumber, job.HeadSHA)

	// Refresh PR state: review the current head, and don't enqueue for a PR
	// that went draft/merged/closed since the mention.
	if job.Type != "issue" {
		pctx, cancel := context.WithTimeout(ctx, r.cfg.CommentTimeout)
		info, err := r.gh.GetPR(pctx, parent.InstallationID, parent.Owner, parent.Repo, parent.PRNumber, 0, 0)
		cancel()
		if err == nil {
			if info.Merged || info.Closed {
				return fmt.Sprintf("\n- `%s` skipped — the PR is now closed/merged.", a.Type)
			}
			if info.IsDraft {
				return fmt.Sprintf("\n- `%s` skipped — the PR is currently a draft.", a.Type)
			}
			job.HeadSHA, job.BaseSHA = info.HeadSHA, info.BaseSHA
			job.Title, job.Body, job.Author = info.Title, info.Body, info.Author
			job.JobKey = dedup.JobKey(job.Owner, job.Repo, job.PRNumber, job.HeadSHA)
		} else {
			slog.Warn("respond: PR refresh failed, dispatching with stale SHA", "job", parent.JobKey, "error", err)
		}
	}

	ctx2, started, ok := r.d.TryStart(ctx, job)
	if !ok {
		return fmt.Sprintf("\n- a `%s` job is already in flight for this %s.", a.Type, prOrIssue(job.IsPR()))
	}

	if started.PendingCommentID == 0 {
		pendingCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		id, err := r.gh.PostPendingComment(pendingCtx, job.InstallationID, job.Owner, job.Repo, job.PRNumber,
			queuedBody(&job, jobBudget(r.cfg, job)))
		cancel()
		if err == nil {
			started.PendingCommentID = id
		}
	}

	if !r.q.Submit(ctx2, *started) {
		r.d.Done(*started)
		return fmt.Sprintf("\n- `%s` couldn't be queued — try again in a bit.", a.Type)
	}

	slog.Info("respond: dispatched job", "parent", parent.JobKey, "job", job.JobKey, "type", job.Type,
		"split_create", job.SplitCreate, "sha", job.HeadSHA)
	return ""
}

func prOrIssue(isPR bool) string {
	if isPR {
		return "PR"
	}
	return "issue"
}
