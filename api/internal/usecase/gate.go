package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"text/template"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
)

// triageGate runs the size-triage pass and returns "review" or "split".
// The pass runs under a child context capped at GATE_TIMEOUT: a triage
// timeout or agent failure falls back to the deterministic verdict (split —
// the nomination that created this job), so a nominated-oversize PR is never
// silently reviewed. Only cancellation of the PARENT job context — a newer
// push or explicit request superseding this job — returns an error.
func (r *ReviewUsecase) triageGate(ctx context.Context, j *domain.Review, worktree string, prInfo *github.PRInfo, gen func(string) bool) (string, error) {
	size := refinedEstimate(prInfo, j, r.cfg.PRLinesPerMinute, gen)

	prompt, err := r.renderGatePrompt(j, prInfo, size)
	if err != nil {
		slog.Warn("gate: render prompt failed, falling back to split", "job", j.JobKey, "error", err)
		r.gateFallback()
		return "split", nil
	}

	tctx, tcancel := context.WithTimeout(ctx, r.cfg.GateTimeout)
	out, _, _, err := r.runDevin(tctx, j, worktree, prompt, validateGateOutput)
	tcancel()
	if err != nil {
		if ctx.Err() != nil {
			// Parent job context cancelled/deadlined — real cancellation,
			// not a triage failure. Don't fall back into work the job
			// context can no longer support.
			return "", queue.ErrJobCancelled
		}
		// Triage-only failure or timeout: the deterministic verdict stands.
		slog.Warn("gate: triage failed, falling back to split", "job", j.JobKey, "error", err)
		r.gateFallback()
		return "split", nil
	}

	verdict, verr := parseGateVerdict(out)
	if verr != nil {
		slog.Warn("gate: unparseable verdict, falling back to split", "job", j.JobKey, "error", verr)
		r.gateFallback()
		return "split", nil
	}
	if r.metrics != nil {
		r.metrics.IncGateVerdict(verdict.Verdict)
	}
	slog.Info("gate verdict", "job", j.JobKey, "verdict", verdict.Verdict, "reason", verdict.Reason)
	return verdict.Verdict, nil
}

// gateFallback counts a deterministic-fallback split distinctly from an
// agent-chosen one — it measures how often the triage pass actually failed.
func (r *ReviewUsecase) gateFallback() {
	if r.metrics != nil {
		r.metrics.IncGateVerdict("fallback_split")
	}
}

// gateVerdict is the gate agent's structured output.
type gateVerdict struct {
	Verdict string `json:"verdict"` // "review" | "split"
	Reason  string `json:"reason"`
}

func parseGateVerdict(out string) (gateVerdict, error) {
	matches := jsonBlockRegex.FindAllStringSubmatchIndex(out, -1)
	if len(matches) == 0 {
		return gateVerdict{}, errors.New("no verdict block")
	}
	last := matches[len(matches)-1]
	var v gateVerdict
	if err := json.Unmarshal([]byte(out[last[2]:last[3]]), &v); err != nil {
		return gateVerdict{}, fmt.Errorf("unparseable verdict block: %w", err)
	}
	switch v.Verdict {
	case "review", "split":
		return v, nil
	}
	return gateVerdict{}, fmt.Errorf("unknown verdict %q", v.Verdict)
}

// validateGateOutput makes a missing/malformed verdict a retriable failure.
func validateGateOutput(out string) error {
	_, err := parseGateVerdict(out)
	return err
}

func (r *ReviewUsecase) renderGatePrompt(j *domain.Review, prInfo *github.PRInfo, size prSize) (string, error) {
	tmpl, err := template.ParseFiles(filepath.Join(r.cfg.PromptsDir, "gate.md"))
	if err != nil {
		return "", err
	}

	data := map[string]any{
		"Owner":         j.Owner,
		"Repo":          j.Repo,
		"RepoKey":       fmt.Sprintf("%s/%s", j.Owner, j.Repo),
		"PRNumber":      j.PRNumber,
		"Title":         j.Title,
		"Body":          j.Body,
		"Author":        j.Author,
		"HeadSHA":       j.HeadSHA,
		"BaseSHA":       j.BaseSHA,
		"BotHandle":     r.cfg.BotHandle,
		"Size":          size,
		"BudgetMinutes": jobBudget(r.cfg, *j),
		"Files":         prInfo.Files,
		"Truncated":     prInfo.FilesTruncated,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
