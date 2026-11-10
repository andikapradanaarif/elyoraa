package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
	"github.com/andikapradanaarif/elyoraa/internal/repository"
	"github.com/andikapradanaarif/elyoraa/internal/repository/cache"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
	go_github "github.com/google/go-github/v88/github"
)

// ErrQueueFull is returned when the queue is at capacity.
var ErrQueueFull = errors.New("queue full")

// ErrDuplicate is returned when the same review is already in flight.
var ErrDuplicate = errors.New("duplicate review in flight")

// ErrRateLimited is returned when an author exceeds the mention rate limit.
var ErrRateLimited = errors.New("mention rate limited")

// ErrCapped is returned when the repo exceeds the daily job cap.
var ErrCapped = errors.New("daily job cap reached")

// ErrNotForUs is returned when the webhook is not an event the bot handles.
var ErrNotForUs = errors.New("event not handled")

type contextKey string

const (
	eventTypeKey  contextKey = "event_type"
	deliveryIDKey contextKey = "delivery_id"
)

// WithEventType returns a context carrying the GitHub webhook event type.
func WithEventType(ctx context.Context, eventType string) context.Context {
	return context.WithValue(ctx, eventTypeKey, eventType)
}

// WithDeliveryID returns a context carrying the GitHub webhook delivery ID.
func WithDeliveryID(ctx context.Context, deliveryID string) context.Context {
	return context.WithValue(ctx, deliveryIDKey, deliveryID)
}

// getPRFunc fetches per-file PR data for the size gate — github.Client.GetPR
// in production, a stub in tests.
type getPRFunc func(ctx context.Context, installationID int64, owner, repo string, number, maxPatchBytes, maxFiles int) (*github.PRInfo, error)

// WebhookUsecase validates and routes incoming GitHub webhooks.
type WebhookUsecase struct {
	cfg        *config.Config
	gh         *github.Client
	getPRStats getPRFunc
	d          dedup.Interface
	q          *queue.Queue
	store      repository.WebhookStore
	limiter    *rateLimiter
	cache      *cache.Cache
	metrics    *metrics.Registry
}

// NewWebhookUsecase creates a WebhookUsecase.
func NewWebhookUsecase(cfg *config.Config, client *github.Client, d dedup.Interface, q *queue.Queue, store repository.WebhookStore, c *cache.Cache, m *metrics.Registry) *WebhookUsecase {
	return &WebhookUsecase{
		cfg:        cfg,
		gh:         client,
		getPRStats: client.GetPR,
		d:          d,
		q:          q,
		store:      store,
		limiter:    newRateLimiter(cfg.MentionRateLimit, cfg.MentionRateWindow),
		cache:      c,
		metrics:    m,
	}
}

// Handle validates a webhook, records the event, and submits a review job to the queue.
// The GitHub event type and delivery ID are read from the supplied context (see WithEventType / WithDeliveryID).
func (u *WebhookUsecase) Handle(ctx context.Context, body []byte, signature string) (domain.Review, error) {
	deliveryID := u.deliveryID(ctx)

	if err := u.gh.VerifyWebhookSignature(body, signature, u.cfg.GitHubWebhookSecret); err != nil {
		_ = u.store.UpdateStatus(ctx, deliveryID, "failed", fmt.Sprintf("signature: %v", err))
		return domain.Review{}, fmt.Errorf("signature: %w", err)
	}

	eventType, _ := ctx.Value(eventTypeKey).(string)

	switch eventType {
	case "pull_request":
		return u.handlePullRequest(ctx, deliveryID, body)
	case "issue_comment":
		return u.handleIssueComment(ctx, deliveryID, body)
	case "pull_request_review":
		return u.handlePullRequestReview(ctx, deliveryID, body)
	case "pull_request_review_comment":
		return u.handlePullRequestReviewComment(ctx, deliveryID, body)
	case "issues":
		return u.handleIssues(ctx, deliveryID, body)
	case "reaction":
		return u.handleReaction(ctx, deliveryID, body)
	default:
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  eventType,
			Status:     "ignored",
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, ErrNotForUs
	}
}

func (u *WebhookUsecase) handlePullRequest(ctx context.Context, deliveryID string, body []byte) (domain.Review, error) {
	event, err := go_github.ParseWebHook("pull_request", body)
	if err != nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  "pull_request",
			Status:     "failed",
			Error:      fmt.Sprintf("parse: %v", err),
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, fmt.Errorf("parse: %w", err)
	}

	prEvent, ok := event.(*go_github.PullRequestEvent)
	if !ok {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  "pull_request",
			Status:     "ignored",
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	action := prEvent.GetAction()
	// Only review when a PR is opened, reopened, or marked ready for review.
	// synchronize (new pushes) is ignored unless the author explicitly requests a rereview.
	if action != "opened" && action != "reopened" && action != "ready_for_review" {
		_ = u.store.Record(ctx, u.prWebhookEvent(deliveryID, prEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	pr := prEvent.GetPullRequest()
	if pr.GetDraft() {
		_ = u.store.Record(ctx, u.prWebhookEvent(deliveryID, prEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	repo := prEvent.GetRepo()
	if repo == nil || repo.GetFullName() == "" {
		_ = u.store.Record(ctx, u.prWebhookEvent(deliveryID, prEvent, body, "failed", "missing repository"))
		return domain.Review{}, fmt.Errorf("missing repository")
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	if len(ownerRepo) != 2 {
		_ = u.store.Record(ctx, u.prWebhookEvent(deliveryID, prEvent, body, "failed", "invalid repository"))
		return domain.Review{}, fmt.Errorf("invalid repository")
	}
	owner, repoName := ownerRepo[0], ownerRepo[1]
	rc := loadRepoConfig(ctx, u.cache, owner, repoName)

	review := domain.Review{
		DeliveryID:     deliveryID,
		Owner:          owner,
		Repo:           repoName,
		PRNumber:       pr.GetNumber(),
		HeadSHA:        pr.GetHead().GetSHA(),
		BaseSHA:        pr.GetBase().GetSHA(),
		InstallationID: prEvent.GetInstallation().GetID(),
		Author:         pr.GetUser().GetLogin(),
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		PRLabels:       labelNames(pr.Labels),
		EnqueuedAt:     time.Now(),
	}

	// The pull_request payload already carries the size counters — the gate
	// is free until the raw count nominates oversize, which pays one GetPR
	// to exclude generated files before the job is typed.
	u.applySizeGate(ctx, &review, rc, nil, sizeStats{
		additions:    pr.GetAdditions(),
		deletions:    pr.GetDeletions(),
		changedFiles: pr.GetChangedFiles(),
	})

	return u.enqueueReview(ctx, review, u.prWebhookEvent(deliveryID, prEvent, body, "received", ""), rc)
}

// labelNames flattens the webhook payload's label objects to names.
func labelNames(labels []*go_github.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		if n := l.GetName(); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func (u *WebhookUsecase) handleIssueComment(ctx context.Context, deliveryID string, body []byte) (domain.Review, error) {
	event, err := go_github.ParseWebHook("issue_comment", body)
	if err != nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  "issue_comment",
			Status:     "failed",
			Error:      fmt.Sprintf("parse: %v", err),
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, fmt.Errorf("parse: %w", err)
	}

	cmEvent, ok := event.(*go_github.IssueCommentEvent)
	if !ok {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  "issue_comment",
			Status:     "ignored",
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	action := cmEvent.GetAction()
	if action != "created" && action != "edited" {
		slog.Info("issue_comment ignored", "action", action, "delivery", deliveryID)
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	issue := cmEvent.GetIssue()
	if issue == nil {
		slog.Info("issue_comment ignored: no issue", "delivery", deliveryID)
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	comment := cmEvent.GetComment()
	if comment == nil {
		slog.Info("issue_comment ignored: no comment", "delivery", deliveryID)
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	// Ignore comments from bots (including ourselves) to prevent feedback loops.
	commentAuthor := strings.ToLower(comment.GetUser().GetLogin())
	if strings.HasSuffix(commentAuthor, "[bot]") || strings.Contains(commentAuthor, "github-actions") {
		slog.Info("issue_comment ignored: bot author", "delivery", deliveryID, "author", commentAuthor)
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	if !u.isBotMentioned(comment.GetBody()) {
		slog.Info("issue_comment ignored: bot not mentioned", "delivery", deliveryID, "handle", u.cfg.BotHandle)
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	slog.Info("issue_comment: bot mentioned", "delivery", deliveryID, "is_pr", issue.IsPullRequest(), "issue_number", issue.GetNumber())

	repo := cmEvent.GetRepo()
	if repo == nil || repo.GetFullName() == "" {
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "failed", "missing repository"))
		return domain.Review{}, fmt.Errorf("missing repository")
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	if len(ownerRepo) != 2 {
		_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "failed", "invalid repository"))
		return domain.Review{}, fmt.Errorf("invalid repository")
	}
	owner, repoName := ownerRepo[0], ownerRepo[1]

	issueNumber := issue.GetNumber()
	instID := cmEvent.GetInstallation().GetID()
	rc := loadRepoConfig(ctx, u.cache, owner, repoName)
	labels := labelNames(issue.Labels)

	// If the comment is on a PR, trigger a rereview.
	if issue.IsPullRequest() {
		prCtx, cancel := context.WithTimeout(ctx, u.cfg.CommentTimeout)
		prInfo, err := u.gh.GetPR(prCtx, instID, owner, repoName, issueNumber, 0, u.cfg.MaxChangedFiles)
		cancel()
		if err != nil {
			_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "failed", fmt.Sprintf("get pr: %v", err)))
			return domain.Review{}, fmt.Errorf("get pr: %w", err)
		}

		if prInfo.IsDraft {
			_ = u.store.Record(ctx, u.commentWebhookEvent(deliveryID, cmEvent, body, "ignored", ""))
			return domain.Review{}, nil
		}

		review := domain.Review{
			DeliveryID:         deliveryID,
			Owner:              owner,
			Repo:               repoName,
			PRNumber:           issueNumber,
			HeadSHA:            prInfo.HeadSHA,
			BaseSHA:            prInfo.BaseSHA,
			InstallationID:     instID,
			Author:             prInfo.Author,
			Title:              prInfo.Title,
			Body:               prInfo.Body,
			TriggerComment:     comment.GetBody(),
			TriggerAuthor:      comment.GetUser().GetLogin(),
			TriggerAssociation: comment.GetAuthorAssociation(),
			Origin:             domain.OriginMentionKeyword,
			PRLabels:           labels,
			EnqueuedAt:         time.Now(),
		}
		if mentionRouterFor(u.cfg, rc) == "agent" && mentionCommand(comment.GetBody()) == "" {
			review.Type = "respond"
			review.JobKey = dedup.RespondKey(owner, repoName, issueNumber, comment.GetID())
		}

		// The fetch above already produced per-file stats — the gate is exact.
		u.applySizeGate(ctx, &review, rc, prInfo, sizeStats{})

		return u.enqueueReview(ctx, review, u.commentWebhookEvent(deliveryID, cmEvent, body, "received", ""), rc)
	}

	// Non-PR issue: trigger issue analysis (or the respond agent).
	review := domain.Review{
		DeliveryID:         deliveryID,
		Owner:              owner,
		Repo:               repoName,
		PRNumber:           issueNumber,
		InstallationID:     instID,
		Author:             issue.GetUser().GetLogin(),
		Title:              issue.GetTitle(),
		Body:               issue.GetBody(),
		Type:               "issue",
		TriggerComment:     comment.GetBody(),
		TriggerAuthor:      comment.GetUser().GetLogin(),
		TriggerAssociation: comment.GetAuthorAssociation(),
		Origin:             domain.OriginMentionKeyword,
		PRLabels:           labels,
		EnqueuedAt:         time.Now(),
	}
	if mentionRouterFor(u.cfg, rc) == "agent" {
		review.Type = "respond"
		review.JobKey = dedup.RespondKey(owner, repoName, issueNumber, comment.GetID())
	}

	return u.enqueueReview(ctx, review, u.commentWebhookEvent(deliveryID, cmEvent, body, "received", ""), rc)
}

// isBotMentioned reports whether a comment body mentions the bot handle.
func (u *WebhookUsecase) isBotMentioned(body string) bool {
	body = strings.ToLower(body)
	handle := strings.ToLower(u.cfg.BotHandle)
	return strings.Contains(body, "@"+handle) || strings.Contains(body, handle)
}

// mentionCommand parses exact slash commands that bypass both the keyword
// router and the respond agent — `@elyoraa /review`, `/split`, `/accept`.
// Returns "" when the trigger is natural language.
func mentionCommand(body string) string {
	for _, field := range strings.Fields(body) {
		switch strings.ToLower(field) {
		case "/review", "/rereview":
			return "review"
		case "/split":
			return "split"
		case "/accept":
			return "accept"
		}
	}
	return ""
}

// mayCreateIssues reports whether the trigger's author_association permits
// issue-creating actions. Anything outside collaborator-level trust fails
// closed — on a public repo a stranger's "create the issues" must not conjure
// issues.
func mayCreateIssues(association string) bool {
	switch strings.ToUpper(association) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// mayDispatchReports whether the trigger may request work: repository
// collaborators, or the PR/issue author asking about their own change.
func mayDispatch(j *domain.Review) bool {
	if mayCreateIssues(j.TriggerAssociation) {
		return true
	}
	return j.TriggerAuthor != "" && j.Author != "" &&
		strings.EqualFold(j.TriggerAuthor, j.Author)
}

// splitProposalOutstanding reports whether the newest split job for this PR
// left an un-consumed proposal: it ran in propose mode and completed (or is
// still running). Once a create-mode split completes, the proposal is
// consumed — a second acceptance must not create issues again.
func splitProposalOutstanding(t queue.JobTracker, ctx context.Context, j *domain.Review) bool {
	if t == nil {
		return false
	}
	for _, rec := range t.List(ctx) { // most recently updated first
		if rec.Owner != j.Owner || rec.Repo != j.Repo || rec.PRNumber != j.PRNumber ||
			rec.Type != "split" {
			continue
		}
		if rec.Job.SplitCreate {
			return false // already consumed
		}
		return rec.Status == queue.JobStatusCompleted ||
			rec.Status == queue.JobStatusQueued ||
			rec.Status == queue.JobStatusProcessing
	}
	return false
}

// priorBotReply reports whether a conversational bot job (issue analysis or
// respond) previously completed on this issue — the "proposal" for an issue
// thread is a breakdown inside that reply, so a completed conversational job
// is the existence check for `create_issues`. This blocks the zero-history
// case where a mention conjures issues the bot never proposed.
func priorBotReply(t queue.JobTracker, ctx context.Context, j *domain.Review) bool {
	if t == nil {
		return false
	}
	for _, rec := range t.List(ctx) {
		if rec.Owner == j.Owner && rec.Repo == j.Repo && rec.PRNumber == j.PRNumber &&
			rec.Status == queue.JobStatusCompleted && rec.JobKey != j.JobKey &&
			(rec.Type == "issue" || rec.Type == "respond") {
			return true
		}
	}
	return false
}

// sizeStats carries the PR size counters GitHub embeds in webhook event
// payloads — no API call needed.
type sizeStats struct {
	additions    int
	deletions    int
	changedFiles int
}

// applySizeGate converts an oversize review job into a split-proposal job (or
// a gate triage job when SIZE_GATE_MODE=agent) at enqueue time, so the verdict
// isn't delayed by queue wait. Intent is checked before measurement: explicit
// human commands are honored even when a fresh estimate no longer says the PR
// is too big — acceptance keywords ("create the issues") must work on a PR
// that re-measures under budget. An empty payload without prInfo falls back
// to one GetPR call; a fetch failure leaves the job as-is. Payload counters
// count generated files (bun.lock, *.pb.*), so an oversize nomination from
// counters is re-measured with per-file data before the job is typed.
func (u *WebhookUsecase) applySizeGate(ctx context.Context, r *domain.Review, rc domain.RepoConfig, prInfo *github.PRInfo, st sizeStats) {
	if r.IsIssue() {
		return
	}

	// The rate is calibrated from recently completed reviews so the budget
	// tracks real execution time; it falls back to config until then. A
	// repo-level lines_per_minute overrides the config fallback.
	lpm := linesPerMinuteFor(u.cfg, rc)
	if t := u.q.Tracker(); t != nil {
		lpm = calibratedLinesPerMinute(t.List(ctx), lpm, r.Owner, r.Repo)
	}
	budget := budgetMinutesFor(u.cfg, rc)

	// ignored_paths from .elyoraa.yml joins the generated-file heuristics —
	// it needs the per-file list, so it applies only when stats are present.
	var gen func(string) bool
	if len(rc.IgnoredPaths) > 0 {
		gen = func(p string) bool { return ignoredPath(rc.IgnoredPaths, p) }
	}

	var size prSize
	var counterBased bool
	switch {
	case prInfo != nil:
		size = estimatePRSizeWith(prInfo, lpm, gen)
	case st.additions+st.deletions+st.changedFiles > 0:
		size = estimateFromCounts(st.additions, st.deletions, st.changedFiles, lpm, u.cfg.MaxChangedFiles)
		counterBased = true
	default:
		info := u.gatePRInfo(ctx, r)
		if info == nil {
			return
		}
		size = estimatePRSizeWith(info, lpm, gen)
	}

	// The estimate is recorded on every non-issue job — respond jobs consume
	// it as prompt context, completed reviews feed it to rate calibration.
	r.SizeEstMinutes = size.EstMinutes
	r.SizeLines = size.Additions + size.Deletions
	r.SizeOversize = size.tooBig(budget)
	r.BudgetMinutes = budget
	r.IgnoredPaths = rc.IgnoredPaths

	// Respond jobs are never gated — the agent decides from these stats.
	if r.IsRespond() {
		return
	}

	// Slash commands are exact user intent — they bypass the gate entirely.
	// `/accept` still requires an outstanding proposal and collaborator
	// permissions; without them it degrades to a plain mention (review).
	switch mentionCommand(r.TriggerComment) {
	case "review":
		r.Origin = domain.OriginDispatched
		return
	case "split":
		r.Type = "split"
		r.Origin = domain.OriginDispatched
		return
	case "accept":
		if mayCreateIssues(r.TriggerAssociation) && splitProposalOutstanding(u.q.Tracker(), ctx, r) {
			r.Type = "split"
			r.SplitCreate = true
			r.Origin = domain.OriginAccepted
		} else {
			r.Origin = domain.OriginDispatched
		}
		return
	}

	if !u.cfg.PRSizeGate {
		return
	}

	// Intent before measurement: explicit human commands are honored even
	// when the fresh estimate no longer says the PR is too big. Acceptance
	// additionally requires an outstanding proposal on record — the
	// acceptance keywords are everyday phrases ("go ahead", "breakdown")
	// that must not conjure issue creation on a PR that was never offered
	// one — and collaborator permissions, since these keywords create issues.
	switch classifyOversize(r.TriggerComment) {
	case oversizeReview:
		r.Origin = domain.OriginMentionKeyword
		return
	case oversizeCreate:
		if mayCreateIssues(r.TriggerAssociation) && splitProposalOutstanding(u.q.Tracker(), ctx, r) {
			r.Type = "split"
			r.SplitCreate = true
			r.Origin = domain.OriginAccepted
		}
		return
	}

	if !r.SizeOversize {
		return
	}

	// Payload counters can't see paths — generated churn (bun.lock, *.pb.*,
	// dist/) inflates line counts without adding review effort. Before
	// nominating split work, buy the per-file verdict with one GetPR; it
	// also lets ignored_paths apply on the counter path. A truncated
	// payload can't be rescued (the fetch hits the same cap), and a failed
	// fetch keeps the counter verdict — process-time re-verification still
	// demotes a false nomination.
	if counterBased && !size.Truncated {
		if info := u.gatePRInfo(ctx, r); info != nil {
			size = estimatePRSizeWith(info, lpm, gen)
			r.SizeEstMinutes = size.EstMinutes
			r.SizeLines = size.Additions + size.Deletions
			r.SizeOversize = size.tooBig(budget)
			if !r.SizeOversize {
				slog.Info("size gate: refined under budget, keeping review",
					"owner", r.Owner, "repo", r.Repo, "pr", r.PRNumber,
					"est_minutes", size.EstMinutes, "generated_files", size.GeneratedFiles)
				return
			}
		}
	}

	slog.Info("oversize PR at enqueue",
		"owner", r.Owner, "repo", r.Repo, "pr", r.PRNumber,
		"files", size.Files, "additions", size.Additions, "deletions", size.Deletions,
		"est_minutes", size.EstMinutes, "budget_minutes", budget,
		"truncated", size.Truncated)

	if sizeGateModeFor(u.cfg, rc) == "agent" {
		// Nomination bands: triage is only worth a run near the boundary.
		// Clearly-huge or truncated PRs go straight to the split proposal —
		// no amount of "actually it's simple" rescues 3× budget or an
		// unfetchably-large file list.
		if size.Truncated || (budget > 0 && size.EstMinutes > 3*budget) {
			r.Type = "split"
			return
		}
		r.Type = "gate"
		return
	}
	r.Type = "split"
}

// gatePRInfo fetches per-file PR data for the size gate. A missing fetcher
// or a failed fetch returns nil — callers keep the coarser verdict they
// already have.
func (u *WebhookUsecase) gatePRInfo(ctx context.Context, r *domain.Review) *github.PRInfo {
	if u.getPRStats == nil {
		return nil
	}
	gctx, cancel := context.WithTimeout(ctx, u.cfg.CommentTimeout)
	defer cancel()
	info, err := u.getPRStats(gctx, r.InstallationID, r.Owner, r.Repo, r.PRNumber, 0, u.cfg.MaxChangedFiles)
	if err != nil {
		slog.Warn("size gate: could not fetch PR stats",
			"owner", r.Owner, "repo", r.Repo, "pr", r.PRNumber, "error", err)
		return nil
	}
	return info
}

func (u *WebhookUsecase) enqueueReview(ctx context.Context, review domain.Review, evt *domain.WebhookEvent, rc domain.RepoConfig) (domain.Review, error) {
	// WORKSPACE_BASE is the repo registry: only pre-cloned repos are served.
	// Webhooks for anything else are acknowledged and dropped silently —
	// no pending comment, no queue slot. Registration is a manual
	// `git clone --bare` on the server.
	if u.cache != nil && !u.cache.IsCloned(review.Owner, review.Repo) {
		if evt != nil {
			_ = u.store.Record(ctx, evt)
		}
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "repo not registered")
		slog.Info("dropping job: repo not registered", "repo", review.Owner+"/"+review.Repo)
		return domain.Review{}, nil
	}

	// Skip bot-authored PRs for auto-reviews, but allow explicit mentions
	// (rereview/issue analysis) to proceed even if the PR was bot-authored.
	if review.IsBot() && review.TriggerComment == "" {
		if evt != nil {
			_ = u.store.Record(ctx, evt)
		}
		slog.Info("skipping bot-authored PR (auto-review)", "author", review.Author)
		return domain.Review{}, nil
	}

	// .elyoraa.yml `enabled: false` opts the repo out entirely — drop
	// silently (a visible "I'm disabled" reply per mention is spam).
	if !repoEnabled(rc) {
		if evt != nil {
			_ = u.store.Record(ctx, evt)
		}
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "repo disabled in "+repoConfigFile)
		slog.Info("dropping job: repo disabled", "repo", review.Owner+"/"+review.Repo)
		return domain.Review{}, nil
	}

	// The opt-out label suppresses automation, not humans: only jobs with no
	// trigger author (pure auto-review) are skipped; an explicit mention
	// overrides the label.
	if review.TriggerAuthor == "" {
		if lbl := skipLabelFor(u.cfg, rc); lbl != "" && slices.Contains(review.PRLabels, lbl) {
			if evt != nil {
				_ = u.store.Record(ctx, evt)
			}
			_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "skip label "+lbl)
			slog.Info("dropping job: skip label", "repo", review.Owner+"/"+review.Repo, "pr", review.PRNumber, "label", lbl)
			return domain.Review{}, nil
		}
	}

	// Persist the effective repo config on the job so Process doesn't have
	// to re-read it — the file could change between enqueue and execution.
	review.BudgetMinutes = budgetMinutesFor(u.cfg, rc)
	review.IgnoredPaths = rc.IgnoredPaths

	// Mention-triggered jobs are rate-limited per author per repo — every
	// mention costs a Crush run. Auto-reviews carry no TriggerAuthor.
	if review.TriggerAuthor != "" && !u.limiter.allow(review.TriggerAuthor+"@"+review.Owner+"/"+review.Repo, time.Now()) {
		if evt != nil {
			_ = u.store.Record(ctx, evt)
		}
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "mention rate limit")
		slog.Info("dropping mention: rate limited", "author", review.TriggerAuthor, "repo", review.Owner+"/"+review.Repo)
		return domain.Review{}, ErrRateLimited
	}

	// A daily per-repo job cap bounds spend even for authorized triggers
	// (webhook storms, mention loops). Dispatched jobs bypass this path and
	// are bounded by RESPOND_MAX_ACTIONS instead.
	if u.cfg.DailyJobCapPerRepo > 0 && u.jobsToday(ctx, review.Owner, review.Repo) >= u.cfg.DailyJobCapPerRepo {
		if evt != nil {
			_ = u.store.Record(ctx, evt)
		}
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "daily job cap")
		slog.Info("dropping job: daily repo cap", "repo", review.Owner+"/"+review.Repo)
		return domain.Review{}, ErrCapped
	}

	// Respond jobs arrive with their own trigger-keyed JobKey; everything
	// else keys on owner/repo/pr/sha.
	if review.JobKey == "" {
		review.JobKey = dedup.JobKey(review.Owner, review.Repo, review.PRNumber, review.HeadSHA)
	}

	// A respond job whose trigger already ran to completion must not re-run:
	// `edited` events re-deliver the same trigger ID forever, and a typo fix
	// on "@elyoraa create the issues" must not create the issues twice.
	if review.IsRespond() {
		if t := u.q.Tracker(); t != nil {
			if rec, ok := t.Get(ctx, review.JobKey); ok && rec.Status == queue.JobStatusCompleted {
				if evt != nil {
					_ = u.store.Record(ctx, evt)
				}
				_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "trigger already handled")
				slog.Info("dropping redelivered/edited mention with completed job", "job", review.JobKey)
				return domain.Review{}, nil
			}
		}
	}

	if evt != nil {
		_ = u.store.Record(ctx, evt)
	}

	jobCtx, job, ok := u.d.TryStart(ctx, review)
	if !ok {
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "duplicate review in flight")
		slog.Info("skipping duplicate review", "job", review.JobKey)
		return domain.Review{}, ErrDuplicate
	}
	ctx = jobCtx

	// Post the queued comment if we don't already have one. It says
	// "queued", not "in progress" — the job may wait behind running work
	// for a while, and that wait should be visible. Process flips it to
	// the running state on pickup.
	if job.PendingCommentID == 0 {
		pendingCtx, cancel := context.WithTimeout(context.Background(), u.cfg.CommentTimeout)
		id, err := u.gh.PostPendingComment(pendingCtx, job.InstallationID, job.Owner, job.Repo, job.PRNumber,
			queuedBody(job, jobBudget(u.cfg, *job)))
		cancel()
		if err != nil {
			slog.Warn("failed to post pending comment", "error", err)
		} else {
			job.PendingCommentID = id
		}
	}

	if !u.q.Submit(ctx, *job) {
		slog.Warn("queue full, dropping job", "job", job.JobKey)
		if job.PendingCommentID != 0 {
			editCtx, cancel := context.WithTimeout(context.Background(), u.cfg.CommentTimeout)
			_ = u.gh.EditComment(editCtx, job.InstallationID, job.Owner, job.Repo, job.PendingCommentID, queueFullBody)
			cancel()
		}
		u.d.Done(*job)
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "queue full")
		return *job, ErrQueueFull
	}

	_ = u.store.UpdateStatus(ctx, review.DeliveryID, "queued", "")
	return *job, nil
}

// jobsToday counts tracker records for the repo started today (UTC). The
// tracker is bounded (~1000 records memory / persistent for BoltDB), so a
// full scan per enqueue is cheap relative to a Crush run.
func (u *WebhookUsecase) jobsToday(ctx context.Context, owner, repo string) int {
	t := u.q.Tracker()
	if t == nil {
		return 0
	}
	dayStart := time.Now().UTC().Truncate(24 * time.Hour)
	n := 0
	for _, rec := range t.List(ctx) {
		if rec.Owner == owner && rec.Repo == repo && rec.StartedAt != nil && !rec.StartedAt.Before(dayStart) {
			n++
		}
	}
	return n
}

func (u *WebhookUsecase) deliveryID(ctx context.Context) string {
	if id, ok := ctx.Value(deliveryIDKey).(string); ok && id != "" {
		return id
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("elyoraa-%d", time.Now().UnixNano())
	}
	return "elyoraa-" + hex.EncodeToString(b)
}

func (u *WebhookUsecase) prWebhookEvent(deliveryID string, prEvent *go_github.PullRequestEvent, body []byte, status, errMsg string) *domain.WebhookEvent {
	evt := &domain.WebhookEvent{
		DeliveryID: deliveryID,
		EventType:  "pull_request",
		Action:     prEvent.GetAction(),
		Payload:    body,
		Status:     status,
		ReceivedAt: time.Now(),
		Error:      errMsg,
	}
	if repo := prEvent.GetRepo(); repo != nil {
		evt.Owner = repo.GetOwner().GetLogin()
		evt.Repo = repo.GetName()
	}
	if pr := prEvent.GetPullRequest(); pr != nil {
		evt.PRNumber = pr.GetNumber()
	}
	return evt
}

func (u *WebhookUsecase) commentWebhookEvent(deliveryID string, cmEvent *go_github.IssueCommentEvent, body []byte, status, errMsg string) *domain.WebhookEvent {
	evt := &domain.WebhookEvent{
		DeliveryID: deliveryID,
		EventType:  "issue_comment",
		Action:     cmEvent.GetAction(),
		Payload:    body,
		Status:     status,
		ReceivedAt: time.Now(),
		Error:      errMsg,
	}
	if repo := cmEvent.GetRepo(); repo != nil {
		evt.Owner = repo.GetOwner().GetLogin()
		evt.Repo = repo.GetName()
	}
	if issue := cmEvent.GetIssue(); issue != nil {
		evt.PRNumber = issue.GetNumber()
	}
	return evt
}

func (u *WebhookUsecase) handlePullRequestReview(ctx context.Context, deliveryID string, body []byte) (domain.Review, error) {
	event, err := go_github.ParseWebHook("pull_request_review", body)
	if err != nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review",
			Status: "failed", Error: fmt.Sprintf("parse: %v", err), ReceivedAt: time.Now(),
		})
		return domain.Review{}, fmt.Errorf("parse: %w", err)
	}

	rvEvent, ok := event.(*go_github.PullRequestReviewEvent)
	if !ok {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review",
			Status: "ignored", ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	action := rvEvent.GetAction()
	if action != "submitted" && action != "edited" {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	review := rvEvent.GetReview()
	if review == nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}
	reviewAuthor := strings.ToLower(review.GetUser().GetLogin())
	if strings.HasSuffix(reviewAuthor, "[bot]") || strings.Contains(reviewAuthor, "github-actions") {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}
	if !u.isBotMentioned(review.GetBody()) {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	pr := rvEvent.GetPullRequest()
	repo := rvEvent.GetRepo()
	if pr == nil || repo == nil || repo.GetFullName() == "" {
		return domain.Review{}, fmt.Errorf("missing pr or repo")
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	owner, repoName := ownerRepo[0], ownerRepo[1]
	instID := rvEvent.GetInstallation().GetID()
	rc := loadRepoConfig(ctx, u.cache, owner, repoName)

	if pr.GetDraft() {
		return domain.Review{}, nil
	}

	r := domain.Review{
		DeliveryID:         deliveryID,
		Owner:              owner,
		Repo:               repoName,
		PRNumber:           pr.GetNumber(),
		HeadSHA:            pr.GetHead().GetSHA(),
		BaseSHA:            pr.GetBase().GetSHA(),
		InstallationID:     instID,
		Author:             pr.GetUser().GetLogin(),
		Title:              pr.GetTitle(),
		Body:               pr.GetBody(),
		TriggerComment:     review.GetBody(),
		TriggerAuthor:      review.GetUser().GetLogin(),
		TriggerAssociation: review.GetAuthorAssociation(),
		Origin:             domain.OriginMentionKeyword,
		PRLabels:           labelNames(pr.Labels),
		EnqueuedAt:         time.Now(),
	}
	if mentionRouterFor(u.cfg, rc) == "agent" && mentionCommand(review.GetBody()) == "" {
		r.Type = "respond"
		r.JobKey = dedup.RespondKey(owner, repoName, pr.GetNumber(), review.GetID())
	}

	u.applySizeGate(ctx, &r, rc, nil, sizeStats{
		additions:    pr.GetAdditions(),
		deletions:    pr.GetDeletions(),
		changedFiles: pr.GetChangedFiles(),
	})

	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "pull_request_review",
		Action: action, Owner: owner, Repo: repoName, PRNumber: pr.GetNumber(),
		Status: "received", Payload: body, ReceivedAt: time.Now(),
	})
	return u.enqueueReview(ctx, r, nil, rc)
}

func (u *WebhookUsecase) handlePullRequestReviewComment(ctx context.Context, deliveryID string, body []byte) (domain.Review, error) {
	event, err := go_github.ParseWebHook("pull_request_review_comment", body)
	if err != nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review_comment",
			Status: "failed", Error: fmt.Sprintf("parse: %v", err), ReceivedAt: time.Now(),
		})
		return domain.Review{}, fmt.Errorf("parse: %w", err)
	}

	rcEvent, ok := event.(*go_github.PullRequestReviewCommentEvent)
	if !ok {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review_comment",
			Status: "ignored", ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	action := rcEvent.GetAction()
	if action != "created" && action != "edited" {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review_comment",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	comment := rcEvent.GetComment()
	if comment == nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review_comment",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}
	commentAuthor := strings.ToLower(comment.GetUser().GetLogin())
	if strings.HasSuffix(commentAuthor, "[bot]") || strings.Contains(commentAuthor, "github-actions") {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review_comment",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}
	if !u.isBotMentioned(comment.GetBody()) {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "pull_request_review_comment",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	pr := rcEvent.GetPullRequest()
	repo := rcEvent.GetRepo()
	if pr == nil || repo == nil || repo.GetFullName() == "" {
		return domain.Review{}, fmt.Errorf("missing pr or repo")
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	owner, repoName := ownerRepo[0], ownerRepo[1]
	instID := rcEvent.GetInstallation().GetID()
	rc := loadRepoConfig(ctx, u.cache, owner, repoName)

	if pr.GetDraft() {
		return domain.Review{}, nil
	}

	r := domain.Review{
		DeliveryID:         deliveryID,
		Owner:              owner,
		Repo:               repoName,
		PRNumber:           pr.GetNumber(),
		HeadSHA:            pr.GetHead().GetSHA(),
		BaseSHA:            pr.GetBase().GetSHA(),
		InstallationID:     instID,
		Author:             pr.GetUser().GetLogin(),
		Title:              pr.GetTitle(),
		Body:               pr.GetBody(),
		TriggerComment:     comment.GetBody(),
		TriggerAuthor:      comment.GetUser().GetLogin(),
		TriggerAssociation: comment.GetAuthorAssociation(),
		Origin:             domain.OriginMentionKeyword,
		PRLabels:           labelNames(pr.Labels),
		EnqueuedAt:         time.Now(),
	}
	if mentionRouterFor(u.cfg, rc) == "agent" && mentionCommand(comment.GetBody()) == "" {
		r.Type = "respond"
		r.JobKey = dedup.RespondKey(owner, repoName, pr.GetNumber(), comment.GetID())
	}

	u.applySizeGate(ctx, &r, rc, nil, sizeStats{
		additions:    pr.GetAdditions(),
		deletions:    pr.GetDeletions(),
		changedFiles: pr.GetChangedFiles(),
	})

	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "pull_request_review_comment",
		Action: action, Owner: owner, Repo: repoName, PRNumber: pr.GetNumber(),
		Status: "received", Payload: body, ReceivedAt: time.Now(),
	})
	return u.enqueueReview(ctx, r, nil, rc)
}

func (u *WebhookUsecase) handleIssues(ctx context.Context, deliveryID string, body []byte) (domain.Review, error) {
	event, err := go_github.ParseWebHook("issues", body)
	if err != nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "issues",
			Status: "failed", Error: fmt.Sprintf("parse: %v", err), ReceivedAt: time.Now(),
		})
		return domain.Review{}, fmt.Errorf("parse: %w", err)
	}

	isEvent, ok := event.(*go_github.IssuesEvent)
	if !ok {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "issues",
			Status: "ignored", ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	action := isEvent.GetAction()
	if action != "opened" && action != "edited" && action != "reopened" {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "issues",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	issue := isEvent.GetIssue()
	if issue == nil || !u.isBotMentioned(issue.GetBody()) {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID, EventType: "issues",
			Action: action, Status: "ignored", Payload: body, ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	repo := isEvent.GetRepo()
	if repo == nil || repo.GetFullName() == "" {
		return domain.Review{}, fmt.Errorf("missing repository")
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	owner, repoName := ownerRepo[0], ownerRepo[1]
	instID := isEvent.GetInstallation().GetID()
	rc := loadRepoConfig(ctx, u.cache, owner, repoName)

	r := domain.Review{
		DeliveryID:         deliveryID,
		Owner:              owner,
		Repo:               repoName,
		PRNumber:           issue.GetNumber(),
		InstallationID:     instID,
		Author:             issue.GetUser().GetLogin(),
		Title:              issue.GetTitle(),
		Body:               issue.GetBody(),
		Type:               "issue",
		TriggerAuthor:      issue.GetUser().GetLogin(),
		TriggerAssociation: issue.GetAuthorAssociation(),
		Origin:             domain.OriginMentionKeyword,
		PRLabels:           labelNames(issue.Labels),
		EnqueuedAt:         time.Now(),
	}
	if mentionRouterFor(u.cfg, rc) == "agent" {
		r.Type = "respond"
		r.JobKey = dedup.RespondKey(owner, repoName, issue.GetNumber(), issue.GetID())
		// TriggerComment stays empty so enqueueReview's bot-authored-issue
		// guard (IsBot && TriggerComment == "") keeps applying.
	}

	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "issues",
		Action: action, Owner: owner, Repo: repoName, PRNumber: issue.GetNumber(),
		Status: "received", Payload: body, ReceivedAt: time.Now(),
	})
	return u.enqueueReview(ctx, r, nil, rc)
}

// handleReaction records 👍/👎 (and other) reactions left on the bot's own
// comments — the quality-feedback signal. Only counts reactions whose subject
// comment was authored by this app (login "{bot}[bot]" or an elyoraa marker
// in the body), so reactions on arbitrary comments don't pollute the metric.
// Events are recorded in the audit log too, which is what replay fixtures
// read from.
func (u *WebhookUsecase) handleReaction(ctx context.Context, deliveryID string, body []byte) (domain.Review, error) {
	// The reaction event's subject key varies by what was reacted to
	// (comment, pull_request_review_comment, pull_request_review, …) — parse
	// the shared shape instead of the typed event.
	var ev struct {
		Action   string `json:"action"`
		Reaction struct {
			Content string `json:"content"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"reaction"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return domain.Review{}, fmt.Errorf("parse reaction: %w", err)
	}
	// Subject comment payloads: any of these keys may carry the reacted-on
	// object — collect whichever author/body pairs are present.
	var subjects []struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	for _, key := range []string{"comment", "pull_request_review_comment", "pull_request_review", "issue", "discussion"} {
		var s struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err == nil {
			if sub, ok := raw[key]; ok {
				if json.Unmarshal(sub, &s) == nil && s.User.Login != "" {
					subjects = append(subjects, s)
				}
			}
		}
	}

	botLogin := strings.ToLower(u.cfg.BotHandle) + "[bot]"
	botSubject := false
	for _, s := range subjects {
		if strings.EqualFold(s.User.Login, botLogin) || strings.Contains(s.Body, "<!-- elyoraa") {
			botSubject = true
			break
		}
	}

	status := "ignored"
	err := ""
	if !botSubject {
		err = "reaction not on a bot comment"
	} else {
		status = "received"
		if u.metrics != nil {
			u.metrics.IncReaction(ev.Reaction.Content, ev.Action)
		}
		slog.Info("reaction on bot comment",
			"reaction", ev.Reaction.Content, "action", ev.Action,
			"user", ev.Reaction.User.Login, "repo", ev.Repository.FullName)
	}
	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "reaction", Action: ev.Action,
		Status: status, Error: err, Payload: body, ReceivedAt: time.Now(),
	})
	return domain.Review{}, nil
}

// ErrJobNotFound is returned when a job key has no tracked record.
var ErrJobNotFound = errors.New("job not found")

// ListJobs returns all tracked job outcomes, most recently updated first.
func (u *WebhookUsecase) ListJobs(ctx context.Context) []queue.JobRecord {
	t := u.q.Tracker()
	if t == nil {
		return nil
	}
	return t.List(ctx)
}

// GetJob returns a single tracked job record by key.
func (u *WebhookUsecase) GetJob(ctx context.Context, key string) (queue.JobRecord, bool) {
	t := u.q.Tracker()
	if t == nil {
		return queue.JobRecord{}, false
	}
	return t.Get(ctx, key)
}

// RetryJob re-enqueues a previously tracked job (e.g. a failed review) so it
// can be retriggered without waiting for a new GitHub webhook delivery.
func (u *WebhookUsecase) RetryJob(ctx context.Context, key string) (domain.Review, error) {
	rec, ok := u.GetJob(ctx, key)
	if !ok || rec.Job.JobKey == "" {
		return domain.Review{}, ErrJobNotFound
	}

	job := rec.Job
	job.EnqueuedAt = time.Now()

	newCtx, newJob, ok := u.d.TryStart(ctx, job)
	if !ok {
		return domain.Review{}, ErrDuplicate
	}

	// Make the retry visible: a failed job's pending comment is still
	// around (flip it back to queued); a completed job's was deleted —
	// post a fresh one. Best-effort, the run proceeds either way.
	if newJob.PendingCommentID != 0 {
		editCtx, cancel := context.WithTimeout(context.Background(), u.cfg.CommentTimeout)
		err := u.gh.EditComment(editCtx, newJob.InstallationID, newJob.Owner, newJob.Repo, newJob.PendingCommentID,
			queuedBody(newJob, jobBudget(u.cfg, *newJob)))
		cancel()
		if err != nil {
			newJob.PendingCommentID = 0
		}
	}
	if newJob.PendingCommentID == 0 {
		pendingCtx, cancel := context.WithTimeout(context.Background(), u.cfg.CommentTimeout)
		id, err := u.gh.PostPendingComment(pendingCtx, newJob.InstallationID, newJob.Owner, newJob.Repo, newJob.PRNumber,
			queuedBody(newJob, jobBudget(u.cfg, *newJob)))
		cancel()
		if err == nil {
			newJob.PendingCommentID = id
		}
	}

	if !u.q.Submit(newCtx, *newJob) {
		u.d.Done(*newJob)
		if newJob.PendingCommentID != 0 {
			editCtx, cancel := context.WithTimeout(context.Background(), u.cfg.CommentTimeout)
			_ = u.gh.EditComment(editCtx, newJob.InstallationID, newJob.Owner, newJob.Repo, newJob.PendingCommentID, queueFullBody)
			cancel()
		}
		return domain.Review{}, ErrQueueFull
	}
	slog.Info("job retried",
		"job", job.JobKey,
		"delivery", job.DeliveryID,
		"owner", job.Owner,
		"repo", job.Repo,
		"pr", job.PRNumber,
	)
	return *newJob, nil
}
