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

	prompt, err := r.renderPrompt(&j, prInfo)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not render prompt.")
		r.markFailed(ctx, j, "render prompt")
		return attempts, fmt.Errorf("render prompt: %w", err)
	}

	out, attempts, err := r.runDevin(ctx, &j, worktree, prompt)
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

	review, err := r.parseOutput(out)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not parse Devin output.")
		r.markFailed(ctx, j, "parse output")
		return attempts, fmt.Errorf("parse output: %w", err)
	}

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

	prompt, err := r.renderIssuePrompt(j)
	if err != nil {
		r.updatePending(ctx, j, "Issue analysis failed: could not render prompt.")
		r.markFailed(ctx, *j, "render issue prompt")
		return attempts, fmt.Errorf("render issue prompt: %w", err)
	}

	out, attempts, err := r.runDevin(ctx, j, workdir, prompt)
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

	// Post the raw output as an issue comment.
	response := strings.TrimSpace(out)
	if response == "" {
		response = "No output from analysis."
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

func (r *ReviewUsecase) renderIssuePrompt(j *domain.Review) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "issue.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"Owner":          j.Owner,
		"Repo":           j.Repo,
		"IssueNumber":    j.PRNumber,
		"Title":          j.Title,
		"Body":           j.Body,
		"TriggerComment": j.TriggerComment,
		"BotHandle":      r.cfg.BotHandle,
		"RepoAvailable":  j.RepoAvailable,
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
// backoff. It returns the combined output, the number of attempts used, and
// the final error (if any).
func (r *ReviewUsecase) runDevin(ctx context.Context, j *domain.Review, worktree, prompt string) (string, int, error) {
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
			if runErr != nil {
				transient, reason = classifyDevinError(runErr, attemptCtx, out)
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

func (r *ReviewUsecase) parseOutput(out string) (*domain.Output, error) {
	matches := jsonBlockRegex.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return &domain.Output{Summary: strings.TrimSpace(out), Comments: nil}, nil
	}
	last := matches[len(matches)-1][1]
	var result domain.Output
	if err := json.Unmarshal([]byte(last), &result); err != nil {
		return &domain.Output{Summary: strings.TrimSpace(out), Comments: nil}, nil
	}
	return &result, nil
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
