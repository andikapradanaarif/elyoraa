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

// pendingBody returns the transient "in progress" comment for a job type.
func pendingBody(j *domain.Review, budgetMinutes int) string {
	switch {
	case j.IsRespond():
		return "On it — reading the thread…\n\n" + inProgressMarker
	case j.IsGate():
		return "Sizing up this PR…\n\n" + inProgressMarker
	case j.IsSplit():
		return fmt.Sprintf(
			"This PR looks too big for a single review (estimated ~%d min vs the %d min budget). Drafting a split proposal…\n\n%s",
			j.SizeEstMinutes, budgetMinutes, inProgressMarker,
		)
	case j.IsIssue():
		return "Analyzing issue…\n\n" + inProgressMarker
	default:
		return "Review in progress…\n\n" + inProgressMarker
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

	out, attempts, lastDur, err := r.runDevin(ctx, &j, worktree, prompt, validateReviewOutput)
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
	if r.tracker != nil && lastDur > 0 {
		r.tracker.Annotate(ctx, j.JobKey, queue.JobAnnotation{AttemptSeconds: int64(lastDur.Seconds())})
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

	// Fetch the discussion so the analysis can follow prior discussion:
	// previous bot replies, human acceptance of a proposal, open questions.
	thread := r.fetchConversation(ctx, j)

	prompt, err := r.renderIssuePrompt(j, thread)
	if err != nil {
		r.updatePending(ctx, j, "Issue analysis failed: could not render prompt.")
		r.markFailed(ctx, *j, "render issue prompt")
		return attempts, fmt.Errorf("render issue prompt: %w", err)
	}

	out, attempts, _, err := r.runDevin(ctx, j, workdir, prompt, nil)
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

// issueActions is the optional JSON action block Devin appends to an issue
// reply when a human has accepted a proposed breakdown.
type issueActions struct {
	CreateIssues []domain.ProposedIssue `json:"create_issues"`
}

// maxCreatedIssues bounds how many issues one job may create.
const maxCreatedIssues = 10

// parseIssueActions extracts a trailing ```json action block from Devin's
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
// of a full review it runs a single Devin pass that analyzes the diff and
// proposes how to split the PR into smaller, reviewable pieces — posted as
// the pending comment. When j.SplitCreate is set (a human accepted a previous
// proposal) Devin may emit a create_issues block, which is turned into real
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

	out, attempts, _, err := r.runDevin(ctx, j, worktree, prompt, validateSplitOutput)
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

// ReconcileStuckJobs runs once at startup: tracker records still in
// "processing" belong to jobs killed mid-flight. Mark them failed — this
// makes them retryable via POST /jobs/retry — and fix the pending comment
// so the PR doesn't show "in progress" forever. Jobs still sitting queued
// in a durable backend re-run on their own. StuckProcessing is
// backend-aware: on Postgres it only returns orphaned records, so a replica
// starting up never fails jobs another replica is running.
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
			_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID,
				"Interrupted by a restart — re-mention me or retry this job via `POST /jobs/retry/"+j.JobKey+"`.")
			cancel()
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

// devinPreflightTimeout bounds the `devin models list` check so a hung CLI
// doesn't consume the whole attempt budget.
const devinPreflightTimeout = 60 * time.Second

// runDevin invokes the Devin CLI, retrying transient failures with exponential
// backoff. When validate is non-nil, a zero-exit run whose output fails
// validation is retried as a transient failure — a malformed response must
// never silently reach the caller (e.g. an unparseable review approving a PR).
// It returns the combined output, the number of attempts used, and the final
// error (if any).
// The lastDuration return is the final attempt's wall time (preflight +
// exec), recorded for calibration — pickup→finish includes retry backoffs
// that would otherwise depress the measured lines-per-minute rate.
func (r *ReviewUsecase) runDevin(ctx context.Context, j *domain.Review, worktree, prompt string, validate func(string) error) (string, int, time.Duration, error) {
	tmpDir, err := os.MkdirTemp("", "elyoraa-review-*")
	if err != nil {
		return "", 0, 0, err
	}
	defer os.RemoveAll(tmpDir)

	promptFile := filepath.Join(tmpDir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o644); err != nil {
		return "", 0, 0, err
	}

	// Credentials for agent-run git: the token file lives in the deny-listed
	// token dir so sandboxed commands cannot read it — only the excluded
	// repo-sync helper consumes it (via the host-locked askpass). Best-effort:
	// a token failure must not block the run itself.
	tokenFile := ""
	if r.gh != nil && r.cache != nil {
		if tok, terr := r.gh.InstallationToken(ctx, j.InstallationID); terr != nil {
			slog.Warn("devin run without git credentials: token mint failed", "job", j.JobKey, "error", terr)
		} else if tf, cleanupToken, werr := r.cache.WriteTokenFile(tok); werr != nil {
			slog.Warn("devin run without git credentials: token file", "job", j.JobKey, "error", werr)
		} else {
			tokenFile = tf
			defer cleanupToken()
		}
	}

	model := r.jobModel(j)
	args := []string{
		"-p",
		"--model", model,
		"--config", filepath.Join(r.cfg.DevinConfigDir, "devin-config.json"),
		"--respect-workspace-trust", "false",
		"--prompt-file", promptFile,
	}
	if r.cfg.DevinSandbox {
		// Autonomous is the only permission mode under --sandbox; the sandbox
		// enforces Read(...) deny rules at the OS level so credentials stay
		// hidden even from shell commands.
		args = append([]string{"--sandbox"}, args...)
	} else {
		args = append(args, "--permission-mode", r.cfg.DevinPermissionMode)
	}

	maxRetries := r.cfg.DevinMaxRetries
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

		if r.cfg.DevinPreflightCheck {
			if perr := r.devinPreflight(attemptCtx, worktree, model); perr != nil {
				runErr = fmt.Errorf("devin preflight: %w", perr)
				if rsn, det := deterministicDevinReason(runErr, runErr.Error()); det {
					transient, reason = false, rsn
				}
			}
		}
		if runErr == nil {
			log.Info("devin run started")
			out, runErr = r.execDevin(attemptCtx, worktree, args, tmpDir, tokenFile)
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
			return out, attempt, time.Since(attemptStart), nil
		}

		tail := tailString(out, maxOutputLogBytes)
		if !transient || attempt > maxRetries || ctx.Err() != nil {
			if reason == "canceled" {
				log.Info("devin run canceled", "output", tail)
			} else {
				log.Error("devin run failed", "error", runErr, "transient", transient, "reason", reason, "output", tail)
			}
			return out, attempt, time.Since(attemptStart), runErr
		}

		wait := r.backoff(attempt)
		log.Warn("devin attempt failed, will retry", "error", runErr, "reason", reason, "output", tail, "retry_in", wait.String())
		if !sleepCtx(ctx, wait) {
			return "", attempt, 0, ctx.Err()
		}
	}
}

// devinPreflight verifies the devin binary is usable: `devin models list` must
// succeed and list the model this job will run with.
func (r *ReviewUsecase) devinPreflight(ctx context.Context, worktree, model string) error {
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
	if model != "" && !strings.Contains(string(out), model) {
		return fmt.Errorf("configured model %q not present in `devin models list` output", model)
	}
	return nil
}

// execDevin runs the devin subprocess once and returns its combined output.
// The environment carries the git credential plumbing the agent-side
// repo-sync helper consumes — the token file itself is deny-hidden inside
// the sandbox, and GIT_TERMINAL_PROMPT=0 turns any sandboxed credentialed
// git operation (which cannot succeed) into a fast failure instead of a
// blocked prompt.
func (r *ReviewUsecase) execDevin(ctx context.Context, worktree string, args []string, tmpDir, tokenFile string) (string, error) {
	cmd := exec.CommandContext(ctx, r.cfg.DevinBin, args...)
	cmd.Dir = worktree
	env := os.Environ()
	env = append(env,
		"XDG_CACHE_HOME="+tmpDir,
		"TMPDIR="+tmpDir,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+r.cfg.GitAskpassPath,
		"ELYORAA_GIT_HOST="+cache.GitHost(r.cfg.GitBaseURL),
		"ELYORAA_WORKSPACE_BASE="+r.cfg.WorkspaceBase,
	)
	if tokenFile != "" {
		env = append(env, "ELYORAA_GIT_TOKEN_FILE="+tokenFile)
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// jobModel returns the Devin model for this job type: dedicated lighter
// models may handle routing/triage while reviews keep DEVIN_MODEL.
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
	return r.cfg.DevinModel
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
// overall timeout when DEVIN_ATTEMPT_TIMEOUT is unset.
func (r *ReviewUsecase) attemptTimeout(j *domain.Review) time.Duration {
	if r.cfg.DevinAttemptTimeout > 0 {
		return r.cfg.DevinAttemptTimeout
	}
	return r.jobTimeout(j)
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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && !exitErr.ProcessState.Exited() {
		// Killed by a signal before it could write a verdict — OOM,
		// container stop, or a crash. Almost always transient; same
		// retry path as malformed output.
		return true, exitErr.ProcessState.String()
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

	out, _, _, err := r.runDevin(ctx, j, worktree, prompt, validateReconcileOutput)
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

// syncScriptName is the generated helper the agent runs to refresh the bare
// clone its worktree belongs to. It lives in the (deny-listed) config dir —
// the script only needs to be executable, not readable.
const syncScriptName = "repo-sync.sh"

// repoSyncScript is the body of the generated helper. The script is the only
// credentialed git path an agent can reach: it runs outside the Devin sandbox
// (sandbox.excluded), fixes git's argv itself, and wipes the environment so
// agent-controlled variables (GIT_CONFIG_PARAMETERS URL rewrites, credential
// helpers, trace targets) cannot redirect the installation token. The
// askpass helper additionally answers credentials only for ELYORAA_GIT_HOST.
const repoSyncScript = `#!/bin/sh
# elyoraa repo-sync — refresh the local clone backing a worktree.
#
# Usage: repo-sync.sh [worktree-path]      (defaults to the current directory)
#
# Runs ` + "`git -C <bare> fetch --prune origin`" + ` with the server's credentials. The
# worktree shares the clone's object store and refs, so a synced clone is
# visible to ` + "`git log`" + `/` + "`git show`" + `/` + "`git checkout`" + ` here immediately.
set -eu

BASE=${ELYORAA_WORKSPACE_BASE:?ELYORAA_WORKSPACE_BASE is not set}
BASE=$(cd "$BASE" && pwd -P)

# Resolve the worktree (positional arg or cwd) and require it to live under
# the workspace.
WT=$(cd "${1:-.}" && pwd -P)
case "$WT" in
  "$BASE"/*) ;;
  *) echo "repo-sync: $WT is not inside $BASE" >&2; exit 1 ;;
esac

# Locate the shared git dir backing the worktree. PATH is pinned so no
# agent-provided binary can shadow git here.
COMMON=$(env -i PATH=/usr/local/bin:/usr/bin:/bin git -C "$WT" rev-parse --git-common-dir 2>/dev/null) || {
  echo "repo-sync: $WT is not a git worktree" >&2; exit 1
}
case "$COMMON" in
  /*) ;;
  *) COMMON="$WT/$COMMON" ;;
esac
BARE=$(cd "$COMMON" 2>/dev/null && pwd -P) || {
  echo "repo-sync: cannot resolve git dir $COMMON" >&2; exit 1
}

# Only managed bare clones (<base>/<owner>/<repo>.git) may be fetched — a
# repo created deeper in the tree could carry an attacker-controlled origin.
REL=${BARE#"$BASE"/}
case "$REL" in
  */*.git)
    PARENT=${REL%/*}
    case "$PARENT" in
      */*) echo "repo-sync: $BARE is not a managed bare clone" >&2; exit 1 ;;
    esac
    ;;
  *) echo "repo-sync: $BARE is not a managed bare clone" >&2; exit 1 ;;
esac

if env -i \
  PATH=/usr/local/bin:/usr/bin:/bin \
  HOME="${HOME:-/tmp}" \
  GIT_TERMINAL_PROMPT=0 \
  GIT_ASKPASS="${GIT_ASKPASS:?GIT_ASKPASS is not set}" \
  ELYORAA_GIT_TOKEN_FILE="${ELYORAA_GIT_TOKEN_FILE:?ELYORAA_GIT_TOKEN_FILE is not set}" \
  ELYORAA_GIT_HOST="${ELYORAA_GIT_HOST:?ELYORAA_GIT_HOST is not set}" \
  git -C "$BARE" fetch --prune --update-head-ok origin; then
  exit 0
fi

echo "repo-sync: fetch failed" >&2
exit 1
`

// syncCmd is the agent-side path to the generated repo-sync helper.
func (r *ReviewUsecase) syncCmd() string {
	return filepath.Join(r.cfg.DevinConfigDir, syncScriptName)
}

// WriteDevinConfig writes a locked devin-config.json inside dir, plus the
// repo-sync.sh helper the agent uses to refresh a worktree's clone. It omits
// the mem0 MCP server unless MEM0_API_KEY is set. Using a directory instead
// of a single file path makes bind-mounts safe against atomic rename-writes.
//
// The deny list hides credential material from the agent: Read(...) denies
// hide paths at the OS level when --sandbox is active, and block the read tool
// otherwise. tokenDir holds GitHub installation tokens; configDir holds this
// config (and the mem0 API key).
//
// Agent git access: read-only commands and repo-local mutations (checkout,
// switch, reset, restore, clean, worktree) are allowed — the mutations run
// OUTSIDE the sandbox via sandbox.excluded because a worktree's git metadata
// lives in the shared bare clone, outside the sandbox's writable workspace.
// The credentialed path is exactly one fixed-argv script. Direct remote git
// ops are denied outright rather than merely sandboxed: sandboxed commands
// still inherit GIT_ASKPASS/ELYORAA_GIT_TOKEN_FILE, and while the askpass
// host-lock keeps the token on ELYORAA_GIT_HOST, an org-scoped installation
// token could otherwise read OTHER repos the App is installed on. Commands
// that move shared refs or touch the credential machinery are denied too —
// in every mode.
func WriteDevinConfig(dir, tokenDir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	syncPath := filepath.Join(dir, syncScriptName)

	cfg := map[string]any{
		"permissions": map[string][]string{
			"allow": {
				"read",
				"grep",
				"glob",
				"mcp__mem0__*",
				"Exec(" + syncPath + ")",
				"Exec(git diff)",
				"Exec(git status)",
				"Exec(git log)",
				"Exec(git show)",
				"Exec(git rev-parse)",
				"Exec(git ls-files)",
				"Exec(git ls-tree)",
				"Exec(git for-each-ref)",
				"Exec(git cat-file)",
				"Exec(git blame)",
				"Exec(git describe)",
				"Exec(git merge-base)",
				"Exec(git shortlog)",
				"Exec(git name-rev)",
				"Exec(git branch)",
				"Exec(git tag)",
				"Exec(git remote)",
				"Exec(git checkout)",
				"Exec(git switch)",
				"Exec(git restore)",
				"Exec(git reset)",
				"Exec(git clean)",
				"Exec(git worktree)",
			},
			"deny": {
				"edit",
				"Write(**)",
				"Exec(curl)",
				"Exec(wget)",
				"Exec(gh)",
				// Every git path that can reach a remote or mint fresh
				// credentials — repo-sync is the only credentialed fetch.
				"Exec(git push)",
				"Exec(git fetch)",
				"Exec(git pull)",
				"Exec(git clone)",
				"Exec(git ls-remote)",
				"Exec(git submodule)",
				"Exec(git archive)",
				"Exec(git daemon)",
				"Exec(git instaweb)",
				"Exec(git svn)",
				"Exec(git send-email)",
				"Exec(git upload-pack)",
				"Exec(git receive-pack)",
				"Exec(git maintenance)",
				// Credential machinery and remote retargeting — never, in
				// any permission mode.
				"Exec(git credential)",
				"Exec(git config)",
				"Exec(git remote add)",
				"Exec(git remote set-url)",
				"Exec(git remote rm)",
				"Exec(git remote rename)",
				"Exec(git remote update)",
				"Exec(git remote prune)",
				"Exec(git remote set-head)",
				"Exec(git remote set-branches)",
				"Exec(git update-ref)",
				"Exec(git pack-refs)",
				"Exec(git gc)",
				"Exec(git prune)",
				"Exec(git reflog expire)",
				// Ref-creating forms of the otherwise-allowed commands —
				// shared refs live in the bare clone and are visible to
				// every concurrent job.
				"Exec(git checkout -b)",
				"Exec(git checkout -B)",
				"Exec(git checkout -t)",
				"Exec(git checkout --track)",
				"Exec(git switch -c)",
				"Exec(git switch -C)",
				"Exec(git switch -t)",
				"Exec(git switch --track)",
				"Exec(git switch --create)",
				"Exec(git switch --force-create)",
				// Shared-ref mutation: `git branch`/`git tag` list safely, but
				// the flag forms below delete/move/copy refs other jobs use.
				// (Positional `git branch <name>`/`git tag <name>` still create
				// refs — self-healing: fetch --prune removes branch refs that
				// don't exist on origin.)
				"Exec(git branch -d)",
				"Exec(git branch -D)",
				"Exec(git branch --delete)",
				"Exec(git branch -m)",
				"Exec(git branch -M)",
				"Exec(git branch --move)",
				"Exec(git branch -c)",
				"Exec(git branch -C)",
				"Exec(git branch --copy)",
				"Exec(git branch -f)",
				"Exec(git branch --force)",
				"Exec(git branch -u)",
				"Exec(git branch --set-upstream-to)",
				"Exec(git branch --unset-upstream)",
				"Exec(git tag -d)",
				"Exec(git tag -D)",
				"Exec(git tag --delete)",
				"Exec(git tag -f)",
				"Exec(git tag --force)",
				// `git worktree` is allowed for `list` — the subcommands below
				// create/destroy worktrees shared with other jobs.
				"Exec(git worktree add)",
				"Exec(git worktree remove)",
				"Exec(git worktree move)",
				"Exec(git worktree repair)",
				"Exec(git worktree lock)",
				"Exec(git worktree unlock)",
				"Exec(git worktree prune)",
				// Local plumbing that still writes shared refs or worktree
				// linkage.
				"Exec(git symbolic-ref)",
				"Exec(git bisect)",
				// History writers the read-only policy never needs.
				"Exec(git commit)",
				"Exec(git merge)",
				"Exec(git rebase)",
				"Exec(git cherry-pick)",
				"Exec(git revert)",
				"Exec(git am)",
				"Exec(git apply)",
				"Exec(git stash)",
				"Exec(git replace)",
				"Fetch(**)",
				// Credentials and secrets the agent must never see —
				// hidden at OS level under --sandbox.
				"Read(/run/secrets/**)",
				"Read(" + tokenDir + "/**)",
				"Read(" + dir + "/**)",
				"Read(~/.config/devin/**)",
				"Read(~/.local/share/devin/**)",
				"Read(/app/git-askpass.sh)",
				"Read(**/.env)",
				"Read(**/.env.*)",
				"Read(**/.ssh/**)",
				"Read(**/id_rsa*)",
				"Read(**/id_ed25519*)",
				"Read(**/.gitconfig)",
				"Read(**/*.pem)",
			},
		},
		"sandbox": map[string]any{
			"excluded": map[string][]string{
				"allow": {
					// The only credentialed git path: env-scrubbed,
					// fixed-argv `git -C <bare> fetch --prune origin`.
					"Exec(" + syncPath + ")",
					// Repo-scoped, credential-free mutations the sandbox's
					// read-only view of the shared git dir would reject.
					"Exec(git checkout)",
					"Exec(git switch)",
					"Exec(git restore)",
					"Exec(git reset)",
					"Exec(git clean)",
					"Exec(git worktree)",
				},
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
	if err := os.WriteFile(filepath.Join(dir, "devin-config.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(syncPath, []byte(repoSyncScript), 0o755)
}
