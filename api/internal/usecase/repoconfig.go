package usecase

import (
	"context"
	"log/slog"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository/cache"
)

// repoConfigFile is the per-repo config path, read from the default branch.
const repoConfigFile = ".elyoraa.yml"

// loadRepoConfig reads and validates the repository's .elyoraa.yml. The file
// comes from the bare clone's HEAD — the default branch — so a PR can never
// supply its own config. Any failure (repo not cloned yet, file missing,
// invalid YAML/values) yields the zero config, meaning global defaults apply.
// The read is a local `git show` — sub-millisecond — so we do it per webhook
// rather than caching and going stale.
func loadRepoConfig(ctx context.Context, c *cache.Cache, owner, repo string) domain.RepoConfig {
	var rc domain.RepoConfig
	if c == nil {
		return rc
	}
	raw, err := c.ReadRepoFile(ctx, owner, repo, repoConfigFile)
	if err != nil {
		return rc
	}
	return parseRepoConfig(raw, owner+"/"+repo)
}

// parseRepoConfig validates raw .elyoraa.yml bytes; invalid YAML or unknown
// enum values degrade to (parts of) the zero config, i.e. global defaults.
func parseRepoConfig(raw []byte, repo string) domain.RepoConfig {
	var rc domain.RepoConfig
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		slog.Warn("invalid "+repoConfigFile+", using defaults", "repo", repo, "error", err)
		return domain.RepoConfig{}
	}
	if rc.MentionRouter != "" && rc.MentionRouter != "keywords" && rc.MentionRouter != "agent" {
		slog.Warn(repoConfigFile+": unknown mention_router, using global", "repo", repo, "value", rc.MentionRouter)
		rc.MentionRouter = ""
	}
	if rc.SizeGateMode != "" && rc.SizeGateMode != "deterministic" && rc.SizeGateMode != "agent" {
		slog.Warn(repoConfigFile+": unknown size_gate_mode, using global", "repo", repo, "value", rc.SizeGateMode)
		rc.SizeGateMode = ""
	}
	return rc
}

// repoConfig resolves the repo's effective .elyoraa.yml, cloning the bare
// repo on demand when needed so `enabled: false` applies from a repo's
// first webhook rather than only after a job has already run. The clone is
// bounded by the request context — a huge repo may exceed it, in which case
// this delivery uses defaults and the redelivery sees the finished clone.
func (u *WebhookUsecase) repoConfig(ctx context.Context, owner, repo string, installationID int64) domain.RepoConfig {
	if u.cache != nil && u.gh != nil && !u.cache.IsCloned(owner, repo) {
		if token, err := u.gh.InstallationToken(ctx, installationID); err == nil {
			_ = u.cache.EnsureBareClone(ctx, owner, repo, token)
		}
	}
	return loadRepoConfig(ctx, u.cache, owner, repo)
}

// repoEnabled reports whether the repo config disables elyoraa entirely.
func repoEnabled(rc domain.RepoConfig) bool {
	return rc.Enabled == nil || *rc.Enabled
}

// mentionRouterFor resolves the effective router for a repo.
func mentionRouterFor(cfg *config.Config, rc domain.RepoConfig) string {
	if rc.MentionRouter != "" {
		return rc.MentionRouter
	}
	return cfg.MentionRouter
}

// sizeGateModeFor resolves the effective size-gate mode for a repo.
func sizeGateModeFor(cfg *config.Config, rc domain.RepoConfig) string {
	if rc.SizeGateMode != "" {
		return rc.SizeGateMode
	}
	return cfg.SizeGateMode
}

// budgetMinutesFor resolves the review-time budget.
func budgetMinutesFor(cfg *config.Config, rc domain.RepoConfig) int {
	if rc.BudgetMinutes > 0 {
		return rc.BudgetMinutes
	}
	return cfg.PRReviewBudgetMinutes
}

// linesPerMinuteFor resolves the pre-calibration sizing rate.
func linesPerMinuteFor(cfg *config.Config, rc domain.RepoConfig) int {
	if rc.LinesPerMinute > 0 {
		return rc.LinesPerMinute
	}
	return cfg.PRLinesPerMinute
}

// skipLabelFor resolves the opt-out label name.
func skipLabelFor(cfg *config.Config, rc domain.RepoConfig) string {
	if rc.SkipLabel != "" {
		return rc.SkipLabel
	}
	return cfg.PRSkipLabel
}

// jobBudget returns the budget recorded on the job, else the global default.
func jobBudget(cfg *config.Config, j domain.Review) int {
	if j.BudgetMinutes > 0 {
		return j.BudgetMinutes
	}
	return cfg.PRReviewBudgetMinutes
}

// ignoredPath matches .elyoraa.yml ignored_paths entries against a
// repo-relative path, approximating .gitignore rules: a bare name
// ("vendor", "*.pb.go") matches a file or directory component at any depth,
// a slash-bearing pattern is anchored to the repo root, "**" spans zero or
// more path segments, and a matched directory ignores its whole subtree.
func ignoredPath(patterns []string, filePath string) bool {
	segs := strings.Split(filePath, "/")
	for _, p := range patterns {
		p = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(p), "/"), "/")
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			for _, s := range segs {
				if ok, _ := path.Match(p, s); ok {
					return true
				}
			}
			continue
		}
		if matchGlobPath(strings.Split(p, "/"), segs) {
			return true
		}
	}
	return false
}

// matchGlobPath matches path segments against glob segments; "**" spans
// zero or more segments and exhausting the pattern means a leading
// directory matched (whose subtree is ignored).
func matchGlobPath(pat, segs []string) bool {
	if len(pat) == 0 {
		return true
	}
	if len(segs) == 0 {
		return false
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchGlobPath(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	ok, _ := path.Match(pat[0], segs[0])
	return ok && matchGlobPath(pat[1:], segs[1:])
}
