package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
	"github.com/andikapradanaarif/elyoraa/internal/repository"
	"github.com/andikapradanaarif/elyoraa/internal/repository/cache"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
)

// inProgressMarker marks a comment as a transient bot status. Final replies
// must not carry it — thread fetches filter it out, so a marked reply would
// be invisible to later runs.
const inProgressMarker = "<!-- elyoraa-in-progress -->"

// queueFullBody is the terminal message left on a pending comment when a
// job can't be queued at all — marked transient so conversation fetches
// skip it like any other status comment.
const queueFullBody = "Couldn't queue this run — the queue is full. Re-mention me to try again.\n\n" + inProgressMarker

// pendingText returns the running-state status line for a job type.
func pendingText(j *domain.Review, budgetMinutes int) string {
	switch {
	case j.IsRespond():
		return "On it — reading the thread…"
	case j.IsGate():
		return "Sizing up this PR…"
	case j.IsSplit():
		return fmt.Sprintf(
			"This PR looks too big for a single review (estimated ~%d min vs the %d min budget). Drafting a split proposal…",
			j.SizeEstMinutes, budgetMinutes,
		)
	case j.IsIssue():
		return "Analyzing issue…"
	default:
		return "Review in progress…"
	}
}

// pendingBody returns the transient "in progress" comment for a job type.
func pendingBody(j *domain.Review, budgetMinutes int) string {
	return pendingText(j, budgetMinutes) + "\n\n" + inProgressMarker
}

// queuedBody returns the transient comment posted at enqueue. The job is
// accepted but may wait behind running work, so the comment says "queued",
// not "in progress" — Process flips it to pendingText on pickup.
func queuedBody(j *domain.Review, budgetMinutes int) string {
	switch {
	case j.IsRespond():
		return "On it — queued behind other work, I'll read the thread when a worker frees up…\n\n" + inProgressMarker
	case j.IsGate():
		return "Queued — I'll size up this PR when a worker frees up…\n\n" + inProgressMarker
	case j.IsSplit():
		return fmt.Sprintf(
			"This PR looks too big for a single review (estimated ~%d min vs the %d min budget). Split proposal queued — I'll start when a worker frees up…\n\n%s",
			j.SizeEstMinutes, budgetMinutes, inProgressMarker,
		)
	case j.IsIssue():
		return "Issue analysis queued — I'll start when a worker frees up…\n\n" + inProgressMarker
	default:
		return "Review queued — I'll start when a worker frees up…\n\n" + inProgressMarker
	}
}

// ReviewUsecase orchestrates a single PR review.
type ReviewUsecase struct {
	cfg     *config.Config
	cache   *cache.Cache
	gh      *github.Client
	d       dedup.Interface
	store   repository.WebhookStore
	metrics *metrics.Registry
	q       *queue.Queue
	tracker queue.JobTracker
}

// NewReviewUsecase creates a ReviewUsecase.
func NewReviewUsecase(cfg *config.Config, c *cache.Cache, g *github.Client, d dedup.Interface, s repository.WebhookStore, m *metrics.Registry) *ReviewUsecase {
	return &ReviewUsecase{cfg: cfg, cache: c, gh: g, d: d, store: s, metrics: m}
}

// SetQueue injects the job queue after construction — the queue needs the
// usecase as its processor, so it can't arrive via NewReviewUsecase. Gives
// respond-job dispatch (q.Submit) and prior-status lookup (q.Tracker).
func (r *ReviewUsecase) SetQueue(q *queue.Queue) {
	r.q = q
	r.tracker = q.Tracker()
}

// Process implements queue.Processor.
func (r *ReviewUsecase) Process(ctx context.Context, j domain.Review) (attempts int, err error) {
	attempts = 1
	if r.store != nil && j.DeliveryID != "" {
		_ = r.store.UpdateStatus(ctx, j.DeliveryID, "processing", "")
	}

	start := time.Now()
	defer func() {
		r.d.Done(j)
		if r.metrics != nil {
			r.metrics.RecordDuration(time.Since(start))
		}
		if p := recover(); p != nil {
			r.updatePending(ctx, &j, "Review failed due to an internal error.")
			if r.store != nil && j.DeliveryID != "" {
				_ = r.store.UpdateStatus(ctx, j.DeliveryID, "failed", fmt.Sprintf("panic: %v", p))
			}
			slog.Error("review panic", "job", j.JobKey, "panic", p)
			panic(p)
		}
	}()

	if r.metrics != nil {
		r.metrics.IncReviewTotal()
	}

	// Bind a cancel func to this job's dedup slot so a newer push for the same
	// PR actually terminates this run (including any backoff sleep).
	jobCtx, jobCancel := context.WithCancel(ctx)
	defer jobCancel()
	if !r.d.BindCancel(j, jobCancel) {
		r.markStatus(ctx, j, "cancelled")
		return attempts, queue.ErrJobCancelled
	}

	ctx, cancel := context.WithTimeout(jobCtx, r.jobTimeout(&j))
	defer cancel()

	if j.IsIssue() {
		return r.processIssue(ctx, &j)
	}
	if j.IsRespond() {
		return r.processRespond(ctx, &j)
	}

	// A worker has the job — flip the enqueue-time "queued" comment to the
	// running state so the PR reflects that work has actually started.
	r.updatePending(ctx, &j, pendingText(&j, jobBudget(r.cfg, j)))

	token, err := r.gh.InstallationToken(ctx, j.InstallationID)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not authenticate to GitHub.")
		r.markFailed(ctx, j, "installation token")
		return attempts, fmt.Errorf("installation token: %w", err)
	}

	if err := r.cache.Fetch(ctx, j.Owner, j.Repo, token); err != nil {
		if errors.Is(err, cache.ErrRepoNotCloned) {
			// Repo unregistered after enqueue — audit/log only, nothing
			// posts to GitHub. The pending comment is deleted so no stale
			// "in progress" is left behind.
			if j.PendingCommentID != 0 {
				delCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
				_ = r.gh.DeleteComment(delCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID)
				cancel()
			}
			if r.store != nil && j.DeliveryID != "" {
				_ = r.store.UpdateStatus(ctx, j.DeliveryID, "skipped", "repo not registered")
			}
			slog.Info("skipping job: repo not registered",
				"job", j.JobKey, "repo", j.Owner+"/"+j.Repo)
			return attempts, nil
		}
		r.updatePending(ctx, &j, "Review failed: could not fetch repository.")
		r.markFailed(ctx, j, "cache fetch")
		return attempts, fmt.Errorf("cache fetch: %w", err)
	}

	worktree, cleanup, err := r.cache.AddWorktree(ctx, j.Owner, j.Repo, j.PRNumber, j.HeadSHA)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not prepare worktree.")
		r.markFailed(ctx, j, "add worktree")
		return attempts, fmt.Errorf("add worktree: %w", err)
	}
	defer cleanup()

	prInfo, err := r.gh.GetPR(ctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, r.cfg.MaxPatchBytes, r.cfg.MaxChangedFiles)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not fetch PR details.")
		r.markFailed(ctx, j, "get pr")
		return attempts, fmt.Errorf("get pr: %w", err)
	}

	if prInfo.IsDraft {
		r.updatePending(ctx, &j, "Review skipped: draft PR.")
		r.markStatus(ctx, j, "cancelled")
		return attempts, nil
	}

	// Record the refined (generated-excluded) line count on the job record so
	// rate calibration isn't fed by webhook counters that include generated
	// files — the enqueue-time counters can't see paths, only this per-file
	// fetch can. Skipped when the file list is truncated: the partial list
	// undercounts, so the payload estimate is the better number.
	// Generated-file detection upgrades to the repo's own signal when the
	// worktree is available: .gitattributes linguist-generated/vendored marks
	// catch generated files the path heuristics miss.
	gen := isGeneratedPath
	if len(j.IgnoredPaths) > 0 {
		ignored := j.IgnoredPaths
		base := gen
		gen = func(p string) bool { return base(p) || ignoredPath(ignored, p) }
	}
	if data, err := os.ReadFile(filepath.Join(worktree, ".gitattributes")); err == nil {
		if m := gitattrMatcher(data); m != nil {
			prev := gen
			gen = func(p string) bool { return prev(p) || m(p) }
		}
	}

	if r.tracker != nil && j.SizeLines > 0 && !prInfo.FilesTruncated {
		if s := estimatePRSizeWith(prInfo, r.cfg.PRLinesPerMinute, gen); s.Additions+s.Deletions > 0 && s.Additions+s.Deletions != j.SizeLines {
			r.tracker.Annotate(ctx, j.JobKey, queue.JobAnnotation{RefinedLines: s.Additions + s.Deletions})
		}
	}

	// Gate jobs run their triage pass now: the agent judges review-vs-split
	// and the job transitions inline. A cancelled triage is a cancellation,
	// not a fallback — don't convert it into a split.
	if j.IsGate() {
		verdict, err := r.triageGate(ctx, &j, worktree, prInfo, gen)
		if err != nil {
			r.updatePending(context.Background(), &j, "Sizing cancelled: superseded.")
			r.markStatus(ctx, j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		if verdict == "split" {
			j.Type = "split"
			r.d.UpdateJob(j.JobKey, func(sj *domain.Review) { sj.Type = "split" })
			r.annotatePhase(ctx, j.JobKey, "split")
			r.updatePending(ctx, &j, pendingText(&j, jobBudget(r.cfg, j)))
			return r.processOversize(ctx, &j, worktree, prInfo, refinedEstimate(prInfo, &j, r.cfg.PRLinesPerMinute, gen))
		}
		j.Type = "review"
		r.d.UpdateJob(j.JobKey, func(sj *domain.Review) { sj.Type = "review" })
		r.annotatePhase(ctx, j.JobKey, "review")
		r.updatePending(ctx, &j, "Review in progress…")
	}

	// Split jobs were gated at enqueue from webhook counters, which include
	// generated files. Re-verify with per-file stats: if the refined estimate
	// fits the budget, run the normal review after all. The inverse never
	// happens — per-file estimation only removes lines, so a job enqueued as
	// a review can't secretly be oversize. Explicit intent is never demoted:
	// dispatched/accepted jobs reach processOversize regardless of the
	// refined number (SplitCreate kept for records predating Origin).
	if j.IsSplit() {
		size := refinedEstimate(prInfo, &j, r.cfg.PRLinesPerMinute, gen)
		if size.tooBig(jobBudget(r.cfg, j)) || j.SplitCreate || j.HasExplicitIntent() {
			return r.processOversize(ctx, &j, worktree, prInfo, size)
		}
		slog.Info("split job demoted to review: refined estimate under budget",
			"job", j.JobKey, "files", size.Files, "est_minutes", size.EstMinutes)
		j.Type = "review"
		r.d.UpdateJob(j.JobKey, func(sj *domain.Review) { sj.Type = "review" })
		r.annotatePhase(ctx, j.JobKey, "review")
		r.updatePending(ctx, &j, "Review in progress…")
	}

	// Fetch the previous bot review now — a separate reconcile pass will
	// re-verify those findings AFTER the unbiased fresh review completes.
	prevReview := r.fetchPreviousReview(ctx, &j)

	prompt, err := r.renderPrompt(&j, prInfo)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not render prompt.")
		r.markFailed(ctx, j, "render prompt")
		return attempts, fmt.Errorf("render prompt: %w", err)
	}

	out, attempts, lastDur, err := r.runCrush(ctx, &j, worktree, prompt, validateReviewOutput)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), &j, "Review cancelled: a newer push replaced this one.")
			r.markStatus(ctx, j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.updatePending(ctx, &j, "Review failed: Crush review run failed.")
		r.markFailed(ctx, j, "run crush")
		return attempts, fmt.Errorf("run crush: %w", err)
	}
	if r.tracker != nil && lastDur > 0 {
		r.tracker.Annotate(ctx, j.JobKey, queue.JobAnnotation{AttemptSeconds: int64(lastDur.Seconds())})
	}

	review, err := parseReviewOutput(out)
	if err != nil {
		// Should not happen — runCrush validates the output — but never
		// approve on a parse failure.
		r.updatePending(ctx, &j, "Review failed: could not parse Crush output.")
		r.markFailed(ctx, j, "parse output")
		return attempts, fmt.Errorf("parse output: %w", err)
	}

	// Second pass: reconcile previous-review findings against the fresh
	// review. Merges still-present findings the fresh review missed and
	// records each prior finding's status.
	r.reconcileFindings(ctx, &j, worktree, prevReview, review)

	// Map comments to lines. Drop unmappable comments into the summary.
	fileComments := r.gh.MapCommentsToPositions(prInfo.Files, review.Comments)
	dropped := make([]domain.Comment, 0)
	used := make(map[string]bool)
	for _, c := range review.Comments {
		k := fmt.Sprintf("%s:%d", c.Path, c.Line)
		for _, fc := range fileComments {
			if fmt.Sprintf("%s:%d", fc.Path, fc.Line) == k {
				used[k] = true
				break
			}
		}
		if !used[k] {
			dropped = append(dropped, c)
		}
	}

	summary := review.Summary
	{
		var sb strings.Builder
		sb.WriteString(summary)
		if len(review.PassesCompleted) > 0 {
			sb.WriteString(fmt.Sprintf("\n\n_Review passes completed: %s_", strings.Join(review.PassesCompleted, ", ")))
		}
		if len(review.FilesReviewed) > 0 {
			sb.WriteString(fmt.Sprintf("\n_Files reviewed: %d_", len(review.FilesReviewed)))
		}
		if review.HighSeveritySweep != "" {
			sb.WriteString(fmt.Sprintf("\n_High-severity sweep: %s_", review.HighSeveritySweep))
		}
		if len(review.PriorFindings) > 0 {
			var fixed, present, unclear int
			for _, pf := range review.PriorFindings {
				switch pf.Status {
				case "fixed":
					fixed++
				case "still_present":
					present++
				default:
					unclear++
				}
			}
			sb.WriteString(fmt.Sprintf("\n_Previous findings re-check: %d fixed, %d still present, %d unclear_", fixed, present, unclear))
		}
		summary = sb.String()
	}
	if len(dropped) > 0 {
		var sb strings.Builder
		sb.WriteString(summary)
		sb.WriteString("\n\n### Comments that could not be mapped to changed lines\n")
		for _, c := range dropped {
			sb.WriteString(fmt.Sprintf("- **%s** `%s:%d`: %s\n", strings.ToUpper(c.Severity), c.Path, c.Line, c.Body))
		}
		summary = sb.String()
	}

	if len(review.FollowUps) > 0 {
		var sb strings.Builder
		sb.WriteString(summary)
		sb.WriteString("\n\n### Suggested follow-up issues (out of scope for this PR)\n")
		for _, f := range review.FollowUps {
			sb.WriteString(fmt.Sprintf("- **%s** — %s\n", f.Title, f.Description))
		}
		summary = sb.String()
	}

	state := "APPROVE"
	if len(review.Comments) > 0 {
		state = "REQUEST_CHANGES"
	}

	if err := r.gh.PostReview(ctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, summary, fileComments, j.HeadSHA, state); err != nil {
		// GitHub rejects approving your own PR. If the PR was authored by a bot
		// (e.g. elyoraa[bot]/Crush), fall back to posting a comment instead.
		if j.IsBot() && state == "APPROVE" {
			slog.Info("cannot approve own PR, posting comment instead", "author", j.Author)
			commentBody := "✅ **Review complete — no issues found.**\n\n" + summary
			if j.PendingCommentID != 0 {
				editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
				_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, commentBody)
				cancel()
			} else {
				postCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
				_, _ = r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, commentBody)
				cancel()
			}
			r.markStatus(ctx, j, "completed")
			return attempts, nil
		}
		r.updatePending(ctx, &j, "Review failed: could not post review comments.")
		r.markFailed(ctx, j, "post review")
		return attempts, fmt.Errorf("post review: %w", err)
	}

	if j.PendingCommentID != 0 {
		_ = r.gh.DeleteComment(ctx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID)
	}

	r.markStatus(ctx, j, "completed")
	return attempts, nil
}

func (r *ReviewUsecase) markFailed(ctx context.Context, j domain.Review, msg string) {
	if r.store != nil && j.DeliveryID != "" {
		_ = r.store.UpdateStatus(ctx, j.DeliveryID, "failed", msg)
	}
}

// processIssue handles an issue analysis job: spawn Crush to analyze the issue
// and post the response as a comment. If the repo is cloned on the server,
// Crush gets a worktree so it can search the code. If not, Crush analyzes
// from the issue text alone and mentions that the repo isn't available.
func (r *ReviewUsecase) processIssue(ctx context.Context, j *domain.Review) (attempts int, err error) {
	attempts = 1
	body := pendingBody(j, jobBudget(r.cfg, *j))
	if j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, body)
		cancel()
	} else {
		pendingCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		commentID, err := r.gh.PostPendingComment(pendingCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, body)
		cancel()
		if err != nil {
			slog.Warn("failed to post pending comment", "error", err)
		} else {
			j.PendingCommentID = commentID
		}
	}

	// Try to get a worktree so Crush can search the repo code.
	// If the repo isn't cloned, fall back to a temp dir and tell Crush.
	workdir := ""
	var cleanup func()
	repoAvailable := false

	token, tokenErr := r.gh.InstallationToken(ctx, j.InstallationID)
	if tokenErr == nil {
		if fetchErr := r.cache.Fetch(ctx, j.Owner, j.Repo, token); fetchErr == nil {
			// Use the default branch HEAD as the worktree base.
			wt, cl, wtErr := r.cache.AddWorktree(ctx, j.Owner, j.Repo, j.PRNumber, "HEAD")
			if wtErr == nil {
				workdir = wt
				cleanup = cl
				repoAvailable = true
			}
		}
	}

	if workdir == "" {
		tmpDir, err := os.MkdirTemp("", "elyoraa-issue-*")
		if err != nil {
			r.updatePending(ctx, j, "Issue analysis failed: internal error.")
			r.markFailed(ctx, *j, "mkdir temp")
			return attempts, fmt.Errorf("mkdir temp: %w", err)
		}
		workdir = tmpDir
		cleanup = func() { os.RemoveAll(tmpDir) }
	}
	defer cleanup()

	j.RepoAvailable = repoAvailable

	// Fetch the discussion so the analysis can follow prior discussion:
	// previous bot replies, human acceptance of a proposal, open questions.
	thread := r.fetchConversation(ctx, j)

	prompt, err := r.renderIssuePrompt(j, thread)
	if err != nil {
		r.updatePending(ctx, j, "Issue analysis failed: could not render prompt.")
		r.markFailed(ctx, *j, "render issue prompt")
		return attempts, fmt.Errorf("render issue prompt: %w", err)
	}

	out, attempts, _, err := r.runCrush(ctx, j, workdir, prompt, nil)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), j, "Issue analysis cancelled.")
			r.markStatus(ctx, *j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.updatePending(ctx, j, "Issue analysis failed: Crush run failed.")
		r.markFailed(ctx, *j, "run crush issue")
		return attempts, fmt.Errorf("run crush issue: %w", err)
	}

	// Post the analysis as an issue comment. If Crush appended a create_issues
	// action block (i.e. a human accepted a proposed breakdown), create the
	// issues and link them under the reply.
	response := strings.TrimSpace(out)

	actions, response := parseIssueActions(response)
	if response == "" {
		response = "No output from analysis."
	}

	// A superseded run must not create issues — a newer job owns the slot.
	if ctx.Err() == nil && len(actions.CreateIssues) > 0 {
		if !mayCreateIssues(j.TriggerAssociation) {
			response += "\n\n_Issue creation skipped — only repository collaborators can create issues._"
		} else if created := r.createIssues(ctx, j, actions.CreateIssues); len(created) > 0 {
			var sb strings.Builder
			sb.WriteString(response)
			sb.WriteString("\n\n**Created issues:**\n")
			for _, ci := range created {
				sb.WriteString(fmt.Sprintf("- %s — %s\n", ci.URL, ci.Title))
			}
			response = sb.String()
		}
	}

	// The analysis is a final answer, not a transient status — post it
	// without the in-progress marker so later runs see it in the thread.
	if j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, response)
		cancel()
	} else {
		postCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_, _ = r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, response)
		cancel()
	}

	r.markStatus(ctx, *j, "completed")
	return attempts, nil
}

func (r *ReviewUsecase) renderIssuePrompt(j *domain.Review, thread []ConversationEntry) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "issue.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"Owner":          j.Owner,
		"Repo":           j.Repo,
		"IssueNumber":    j.PRNumber,
		"Title":          j.Title,
		"Author":         j.Author,
		"Body":           j.Body,
		"TriggerComment": j.TriggerComment,
		"BotHandle":      r.cfg.BotHandle,
		"RepoAvailable":  j.RepoAvailable,
		"SyncCmd":        r.syncCmd(),
		"Thread":         thread,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// issueActions is the optional JSON action block Crush appends to an issue
// reply when a human has accepted a proposed breakdown.
type issueActions struct {
	CreateIssues []domain.ProposedIssue `json:"create_issues"`
}

// maxCreatedIssues bounds how many issues one job may create.
const maxCreatedIssues = 10

// parseIssueActions extracts a trailing ```json action block from Crush's
// reply, returning the actions and the reply text with the block removed.
// Any well-formed actions block is stripped — even an empty create_issues —
// so protocol JSON never leaks into the posted reply.
func parseIssueActions(out string) (issueActions, string) {
	var acts issueActions
	matches := jsonBlockRegex.FindAllStringSubmatchIndex(out, -1)
	if len(matches) == 0 {
		return acts, out
	}
	last := matches[len(matches)-1]
	if err := json.Unmarshal([]byte(out[last[2]:last[3]]), &acts); err != nil {
		return issueActions{}, out
	}
	if acts.CreateIssues == nil {
		// Parsed but not an actions block (illustrative JSON) — leave it.
		return issueActions{}, out
	}
	cleaned := strings.TrimSpace(out[:last[0]] + out[last[1]:])
	return acts, cleaned
}

// createIssues creates GitHub issues for an accepted breakdown proposal,
// resolving suggested labels against the repo's label set. Final gate: the
// trigger's author_association must be collaborator-level — every caller's
// own check could miss a path, and on a public repo an unauthorized
// "create the issues" must fail here if nowhere else.
func (r *ReviewUsecase) createIssues(ctx context.Context, j *domain.Review, props []domain.ProposedIssue) []github.CreatedIssue {
	if !mayCreateIssues(j.TriggerAssociation) {
		slog.Warn("issue creation blocked: trigger lacks collaborator association",
			"job", j.JobKey, "association", j.TriggerAssociation)
		return nil
	}
	if len(props) > maxCreatedIssues {
		props = props[:maxCreatedIssues]
	}

	var suggested []string
	for _, p := range props {
		suggested = append(suggested, p.Labels...)
	}
	resolved := r.gh.ResolveLabels(ctx, j.InstallationID, j.Owner, j.Repo, []string{r.cfg.BotHandle}, suggested)

	// A hidden marker ties each created issue back to its source — future
	// dedup ("did we already file this?") can search for it instead of
	// trusting prompt text.
	srcMarker := fmt.Sprintf("<!-- elyoraa:src=%s/%s#%d job=%s -->", j.Owner, j.Repo, j.PRNumber, j.JobKey)

	var created []github.CreatedIssue
	for _, p := range props {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			continue
		}
		var labels []string
		if botTag, ok := resolved[strings.ToLower(r.cfg.BotHandle)]; ok {
			labels = append(labels, botTag)
		}
		for _, l := range p.Labels {
			if actual, ok := resolved[strings.ToLower(strings.TrimSpace(l))]; ok {
				labels = append(labels, actual)
			}
		}

		ci, err := r.gh.CreateIssue(ctx, j.InstallationID, j.Owner, j.Repo, title, p.Body+"\n\n"+srcMarker, labels)
		if err != nil {
			slog.Warn("failed to create issue", "job", j.JobKey, "title", title, "error", err)
			continue
		}
		slog.Info("created issue", "job", j.JobKey, "issue", ci.Number, "title", ci.Title)
		created = append(created, *ci)
	}
	return created
}

// annotatePhase records a job's current phase on its tracker record — a gate
// that triaged to "review" must read as a review for audit and calibration.
func (r *ReviewUsecase) annotatePhase(ctx context.Context, key, typ string) {
	if r.tracker != nil {
		r.tracker.Annotate(ctx, key, queue.JobAnnotation{Type: typ})
	}
}

// processOversize handles a PR that exceeds the review-time budget. Instead
// of a full review it runs a single Crush pass that analyzes the diff and
// proposes how to split the PR into smaller, reviewable pieces — posted as
// the pending comment. When j.SplitCreate is set (a human accepted a previous
// proposal) Crush may emit a create_issues block, which is turned into real
// GitHub issues like processIssue does.
func (r *ReviewUsecase) processOversize(ctx context.Context, j *domain.Review, worktree string, prInfo *github.PRInfo, size prSize) (attempts int, err error) {
	attempts = 1
	slog.Info("oversize PR: skipping full review for split proposal",
		"job", j.JobKey, "files", size.Files, "additions", size.Additions,
		"deletions", size.Deletions, "generated_files", size.GeneratedFiles,
		"est_minutes", size.EstMinutes, "budget_minutes", jobBudget(r.cfg, *j),
		"truncated", size.Truncated, "create_mode", j.SplitCreate)

	// PR conversation — in create mode the bot's earlier proposal and the
	// human's acceptance live here.
	thread := r.fetchConversation(ctx, j)

	prompt, err := r.renderSplitPrompt(j, prInfo, size, j.SplitCreate, thread)
	if err != nil {
		r.updatePending(ctx, j, "Review failed: could not render prompt.")
		r.markFailed(ctx, *j, "render split prompt")
		return attempts, fmt.Errorf("render split prompt: %w", err)
	}

	out, attempts, _, err := r.runCrush(ctx, j, worktree, prompt, validateSplitOutput)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), j, "Review cancelled: a newer push replaced this one.")
			r.markStatus(ctx, *j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.updatePending(ctx, j, "Split analysis failed: Crush run failed.")
		r.markFailed(ctx, *j, "run crush split")
		return attempts, fmt.Errorf("run crush split: %w", err)
	}

	response := strings.TrimSpace(out)
	actions, response := parseIssueActions(response)
	if response == "" {
		response = "No output from analysis."
	}

	// A superseded run must not create issues — a newer job owns the slot.
	if ctx.Err() == nil && len(actions.CreateIssues) > 0 {
		if !mayCreateIssues(j.TriggerAssociation) {
			response += "\n\n_Issue creation skipped — only repository collaborators can create issues._"
		} else if created := r.createIssues(ctx, j, actions.CreateIssues); len(created) > 0 {
			var sb strings.Builder
			sb.WriteString(response)
			sb.WriteString("\n\n**Created issues:**\n")
			for _, ci := range created {
				sb.WriteString(fmt.Sprintf("- %s — %s\n", ci.URL, ci.Title))
			}
			response = sb.String()
		}
	}

	// The proposal is a final answer, not a transient status — post it
	// without the in-progress marker so later analysis can see it in the
	// conversation thread.
	if j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, response)
		cancel()
	} else {
		postCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_, _ = r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, response)
		cancel()
	}

	r.markStatus(ctx, *j, "split_suggested")
	return attempts, nil
}

func (r *ReviewUsecase) renderSplitPrompt(j *domain.Review, prInfo *github.PRInfo, size prSize, createMode bool, thread []ConversationEntry) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "split.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"Owner":          j.Owner,
		"Repo":           j.Repo,
		"RepoKey":        fmt.Sprintf("%s/%s", j.Owner, j.Repo),
		"PRNumber":       j.PRNumber,
		"Title":          j.Title,
		"Body":           j.Body,
		"Author":         j.Author,
		"HeadSHA":        j.HeadSHA,
		"BaseSHA":        j.BaseSHA,
		"BotHandle":      r.cfg.BotHandle,
		"TriggerComment": j.TriggerComment,
		"SyncCmd":        r.syncCmd(),
		"Size":           size,
		"BudgetMinutes":  jobBudget(r.cfg, *j),
		"CreateMode":     createMode,
		"PatchText":      prInfo.PatchText,
		"PatchTooBig":    prInfo.PatchTooBig,
		"Files":          prInfo.Files,
		"Thread":         thread,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (r *ReviewUsecase) markStatus(ctx context.Context, j domain.Review, status string) {
	if r.store != nil && j.DeliveryID != "" {
		_ = r.store.UpdateStatus(ctx, j.DeliveryID, status, "")
	}
}

func (r *ReviewUsecase) updatePending(ctx context.Context, j *domain.Review, body string) {
	if j.PendingCommentID == 0 {
		return
	}
	body = body + "\n\n" + inProgressMarker
	ctx2, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
	defer cancel()
	_ = r.gh.EditComment(ctx2, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, body)
}

// ReconcileStuckJobs runs once at startup: tracker records whose job is
// gone for good — killed mid-flight with no surviving queue row — are
// marked failed (retryable via POST /jobs/retry) and their pending
// comments fixed so the PR doesn't show "in progress" forever.
// StuckProcessing is backend-aware: a record whose queue row survived —
// still queued on disk, reclaimed from in-flight, or owned by a live
// replica — re-runs on its own and is left alone.
func (r *ReviewUsecase) ReconcileStuckJobs(ctx context.Context) {
	if r.q == nil {
		return
	}
	t := r.q.Tracker()
	if t == nil {
		return
	}
	for _, rec := range t.StuckProcessing(ctx) {
		j := rec.Job
		if j.JobKey == "" {
			// Skeleton record (e.g. written by Started on a replica before
			// the Enqueued insert landed) — backfill the identity fields so
			// Failed upserts the real row, not an empty key.
			j.JobKey, j.Owner, j.Repo, j.PRNumber = rec.JobKey, rec.Owner, rec.Repo, rec.PRNumber
		}
		t.Failed(ctx, j, 0, errors.New("interrupted by restart"))
		if j.PendingCommentID != 0 {
			editCtx, cancel := context.WithTimeout(ctx, r.cfg.CommentTimeout)
			err := r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID,
				"Interrupted by a restart — re-mention me or retry this job via `POST /jobs/retry/"+j.JobKey+"`.")
			cancel()
			if err != nil {
				slog.Warn("reconcile: pending comment edit failed",
					"job", rec.JobKey, "comment", j.PendingCommentID, "error", err)
			}
		}
		slog.Info("reconciled stuck job", "job", rec.JobKey, "type", rec.Type)
	}
}

// priorFindingsCap bounds how many previous-review comments are injected
// into the next review prompt.
const priorFindingsCap = 30

// fetchPreviousReview returns the most recent bot review on the PR with its
// inline findings. Failures are non-fatal — the review proceeds without them.
func (r *ReviewUsecase) fetchPreviousReview(ctx context.Context, j *domain.Review) *github.PreviousReview {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CommentTimeout)
	defer cancel()
	prev, err := r.gh.GetPreviousReview(cctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, r.cfg.BotHandle, priorFindingsCap)
	if err != nil {
		slog.Warn("failed to fetch previous review", "job", j.JobKey, "owner", j.Owner, "repo", j.Repo, "pr", j.PRNumber, "error", err)
		return nil
	}
	return prev
}

func (r *ReviewUsecase) renderPrompt(j *domain.Review, prInfo *github.PRInfo) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "review.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"Owner":       j.Owner,
		"Repo":        j.Repo,
		"PRNumber":    j.PRNumber,
		"Title":       j.Title,
		"Body":        j.Body,
		"Author":      j.Author,
		"HeadSHA":     j.HeadSHA,
		"BaseSHA":     j.BaseSHA,
		"RepoKey":     fmt.Sprintf("%s/%s", j.Owner, j.Repo),
		"AuthorKey":   j.Author,
		"PatchText":   prInfo.PatchText,
		"PatchTooBig": prInfo.PatchTooBig,
		"Files":       prInfo.Files,
		"SyncCmd":     r.syncCmd(),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// maxOutputLogBytes caps how much subprocess output is written to logs.
const maxOutputLogBytes = 8192

// crushPreflightTimeout bounds the CLI version check so a hung CLI
// doesn't consume the whole attempt budget.
const crushPreflightTimeout = 60 * time.Second

// runCrush invokes the Crush CLI, retrying transient failures with exponential
// backoff. When validate is non-nil, a zero-exit run whose output fails
// validation is retried as a transient failure — a malformed response must
// never silently reach the caller (e.g. an unparseable review approving a PR).
// It returns the combined output, the number of attempts used, and the final
// error (if any).
// The lastDuration return is the final attempt's wall time (preflight +
// exec), recorded for calibration — pickup→finish includes retry backoffs
// that would otherwise depress the measured lines-per-minute rate.
func (r *ReviewUsecase) runCrush(ctx context.Context, j *domain.Review, worktree, prompt string, validate func(string) error) (string, int, time.Duration, error) {
	tmpDir, err := os.MkdirTemp("", "elyoraa-review-*")
	if err != nil {
		return "", 0, 0, err
	}
	defer os.RemoveAll(tmpDir)

	// Run from a trusted, per-job directory. Never let Crush load a PR's
	// crush.json/crushrc (which can execute code during configuration).
	repo, err := startCrushRepo(ctx, worktree, func(syncCtx context.Context) error {
		if r.gh == nil || r.cache == nil {
			return errors.New("repository sync unavailable")
		}
		token, err := r.gh.InstallationToken(syncCtx, j.InstallationID)
		if err != nil {
			return err
		}
		return r.cache.Fetch(syncCtx, j.Owner, j.Repo, token)
	})
	if err != nil {
		return "", 0, 0, err
	}
	defer repo.Close()
	if err := writeCrushConfig(tmpDir, repo); err != nil {
		return "", 0, 0, err
	}
	model := r.jobModel(j)
	args := []string{"run", "--quiet"}
	if model != "" {
		args = append(args, "--model", model)
	}
	prompt = "Repository access is through the elyoraa MCP tools only: read_file, list_files, search, git (read-only argv, without a shell), and sync. Paths are relative to the repository worktree. Use these tools to inspect code and history. Shell execution, edits, and direct network access are unavailable.\n\n" + prompt

	maxRetries := r.cfg.CrushMaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, r.attemptTimeout(j))
		log := r.jobLogger(j, attempt, worktree, args, model)
		attemptStart := time.Now()

		var out string
		var runErr error
		transient := true
		reason := "preflight"

		if r.cfg.CrushPreflightCheck {
			if perr := r.crushPreflight(attemptCtx, tmpDir); perr != nil {
				runErr = fmt.Errorf("crush preflight: %w", perr)
				if rsn, det := deterministicCrushReason(runErr, runErr.Error()); det {
					transient, reason = false, rsn
				}
			}
		}
		if runErr == nil {
			log.Info("crush run started")
			out, runErr = r.execCrush(attemptCtx, tmpDir, args, prompt, attempt)
			switch {
			case runErr != nil:
				transient, reason = classifyCrushError(runErr, attemptCtx, out)
			case validate != nil:
				if verr := validate(out); verr != nil {
					runErr, transient, reason = verr, true, "malformed output"
				}
			}
		}
		cancel()

		if runErr == nil {
			return out, attempt, time.Since(attemptStart), nil
		}

		tail := tailString(out, maxOutputLogBytes)
		if !transient || attempt > maxRetries || ctx.Err() != nil {
			if reason == "canceled" {
				log.Info("crush run canceled", "output", tail)
			} else {
				log.Error("crush run failed", "error", runErr, "transient", transient, "reason", reason, "output", tail)
			}
			return out, attempt, time.Since(attemptStart), runErr
		}

		wait := r.backoff(attempt)
		log.Warn("crush attempt failed, will retry", "error", runErr, "reason", reason, "output", tail, "retry_in", wait.String())
		if !sleepCtx(ctx, wait) {
			return "", attempt, 0, ctx.Err()
		}
	}
}

// crushPreflight checks the installed fork without loading untrusted config.
// Provider/model authentication is validated by the run itself; `crush models`
// also lists unconfigured providers and cannot establish credential validity.
func (r *ReviewUsecase) crushPreflight(ctx context.Context, dir string) error {
	pfCtx, cancel := context.WithTimeout(ctx, crushPreflightTimeout)
	defer cancel()
	cmd := r.crushCommand(pfCtx, dir, []string{"--version"}, 0)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("crush --version: %w: %s", err, tailString(string(out), 2048))
	}
	return nil
}

// execCrush sends the prompt on stdin and parses stdout alone: diagnostics on
// stderr must never be mistaken for an agent's JSON review or action block.
func (r *ReviewUsecase) execCrush(ctx context.Context, dir string, args []string, prompt string, attempt int) (string, error) {
	cmd := r.crushCommand(ctx, dir, args, attempt)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.String() + "\n" + stdout.String(), err
	}
	return stdout.String(), nil
}

func (r *ReviewUsecase) crushCommand(ctx context.Context, dir string, args []string, attempt int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.cfg.CrushBin, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 30 * time.Second
	// The model provider may need environment credentials. Repository tools run
	// in Go and never expose this environment or the trusted Crush configuration.
	cmd.Env = append(os.Environ(),
		"CRUSH_GLOBAL_CONFIG="+r.cfg.CrushConfigDir,
		"CRUSH_GLOBAL_DATA="+r.cfg.CrushDataDir,
		"CRUSH_DATA_DIR="+filepath.Join(dir, fmt.Sprintf("attempt-%d", attempt)),
		"CRUSH_CLIENT_SERVER=false",
		"CRUSH_DISABLE_METRICS=1",
		"CRUSH_DISABLE_PROVIDER_AUTO_UPDATE=1",
		"CRUSH_SKILLS_DIR="+filepath.Join(dir, "skills"),
		"XDG_CACHE_HOME="+dir,
		"TMPDIR="+dir,
		"GIT_TERMINAL_PROMPT=0",
	)
	return cmd
}

// jobModel returns the Crush model for this job type: dedicated lighter
// models may handle routing/triage while reviews keep CRUSH_MODEL.
func (r *ReviewUsecase) jobModel(j *domain.Review) string {
	switch {
	case j.IsRespond():
		if r.cfg.RespondModel != "" {
			return r.cfg.RespondModel
		}
	case j.IsGate():
		if r.cfg.GateModel != "" {
			return r.cfg.GateModel
		}
		if r.cfg.RespondModel != "" {
			return r.cfg.RespondModel
		}
	}
	return r.cfg.CrushModel
}

// jobTimeout returns the overall processing budget for this job type.
// Gate jobs get the full review budget — the triage verdict transitions them
// into a review or split, which needs the remaining time. GATE_TIMEOUT bounds
// only the triage pass itself (see triageGate).
func (r *ReviewUsecase) jobTimeout(j *domain.Review) time.Duration {
	if j.IsRespond() {
		return r.cfg.RespondTimeout
	}
	return r.cfg.ReviewTimeout
}

// attemptTimeout returns the per-attempt timeout; defaults to the job's
// overall timeout when CRUSH_ATTEMPT_TIMEOUT is unset.
func (r *ReviewUsecase) attemptTimeout(j *domain.Review) time.Duration {
	if r.cfg.CrushAttemptTimeout > 0 {
		return r.cfg.CrushAttemptTimeout
	}
	return r.jobTimeout(j)
}

// backoff returns the wait before retry attempt n (1-indexed).
func (r *ReviewUsecase) backoff(retry int) time.Duration {
	bs := r.cfg.CrushRetryBackoffs
	if len(bs) == 0 {
		return 30 * time.Second
	}
	if retry-1 >= len(bs) {
		return bs[len(bs)-1]
	}
	return bs[retry-1]
}

// jobLogger returns a logger carrying the full job context.
func (r *ReviewUsecase) jobLogger(j *domain.Review, attempt int, workdir string, args []string, model string) *slog.Logger {
	return slog.With(
		"job", j.JobKey,
		"delivery", j.DeliveryID,
		"owner", j.Owner,
		"repo", j.Repo,
		"pr", j.PRNumber,
		"type", j.Type,
		"attempt", attempt,
		"model", model,
		"workdir", workdir,
		"argv", redactArgs(r.cfg.CrushBin, args),
	)
}

// Error substrings that mark a crush failure as deterministic (not retriable).
var crushDeterministicPatterns = []string{
	"untrusted workspace",
	"workspace trust",
	"unauthorized",
	"authentication failed",
	"invalid api key",
	"invalid token",
	"bad credentials",
	"access denied",
	"permission denied",
	"401",
	"403",
	"no such file or directory",
}

// Error substrings that mark a crush failure as transient (retriable).
var crushTransientPatterns = []string{
	"unknown model",
	"no models",
	"empty model",
	"model list",
	"connection refused",
	"connection reset",
	"no such host",
	"i/o timeout",
	"timeout",
	"timed out",
	"deadline exceeded",
	"eof",
	"rate limit",
	"too many requests",
	"429",
	"502",
	"503",
	"504",
	"service unavailable",
	"overloaded",
	"temporarily unavailable",
	"tls handshake",
	"network is unreachable",
	"dial tcp",
}

// deterministicCrushReason reports whether err/text marks a deterministic
// (non-retriable) crush failure, returning a short reason when it does.
func deterministicCrushReason(err error, text string) (string, bool) {
	if errors.Is(err, context.Canceled) {
		return "canceled", true
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		return "binary not found", true
	}
	lower := strings.ToLower(text)
	for _, p := range crushDeterministicPatterns {
		if strings.Contains(lower, p) {
			return "deterministic: " + p, true
		}
	}
	return "", false
}

// classifyCrushError reports whether a crush subprocess failure looks
// transient (worth retrying) or deterministic. attemptCtx must still be live.
func classifyCrushError(err error, attemptCtx context.Context, out string) (bool, string) {
	if attemptCtx.Err() == context.DeadlineExceeded {
		return true, "attempt timeout"
	}
	if attemptCtx.Err() == context.Canceled {
		return false, "canceled"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && !exitErr.ProcessState.Exited() {
		// Killed by a signal before it could write a verdict — OOM,
		// container stop, or a crash. Almost always transient; same
		// retry path as malformed output.
		return true, exitErr.ProcessState.String()
	}
	if rsn, det := deterministicCrushReason(err, out); det {
		return false, rsn
	}

	lower := strings.ToLower(out)
	for _, p := range crushTransientPatterns {
		if strings.Contains(lower, p) {
			return true, "transient: " + p
		}
	}
	if strings.TrimSpace(out) == "" {
		return true, "no output"
	}
	return false, "unrecognized error output"
}

// tailString returns the last n bytes of s, trimmed to a line boundary.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < len(t)-1 {
		t = t[i+1:]
	}
	return fmt.Sprintf("…[truncated %d bytes]\n%s", len(s)-len(t), t)
}

// sleepCtx sleeps for d or until ctx is done. Returns false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var secretNameRe = regexp.MustCompile(`(?i)(token|secret|password|api[_-]?key|credential)`)
var secretValueRe = regexp.MustCompile(`(ghp_[A-Za-z0-9]{10,}|github_pat_[A-Za-z0-9_]{10,}|gho_[A-Za-z0-9]{10,}|sk-[A-Za-z0-9]{8,}|xox[baprs]-[A-Za-z0-9-]{5,})`)

// redactArgs renders the command line, masking values of secret-named flags
// and any argument that looks like a credential.
func redactArgs(bin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, bin)
	for i, a := range args {
		if i > 0 && isSecretFlag(args[i-1]) {
			parts = append(parts, "***")
			continue
		}
		if eq := strings.IndexByte(a, '='); eq > 0 && secretNameRe.MatchString(a[:eq]) {
			parts = append(parts, a[:eq+1]+"***")
			continue
		}
		parts = append(parts, secretValueRe.ReplaceAllString(a, "***"))
	}
	return strings.Join(parts, " ")
}

func isSecretFlag(a string) bool {
	if !strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
		return false
	}
	return secretNameRe.MatchString(a)
}

var jsonBlockRegex = regexp.MustCompile("(?s)```json\\s*(.*?)\\s*```")

// parseReviewOutput extracts the last ```json block from Crush's output. It
// is strict: a missing block or unparseable JSON is an error — treating prose
// as "no comments" would silently approve the PR.
func parseReviewOutput(out string) (*domain.Output, error) {
	matches := jsonBlockRegex.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return nil, errors.New("no json block in crush output")
	}
	last := matches[len(matches)-1][1]
	var result domain.Output
	if err := json.Unmarshal([]byte(last), &result); err != nil {
		return nil, fmt.Errorf("invalid review json: %w", err)
	}
	return &result, nil
}

// validateReviewOutput rejects Crush runs that produced unusable review
// output, so the retry loop can try again instead of approving blindly: the
// JSON must parse, and an approval (zero comments) must come with the audit
// trail proving the review actually ran.
func validateReviewOutput(out string) error {
	review, err := parseReviewOutput(out)
	if err != nil {
		return err
	}
	if len(review.Comments) == 0 && len(review.PassesCompleted) == 0 && len(review.FilesReviewed) == 0 {
		return errors.New("review approved without audit fields (passes_completed/files_reviewed empty)")
	}
	return nil
}

// validateSplitOutput rejects Crush runs that produced no usable text — an
// empty proposal comment on a PR is a transient failure, not an answer.
func validateSplitOutput(out string) error {
	if strings.TrimSpace(out) == "" {
		return errors.New("empty split output")
	}
	return nil
}

// validateReconcileOutput rejects reconcile runs that produced no parseable
// JSON block.
func validateReconcileOutput(out string) error {
	_, err := parseReconcileOutput(out)
	return err
}

// reconcileFindings runs a focused second Crush pass that re-verifies each
// finding from the previous bot review against the current code, then merges
// still-present findings the fresh review missed back into the output. It
// runs AFTER the unbiased fresh review so prior findings cannot bias it.
func (r *ReviewUsecase) reconcileFindings(ctx context.Context, j *domain.Review, worktree string, prev *github.PreviousReview, review *domain.Output) {
	if prev == nil || len(prev.Findings) == 0 {
		return
	}

	markUnclear := func() {
		for _, f := range prev.Findings {
			review.PriorFindings = append(review.PriorFindings, domain.PriorFindingStatus{Path: f.Path, Line: f.Line, Status: "unclear"})
		}
	}

	prompt, err := r.renderReconcilePrompt(j, prev, review)
	if err != nil {
		slog.Warn("reconcile prompt render failed", "job", j.JobKey, "error", err)
		markUnclear()
		return
	}

	out, _, _, err := r.runCrush(ctx, j, worktree, prompt, validateReconcileOutput)
	if err != nil {
		if ctx.Err() != nil {
			return // job cancelled or deadline — don't fabricate statuses
		}
		slog.Warn("reconcile pass failed", "job", j.JobKey, "error", err)
		markUnclear()
		return
	}

	rec, err := parseReconcileOutput(out)
	if err != nil {
		slog.Warn("reconcile output parse failed", "job", j.JobKey, "error", err)
		markUnclear()
		return
	}

	review.PriorFindings = rec.PriorFindings
	statused := make(map[string]bool, len(rec.PriorFindings))
	for _, pf := range rec.PriorFindings {
		statused[fmt.Sprintf("%s:%d", pf.Path, pf.Line)] = true
	}
	for _, f := range prev.Findings {
		if !statused[fmt.Sprintf("%s:%d", f.Path, f.Line)] {
			review.PriorFindings = append(review.PriorFindings, domain.PriorFindingStatus{Path: f.Path, Line: f.Line, Status: "unclear"})
		}
	}

	// Merge additional comments, deduped against the fresh comments.
	seen := make(map[string]bool, len(review.Comments))
	for _, c := range review.Comments {
		seen[fmt.Sprintf("%s:%d", c.Path, c.Line)] = true
	}
	for _, c := range rec.AdditionalComments {
		k := fmt.Sprintf("%s:%d", c.Path, c.Line)
		if !seen[k] {
			review.Comments = append(review.Comments, c)
			seen[k] = true
		}
	}
}

// reconcileOutput is the parsed result of the reconcile pass.
type reconcileOutput struct {
	PriorFindings      []domain.PriorFindingStatus `json:"prior_findings_status"`
	AdditionalComments []domain.Comment            `json:"additional_comments"`
}

func parseReconcileOutput(out string) (*reconcileOutput, error) {
	matches := jsonBlockRegex.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return nil, errors.New("no json block in reconcile output")
	}
	var rec reconcileOutput
	if err := json.Unmarshal([]byte(matches[len(matches)-1][1]), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *ReviewUsecase) renderReconcilePrompt(j *domain.Review, prev *github.PreviousReview, review *domain.Output) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "reconcile.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"RepoKey":       fmt.Sprintf("%s/%s", j.Owner, j.Repo),
		"PRNumber":      j.PRNumber,
		"Title":         j.Title,
		"HeadSHA":       j.HeadSHA,
		"Findings":      prev.Findings,
		"FreshComments": review.Comments,
		"SyncCmd":       r.syncCmd(),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// syncCmd names the credential-free repository tool exposed to Crush.
func (r *ReviewUsecase) syncCmd() string { return "mcp_elyoraa_sync" }
