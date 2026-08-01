package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/cache"
	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	gh "github.com/andikapradanaarif/elyoraa/internal/github"
	"github.com/andikapradanaarif/elyoraa/internal/job"
)

// Reviewer orchestrates a single PR review.
type Reviewer struct {
	cfg   *config.Config
	cache *cache.Cache
	gh    *gh.Client
	d     *dedup.Dedup
}

// New creates a Reviewer.
func New(cfg *config.Config, c *cache.Cache, g *gh.Client, d *dedup.Dedup) *Reviewer {
	return &Reviewer{cfg: cfg, cache: c, gh: g, d: d}
}

// Process implements queue.Processor.
func (r *Reviewer) Process(ctx context.Context, j job.Review) (err error) {
	defer func() {
		r.d.Done(j.JobKey)
		if p := recover(); p != nil {
			slog.Error("reviewer panic", "job", j.JobKey, "panic", p)
			r.updatePending(ctx, &j, "Review failed due to an internal error.")
			panic(p)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	token, err := r.gh.InstallationToken(ctx, j.InstallationID)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not authenticate to GitHub.")
		return fmt.Errorf("installation token: %w", err)
	}

	if err := r.cache.Fetch(ctx, j.Owner, j.Repo, token); err != nil {
		r.updatePending(ctx, &j, "Review failed: could not fetch repository.")
		return fmt.Errorf("cache fetch: %w", err)
	}

	worktree, cleanup, err := r.cache.AddWorktree(ctx, j.Owner, j.Repo, j.PRNumber, j.HeadSHA)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not prepare worktree.")
		return fmt.Errorf("add worktree: %w", err)
	}
	defer cleanup()

	prInfo, err := r.gh.GetPR(ctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, r.cfg.MaxPatchBytes, r.cfg.MaxChangedFiles)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not fetch PR details.")
		return fmt.Errorf("get pr: %w", err)
	}

	if prInfo.IsDraft {
		r.updatePending(ctx, &j, "Review skipped: draft PR.")
		return nil
	}

	prompt, err := r.renderPrompt(&j, prInfo)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not render prompt.")
		return fmt.Errorf("render prompt: %w", err)
	}

	out, err := r.runDevin(ctx, worktree, prompt)
	if err != nil {
		if ctx.Err() == context.Canceled {
			r.updatePending(ctx, &j, "Review cancelled: a newer push replaced this one.")
			return nil
		}
		r.updatePending(ctx, &j, "Review failed: Devin review run failed.")
		return fmt.Errorf("run devin: %w", err)
	}

	review, err := r.parseOutput(out)
	if err != nil {
		r.updatePending(ctx, &j, "Review failed: could not parse Devin output.")
		return fmt.Errorf("parse output: %w", err)
	}

	// Map comments to positions. Drop unmappable comments into the summary.
	fileComments := r.gh.MapCommentsToPositions(prInfo.Files, review.Comments)
	dropped := make([]job.Comment, 0)
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

	if err := r.gh.PostReview(ctx, j.InstallationID, j.Owner, j.Repo, j.PRNumber, summary, fileComments, j.HeadSHA); err != nil {
		r.updatePending(ctx, &j, "Review failed: could not post review comments.")
		return fmt.Errorf("post review: %w", err)
	}

	if j.PendingCommentID != 0 {
		_ = r.gh.DeleteComment(ctx, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID)
	}

	return nil
}

func (r *Reviewer) updatePending(ctx context.Context, j *job.Review, body string) {
	if j.PendingCommentID == 0 {
		return
	}
	body = body + "\n\n<!-- elyoraa-in-progress -->"
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.gh.EditComment(ctx2, j.InstallationID, j.Owner, j.Repo, j.PendingCommentID, body)
}

func (r *Reviewer) renderPrompt(j *job.Review, prInfo *gh.PRInfo) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.DevinConfigPath, "..", "prompts", "review.md"))
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

func (r *Reviewer) runDevin(ctx context.Context, worktree, prompt string) (string, error) {
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
		"--config", r.cfg.DevinConfigPath,
		"--prompt-file", promptFile,
	)
	cmd.Dir = worktree
	env := os.Environ()
	env = append(env, "XDG_CACHE_HOME="+tmpDir, "TMPDIR="+tmpDir)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

var jsonBlockRegex = regexp.MustCompile("(?s)```json\\s*(.*?)\\s*```")

func (r *Reviewer) parseOutput(out string) (*job.Output, error) {
	matches := jsonBlockRegex.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return &job.Output{Summary: strings.TrimSpace(out), Comments: nil}, nil
	}
	last := matches[len(matches)-1][1]
	var result job.Output
	if err := json.Unmarshal([]byte(last), &result); err != nil {
		return &job.Output{Summary: strings.TrimSpace(out), Comments: nil}, nil
	}
	return &result, nil
}

// WriteDevinConfig writes a locked /app/devin-config.json. It omits the mem0 MCP
// server unless MEM0_API_KEY is set.
func WriteDevinConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
				"command": "npx",
				"args":    []string{"-y", "mem0-mcp"},
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
	return os.WriteFile(path, b, 0o644)
}
