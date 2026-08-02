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
func (r *ReviewUsecase) Process(ctx context.Context, j domain.Review) (err error) {
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

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if j.IsIssue() {
		return r.processIssue(ctx, &j)
	}

	token, err := r.gh.InstallationToken(ctx, j.InstallationID)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not authenticate to GitHub.")
		r.markFailed(ctx, j, "installation token")
		return fmt.Errorf("installation token: %w", err)
	}

	if err := r.cache.Fetch(ctx, j.Owner, j.Repo, token); err != nil {
		if errors.Is(err, cache.ErrRepoNotCloned) {
			r.updatePending(ctx, &j, fmt.Sprintf(
				"Review skipped: repository `%s/%s` is not cloned on this server. "+
					"Clone it first at `%s/%s/%s.git`.",
				j.Owner, j.Repo, r.cfg.WorkspaceBase, j.Owner, j.Repo,
			))
			r.markStatus(ctx, j, "skipped")
			return nil
		}
		r.updatePending(ctx, &j, "Review failed: could not fetch repository.")
		r.markFailed(ctx, j, "cache fetch")
		return fmt.Errorf("cache fetch: %w", err)
	}

	worktree, cleanup, err := r.cache.AddWorktree(ctx, j.Owner, j.Repo, j.PRNumber, j.HeadSHA)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not prepare worktree.")
		r.markFailed(ctx, j, "add worktree")
		return fmt.Errorf("add worktree: %w", err)
	}
	defer cleanup()

	prInfo, err := r.gh.GetPR(ctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, r.cfg.MaxPatchBytes, r.cfg.MaxChangedFiles)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not fetch PR details.")
		r.markFailed(ctx, j, "get pr")
		return fmt.Errorf("get pr: %w", err)
	}

	if prInfo.IsDraft {
		r.updatePending(ctx, &j, "Review skipped: draft PR.")
		r.markStatus(ctx, j, "cancelled")
		return nil
	}

	prompt, err := r.renderPrompt(&j, prInfo)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not render prompt.")
		r.markFailed(ctx, j, "render prompt")
		return fmt.Errorf("render prompt: %w", err)
	}

	out, err := r.runDevin(ctx, worktree, prompt)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(ctx, &j, "Review cancelled: a newer push replaced this one.")
			r.markStatus(ctx, j, "cancelled")
			return nil
		}
		r.updatePending(ctx, &j, "Review failed: Devin review run failed.")
		r.markFailed(ctx, j, "run devin")
		return fmt.Errorf("run devin: %w", err)
	}

	review, err := r.parseOutput(out)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not parse Devin output.")
		r.markFailed(ctx, j, "parse output")
		return fmt.Errorf("parse output: %w", err)
	}

	// Map comments to positions. Drop unmappable comments into the summary.
	fileComments := r.gh.MapCommentsToPositions(prInfo.Files, review.Comments)
	dropped := make([]domain.Comment, 0)
	used := make(map[string]bool)
	for _, c := range review.Comments {
		k := fmt.Sprintf("%s:%d", c.Path, c.Line)
		for _, fc := range fileComments {
			if fmt.Sprintf("%s:%d", fc.Path, fc.Position) == k { // approximate
				used[k] = true
				break
			}
		}
		if !used[k] {
			dropped = append(dropped, c)
		}
	}

	summary := review.Summary
	if len(dropped) > 0 {
		var sb strings.Builder
		sb.WriteString(summary)
		sb.WriteString("\n\n### Comments that could not be mapped to changed lines\n")
		for _, c := range dropped {
			sb.WriteString(fmt.Sprintf("- **%s** `%s:%d`: %s\n", strings.ToUpper(c.Severity), c.Path, c.Line, c.Body))
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
				editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, commentBody)
				cancel()
			} else {
				postCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, _ = r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, commentBody)
				cancel()
			}
			r.markStatus(ctx, j, "completed")
			return nil
		}
		r.updatePending(ctx, &j, "Review failed: could not post review comments.")
		r.markFailed(ctx, j, "post review")
		return fmt.Errorf("post review: %w", err)
	}

	if j.PendingCommentID != 0 {
		_ = r.gh.DeleteComment(ctx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID)
	}

	r.markStatus(ctx, j, "completed")
	return nil
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
func (r *ReviewUsecase) processIssue(ctx context.Context, j *domain.Review) error {
	pendingBody := "Analyzing issue…\n\n<!-- elyoraa-in-progress -->"
	pendingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
			return fmt.Errorf("mkdir temp: %w", err)
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
		return fmt.Errorf("render issue prompt: %w", err)
	}

	out, err := r.runDevin(ctx, workdir, prompt)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(ctx, j, "Issue analysis cancelled.")
			r.markStatus(ctx, *j, "cancelled")
			return nil
		}
		r.updatePending(ctx, j, "Issue analysis failed: Devin run failed.")
		r.markFailed(ctx, *j, "run devin issue")
		return fmt.Errorf("run devin issue: %w", err)
	}

	// Post the raw output as an issue comment.
	response := strings.TrimSpace(out)
	if response == "" {
		response = "No output from analysis."
	}

	if j.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = r.gh.EditComment(editCtx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, response+"\n\n<!-- elyoraa-in-progress -->")
		cancel()
	} else {
		postCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = r.gh.PostPendingComment(postCtx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, response)
		cancel()
	}

	r.markStatus(ctx, *j, "completed")
	return nil
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
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

func (r *ReviewUsecase) runDevin(ctx context.Context, worktree, prompt string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "elyoraa-review-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	promptFile := filepath.Join(tmpDir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o644); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, r.cfg.DevinBin,
		"--sandbox",
		"--permission-mode", "autonomous",
		"--model", r.cfg.DevinModel,
		"--config", filepath.Join(r.cfg.DevinConfigDir, "devin-config.json"),
		"--prompt-file", promptFile,
	)
	cmd.Dir = worktree
	env := os.Environ()
	env = append(env, "XDG_CACHE_HOME="+tmpDir, "TMPDIR="+tmpDir)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("devin command failed", "bin", r.cfg.DevinBin, "workdir", worktree, "error", err, "output", string(out))
		return string(out), err
	}
	return string(out), nil
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
