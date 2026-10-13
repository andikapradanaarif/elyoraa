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

// ReviewUsecase orchestrates a single PR review.
type ReviewUsecase struct {
	cfg     *config.Config
	cache   *cache.Cache
	gh      *github.Client
	d       *dedup.Dedup
	store   repository.WebhookStore
	metrics *metrics.Registry
}

// NewReviewUsecase creates a ReviewUsecase.
func NewReviewUsecase(cfg *config.Config, c *cache.Cache, g *github.Client, d *dedup.Dedup, s repository.WebhookStore, m *metrics.Registry) *ReviewUsecase {
	return &ReviewUsecase{cfg: cfg, cache: c, gh: g, d: d, store: s, metrics: m}
}

// Process implements queue.Processor.
func (r *ReviewUsecase) Process(ctx context.Context, j domain.Review) (attempts int, err error) {
	attempts = 1
	if r.store != nil && j.DeliveryID != "" {
		_ = r.store.UpdateStatus(ctx, j.DeliveryID, "processing", "")
	}

	start := time.Now()
	defer func() {
		r.d.Done(j.JobKey)
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
	if !r.d.BindCancel(j.JobKey, jobCancel) {
		r.markStatus(ctx, j, "cancelled")
		return attempts, queue.ErrJobCancelled
	}

	ctx, cancel := context.WithTimeout(jobCtx, r.cfg.ReviewTimeout)
	defer cancel()

	if j.IsIssue() {
		return r.processIssue(ctx, &j)
	}

	token, err := r.gh.InstallationToken(ctx, j.InstallationID)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not authenticate to GitHub.")
		r.markFailed(ctx, j, "installation token")
		return attempts, fmt.Errorf("installation token: %w", err)
	}

	if err := r.cache.Fetch(ctx, j.Owner, j.Repo, token); err != nil {
		if errors.Is(err, cache.ErrRepoNotCloned) {
			r.updatePending(ctx, &j, fmt.Sprintf(
				"Review skipped: repository `%s/%s` is not cloned on this server. "+
					"Clone it first at `%s/%s/%s.git`.",
				j.Owner, j.Repo, r.cfg.WorkspaceBase, j.Owner, j.Repo,
			))
			r.markStatus(ctx, j, "skipped")
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

	// Size gate: when the estimated review effort exceeds the budget,
	// post a PR-split proposal instead of a full review — unless the
	// trigger comment explicitly asks for the review (or accepts a
	// previous proposal, in which case the issues get created).
	if r.cfg.PRSizeGate {
		size := estimatePRSize(prInfo, r.cfg.PRLinesPerMinute)
		if size.tooBig(r.cfg.PRReviewBudgetMinutes) {
			switch classifyOversize(j.TriggerComment) {
			case oversizeReview:
				slog.Info("oversize PR: explicit review request, bypassing size gate",
					"job", j.JobKey, "files", size.Files, "est_minutes", size.EstMinutes)
			case oversizeCreate:
				return r.processOversize(ctx, &j, worktree, prInfo, size, true)
			default:
				return r.processOversize(ctx, &j, worktree, prInfo, size, false)
			}
		}
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

	out, attempts, err := r.runDevin(ctx, &j, worktree, prompt, validateReviewOutput)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), &j, "Review cancelled: a newer push replaced this one.")
			r.markStatus(ctx, j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.updatePending(ctx, &j, "Review failed: Devin review run failed.")
		r.markFailed(ctx, j, "run devin")
		return attempts, fmt.Errorf("run devin: %w", err)
	}

	review, err := parseReviewOutput(out)
	if err != nil {
		// Should not happen — runDevin validates the output — but never
		// approve on a parse failure.
		r.updatePending(ctx, &j, "Review failed: could not parse Devin output.")
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
		// (e.g. elyoraa[bot]/Devin), fall back to posting a comment instead.
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

// processIssue handles an issue analysis job: spawn Devin to analyze the issue
// and post the response as a comment. If the repo is cloned on the server,
// Devin gets a worktree so it can search the code. If not, Devin analyzes
// from the issue text alone and mentions that the repo isn't available.
func (r *ReviewUsecase) processIssue(ctx context.Context, j *domain.Review) (attempts int, err error) {
	attempts = 1
	pendingBody := "Analyzing issue…\n\n<!-- elyoraa-in-progress -->"
	pendingCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
	commentID, err := r.gh.PostPendingComment(pendingCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, pendingBody)
	cancel()
	if err != nil {
		slog.Warn("failed to post pending comment", "error", err)
	} else {
		j.PendingCommentID = commentID
	}

	// Try to get a worktree so Devin can search the repo code.
	// If the repo isn't cloned, fall back to a temp dir and tell Devin.
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

	// Fetch the comment thread so the analysis can follow prior discussion:
	// previous bot replies, human acceptance of a proposal, open questions.
	thread := r.fetchIssueThread(ctx, j)

	prompt, err := r.renderIssuePrompt(j, thread)
	if err != nil {
		r.updatePending(ctx, j, "Issue analysis failed: could not render prompt.")
		r.markFailed(ctx, *j, "render issue prompt")
		return attempts, fmt.Errorf("render issue prompt: %w", err)
	}

	out, attempts, err := r.runDevin(ctx, j, workdir, prompt, nil)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), j, "Issue analysis cancelled.")
			r.markStatus(ctx, *j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.updatePending(ctx, j, "Issue analysis failed: Devin run failed.")
		r.markFailed(ctx, *j, "run devin issue")
		return attempts, fmt.Errorf("run devin issue: %w", err)
	}

	// Post the analysis as an issue comment. If Devin appended a create_issues
	// action block (i.e. a human accepted a proposed breakdown), create the
	// issues and link them under the reply.
	response := strings.TrimSpace(out)

	actions, response := parseIssueActions(response)
	if response == "" {
		response = "No output from analysis."
	}

	if len(actions.CreateIssues) > 0 {
		created := r.createIssues(ctx, j, actions.CreateIssues)
		if len(created) > 0 {
			var sb strings.Builder
			sb.WriteString(response)
			sb.WriteString("\n\n**Created issues:**\n")
			for _, ci := range created {
				sb.WriteString(fmt.Sprintf("- %s — %s\n", ci.URL, ci.Title))
			}
			response = sb.String()
		}
	}

	if j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, response+"\n\n<!-- elyoraa-in-progress -->")
		cancel()
	} else {
		postCtx, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
		_, _ = r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, response)
		cancel()
	}

	r.markStatus(ctx, *j, "completed")
	return attempts, nil
}

// issueThreadMaxComments and issueThreadMaxBytes bound how much of an issue's
// comment thread is injected into the analysis prompt.
const (
	issueThreadMaxComments = 50
	issueThreadMaxBytes    = 48 * 1024
)

// fetchIssueThread returns the issue's comment thread, minus the transient
// "Analyzing issue…" placeholder comment. Failures are non-fatal.
func (r *ReviewUsecase) fetchIssueThread(ctx context.Context, j *domain.Review) []github.IssueComment {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CommentTimeout)
	defer cancel()
	comments, err := r.gh.ListIssueComments(cctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, issueThreadMaxComments, issueThreadMaxBytes)
	if err != nil {
		slog.Warn("failed to fetch issue comments", "job", j.JobKey, "owner", j.Owner, "repo", j.Repo, "issue", j.PRNumber, "error", err)
		return nil
	}
	out := comments[:0]
	for _, c := range comments {
		if strings.Contains(c.Body, "<!-- elyoraa-in-progress -->") {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (r *ReviewUsecase) renderIssuePrompt(j *domain.Review, thread []github.IssueComment) (string, error) {
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
		"Thread":         thread,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// issueActions is the optional JSON action block Devin appends to an issue
// reply when a human has accepted a proposed breakdown.
type issueActions struct {
	CreateIssues []domain.ProposedIssue `json:"create_issues"`
}

// maxCreatedIssues bounds how many issues one job may create.
const maxCreatedIssues = 10

// parseIssueActions extracts a trailing ```json action block from Devin's
// reply, returning the actions and the reply text with the block removed.
func parseIssueActions(out string) (issueActions, string) {
	var acts issueActions
	matches := jsonBlockRegex.FindAllStringSubmatchIndex(out, -1)
	if len(matches) == 0 {
		return acts, out
	}
	last := matches[len(matches)-1]
	if err := json.Unmarshal([]byte(out[last[2]:last[3]]), &acts); err != nil || len(acts.CreateIssues) == 0 {
		return issueActions{}, out
	}
	cleaned := strings.TrimSpace(out[:last[0]] + out[last[1]:])
	return acts, cleaned
}

// createIssues creates GitHub issues for an accepted breakdown proposal,
// resolving suggested labels against the repo's label set.
func (r *ReviewUsecase) createIssues(ctx context.Context, j *domain.Review, props []domain.ProposedIssue) []github.CreatedIssue {
	if len(props) > maxCreatedIssues {
		props = props[:maxCreatedIssues]
	}

	var suggested []string
	for _, p := range props {
		suggested = append(suggested, p.Labels...)
	}
	resolved := r.gh.ResolveLabels(ctx, j.InstallationID, j.Owner, j.Repo, []string{r.cfg.BotHandle}, suggested)

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

		ci, err := r.gh.CreateIssue(ctx, j.InstallationID, j.Owner, j.Repo, title, p.Body, labels)
		if err != nil {
			slog.Warn("failed to create issue", "job", j.JobKey, "title", title, "error", err)
			continue
		}
		slog.Info("created issue", "job", j.JobKey, "issue", ci.Number, "title", ci.Title)
		created = append(created, *ci)
	}
	return created
}

// processOversize handles a PR that exceeds the review-time budget. Instead
// of a full review it runs a single Devin pass that analyzes the diff and
// proposes how to split the PR into smaller, reviewable pieces — posted as
// the pending comment. When createMode is on (a human accepted a previous
// proposal) Devin may emit a create_issues block, which is turned into real
// GitHub issues like processIssue does.
func (r *ReviewUsecase) processOversize(ctx context.Context, j *domain.Review, worktree string, prInfo *github.PRInfo, size prSize, createMode bool) (attempts int, err error) {
	attempts = 1
	slog.Info("oversize PR: skipping full review for split proposal",
		"job", j.JobKey, "files", size.Files, "additions", size.Additions,
		"deletions", size.Deletions, "generated_files", size.GeneratedFiles,
		"est_minutes", size.EstMinutes, "budget_minutes", r.cfg.PRReviewBudgetMinutes,
		"truncated", size.Truncated, "create_mode", createMode)

	// PR conversation thread — in create mode the bot's earlier proposal and
	// the human's acceptance live here.
	thread := r.fetchIssueThread(ctx, j)

	prompt, err := r.renderSplitPrompt(j, prInfo, size, createMode, thread)
	if err != nil {
		r.updatePending(ctx, j, "Review failed: could not render prompt.")
		r.markFailed(ctx, *j, "render split prompt")
		return attempts, fmt.Errorf("render split prompt: %w", err)
	}

	out, attempts, err := r.runDevin(ctx, j, worktree, prompt, validateSplitOutput)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(context.Background(), j, "Review cancelled: a newer push replaced this one.")
			r.markStatus(ctx, *j, "cancelled")
			return attempts, queue.ErrJobCancelled
		}
		r.updatePending(ctx, j, "Split analysis failed: Devin run failed.")
		r.markFailed(ctx, *j, "run devin split")
		return attempts, fmt.Errorf("run devin split: %w", err)
	}

	response := strings.TrimSpace(out)
	actions, response := parseIssueActions(response)
	if response == "" {
		response = "No output from analysis."
	}

	if len(actions.CreateIssues) > 0 {
		created := r.createIssues(ctx, j, actions.CreateIssues)
		if len(created) > 0 {
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

func (r *ReviewUsecase) renderSplitPrompt(j *domain.Review, prInfo *github.PRInfo, size prSize, createMode bool, thread []github.IssueComment) (string, error) {
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
		"Size":           size,
		"BudgetMinutes":  r.cfg.PRReviewBudgetMinutes,
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
	body = body + "\n\n<!-- elyoraa-in-progress -->"
	ctx2, cancel := context.WithTimeout(context.Background(), r.cfg.CommentTimeout)
	defer cancel()
	_ = r.gh.EditComment(ctx2, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, body)
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
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// maxOutputLogBytes caps how much subprocess output is written to logs.
const maxOutputLogBytes = 8192

// devinPreflightTimeout bounds the `devin models list` check so a hung CLI
// doesn't consume the whole attempt budget.
const devinPreflightTimeout = 60 * time.Second

// runDevin invokes the Devin CLI, retrying transient failures with exponential
// backoff. When validate is non-nil, a zero-exit run whose output fails
// validation is retried as a transient failure — a malformed response must
// never silently reach the caller (e.g. an unparseable review approving a PR).
// It returns the combined output, the number of attempts used, and the final
// error (if any).
func (r *ReviewUsecase) runDevin(ctx context.Context, j *domain.Review, worktree, prompt string, validate func(string) error) (string, int, error) {
	tmpDir, err := os.MkdirTemp("", "elyoraa-review-*")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(tmpDir)

	promptFile := filepath.Join(tmpDir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o644); err != nil {
		return "", 0, err
	}

	args := []string{
		"-p",
		"--permission-mode", r.cfg.DevinPermissionMode,
		"--model", r.cfg.DevinModel,
		"--config", filepath.Join(r.cfg.DevinConfigDir, "devin-config.json"),
		"--respect-workspace-trust", "false",
		"--prompt-file", promptFile,
	}

	maxRetries := r.cfg.DevinMaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, r.attemptTimeout())
		log := r.jobLogger(j, attempt, worktree, args)

		var out string
		var runErr error
		transient := true
		reason := "preflight"

		if r.cfg.DevinPreflightCheck {
			if perr := r.devinPreflight(attemptCtx, worktree); perr != nil {
				runErr = fmt.Errorf("devin preflight: %w", perr)
				if rsn, det := deterministicDevinReason(runErr, runErr.Error()); det {
					transient, reason = false, rsn
				}
			}
		}
		if runErr == nil {
			log.Info("devin run started")
			out, runErr = r.execDevin(attemptCtx, worktree, args, tmpDir)
			switch {
			case runErr != nil:
				transient, reason = classifyDevinError(runErr, attemptCtx, out)
			case validate != nil:
				if verr := validate(out); verr != nil {
					runErr, transient, reason = verr, true, "malformed output"
				}
			}
		}
		cancel()

		if runErr == nil {
			return out, attempt, nil
		}

		tail := tailString(out, maxOutputLogBytes)
		if !transient || attempt > maxRetries || ctx.Err() != nil {
			if reason == "canceled" {
				log.Info("devin run canceled", "output", tail)
			} else {
				log.Error("devin run failed", "error", runErr, "transient", transient, "reason", reason, "output", tail)
			}
			return out, attempt, runErr
		}

		wait := r.backoff(attempt)
		log.Warn("devin attempt failed, will retry", "error", runErr, "reason", reason, "output", tail, "retry_in", wait.String())
		if !sleepCtx(ctx, wait) {
			return "", attempt, ctx.Err()
		}
	}
}

// devinPreflight verifies the devin binary is usable: `devin models list` must
// succeed and list the configured model.
func (r *ReviewUsecase) devinPreflight(ctx context.Context, worktree string) error {
	pfCtx, cancel := context.WithTimeout(ctx, devinPreflightTimeout)
	defer cancel()

	cmd := exec.CommandContext(pfCtx, r.cfg.DevinBin, "models", "list")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		if pfCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("models list timed out")
		}
		return fmt.Errorf("models list: %w: %s", err, tailString(string(out), 2048))
	}
	if r.cfg.DevinModel != "" && !strings.Contains(string(out), r.cfg.DevinModel) {
		return fmt.Errorf("configured model %q not present in `devin models list` output", r.cfg.DevinModel)
	}
	return nil
}

// execDevin runs the devin subprocess once and returns its combined output.
func (r *ReviewUsecase) execDevin(ctx context.Context, worktree string, args []string, tmpDir string) (string, error) {
	cmd := exec.CommandContext(ctx, r.cfg.DevinBin, args...)
	cmd.Dir = worktree
	env := os.Environ()
	env = append(env, "XDG_CACHE_HOME="+tmpDir, "TMPDIR="+tmpDir)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// attemptTimeout returns the per-attempt timeout; defaults to the whole-review
// timeout when DEVIN_ATTEMPT_TIMEOUT is unset.
func (r *ReviewUsecase) attemptTimeout() time.Duration {
	if r.cfg.DevinAttemptTimeout > 0 {
		return r.cfg.DevinAttemptTimeout
	}
	return r.cfg.ReviewTimeout
}

// backoff returns the wait before retry attempt n (1-indexed).
func (r *ReviewUsecase) backoff(retry int) time.Duration {
	bs := r.cfg.DevinRetryBackoffs
	if len(bs) == 0 {
		return 30 * time.Second
	}
	if retry-1 >= len(bs) {
		return bs[len(bs)-1]
	}
	return bs[retry-1]
}

// jobLogger returns a logger carrying the full job context.
func (r *ReviewUsecase) jobLogger(j *domain.Review, attempt int, workdir string, args []string) *slog.Logger {
	return slog.With(
		"job", j.JobKey,
		"delivery", j.DeliveryID,
		"owner", j.Owner,
		"repo", j.Repo,
		"pr", j.PRNumber,
		"type", j.Type,
		"attempt", attempt,
		"model", r.cfg.DevinModel,
		"workdir", workdir,
		"argv", redactArgs(r.cfg.DevinBin, args),
	)
}

// Error substrings that mark a devin failure as deterministic (not retriable).
var devinDeterministicPatterns = []string{
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

// Error substrings that mark a devin failure as transient (retriable).
var devinTransientPatterns = []string{
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

// deterministicDevinReason reports whether err/text marks a deterministic
// (non-retriable) devin failure, returning a short reason when it does.
func deterministicDevinReason(err error, text string) (string, bool) {
	if errors.Is(err, context.Canceled) {
		return "canceled", true
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		return "binary not found", true
	}
	lower := strings.ToLower(text)
	for _, p := range devinDeterministicPatterns {
		if strings.Contains(lower, p) {
			return "deterministic: " + p, true
		}
	}
	return "", false
}

// classifyDevinError reports whether a devin subprocess failure looks
// transient (worth retrying) or deterministic. attemptCtx must still be live.
func classifyDevinError(err error, attemptCtx context.Context, out string) (bool, string) {
	if attemptCtx.Err() == context.DeadlineExceeded {
		return true, "attempt timeout"
	}
	if attemptCtx.Err() == context.Canceled {
		return false, "canceled"
	}
	if rsn, det := deterministicDevinReason(err, out); det {
		return false, rsn
	}

	lower := strings.ToLower(out)
	for _, p := range devinTransientPatterns {
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

// parseReviewOutput extracts the last ```json block from Devin's output. It
// is strict: a missing block or unparseable JSON is an error — treating prose
// as "no comments" would silently approve the PR.
func parseReviewOutput(out string) (*domain.Output, error) {
	matches := jsonBlockRegex.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return nil, errors.New("no json block in devin output")
	}
	last := matches[len(matches)-1][1]
	var result domain.Output
	if err := json.Unmarshal([]byte(last), &result); err != nil {
		return nil, fmt.Errorf("invalid review json: %w", err)
	}
	return &result, nil
}

// validateReviewOutput rejects Devin runs that produced unusable review
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

// validateSplitOutput rejects Devin runs that produced no usable text — an
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

// reconcileFindings runs a focused second Devin pass that re-verifies each
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

	out, _, err := r.runDevin(ctx, j, worktree, prompt, validateReconcileOutput)
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
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// WriteDevinConfig writes a locked devin-config.json inside dir. It omits the
// mem0 MCP server unless MEM0_API_KEY is set. Using a directory instead of a
// single file path makes bind-mounts safe against atomic rename-writes.
func WriteDevinConfig(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	cfg := map[string]any{
		"permissions": map[string][]string{
			"allow": {
				"read",
				"grep",
				"glob",
				"mcp__mem0__*",
				"Exec(git diff)",
				"Exec(git status)",
				"Exec(git log)",
				"Exec(git show)",
				"Exec(git rev-parse)",
				"Exec(git ls-files)",
			},
			"deny": {
				"edit",
				"Write(**)",
				"Exec(curl)",
				"Exec(wget)",
				"Fetch(**)",
			},
		},
	}

	if os.Getenv("MEM0_API_KEY") != "" {
		cfg["mcpServers"] = map[string]any{
			"mem0": map[string]any{
				"type": "http",
				"url":  "https://mcp.mem0.ai/mcp",
				"env": map[string]string{
					"MEM0_API_KEY": os.Getenv("MEM0_API_KEY"),
				},
			},
		}
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "devin-config.json"), b, 0o644)
}
