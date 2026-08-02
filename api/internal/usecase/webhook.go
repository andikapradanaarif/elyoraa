package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
	go_github "github.com/google/go-github/v88/github"
)

// ErrQueueFull is returned when the queue is at capacity.
var ErrQueueFull = errors.New("queue full")

// ErrDuplicate is returned when the same review is already in flight.
var ErrDuplicate = errors.New("duplicate review in flight")

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

// WebhookUsecase validates and routes incoming GitHub webhooks.
type WebhookUsecase struct {
	cfg   *config.Config
	gh    *github.Client
	d     *dedup.Dedup
	q     *queue.Queue
	store repository.WebhookStore
}

// NewWebhookUsecase creates a WebhookUsecase.
func NewWebhookUsecase(cfg *config.Config, client *github.Client, d *dedup.Dedup, q *queue.Queue, store repository.WebhookStore) *WebhookUsecase {
	return &WebhookUsecase{cfg: cfg, gh: client, d: d, q: q, store: store}
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
		EnqueuedAt:     time.Now(),
	}

	return u.enqueueReview(ctx, review, u.prWebhookEvent(deliveryID, prEvent, body, "received", ""))
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

	// If the comment is on a PR, trigger a rereview.
	if issue.IsPullRequest() {
		prCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		prInfo, err := u.gh.GetPR(prCtx, instID, owner, repoName, issueNumber, 0, 0)
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
			DeliveryID:     deliveryID,
			Owner:          owner,
			Repo:           repoName,
			PRNumber:       issueNumber,
			HeadSHA:        prInfo.HeadSHA,
			BaseSHA:        prInfo.BaseSHA,
			InstallationID: instID,
			Author:         prInfo.Author,
			Title:          prInfo.Title,
			Body:           prInfo.Body,
			TriggerComment: comment.GetBody(),
			EnqueuedAt:     time.Now(),
		}

		return u.enqueueReview(ctx, review, u.commentWebhookEvent(deliveryID, cmEvent, body, "received", ""))
	}

	// Non-PR issue: trigger issue analysis.
	review := domain.Review{
		DeliveryID:     deliveryID,
		Owner:          owner,
		Repo:           repoName,
		PRNumber:       issueNumber,
		InstallationID: instID,
		Author:         issue.GetUser().GetLogin(),
		Title:          issue.GetTitle(),
		Body:           issue.GetBody(),
		Type:           "issue",
		TriggerComment: comment.GetBody(),
		EnqueuedAt:     time.Now(),
	}

	return u.enqueueReview(ctx, review, u.commentWebhookEvent(deliveryID, cmEvent, body, "received", ""))
}

// isBotMentioned reports whether a comment body mentions the bot handle.
func (u *WebhookUsecase) isBotMentioned(body string) bool {
	body = strings.ToLower(body)
	handle := strings.ToLower(u.cfg.BotHandle)
	return strings.Contains(body, "@"+handle) || strings.Contains(body, handle)
}

// isRereviewRequest reports whether a comment is asking the bot to rereview a PR.
func (u *WebhookUsecase) isRereviewRequest(body string) bool {
	if !u.isBotMentioned(body) {
		return false
	}
	return true
}

func (u *WebhookUsecase) enqueueReview(ctx context.Context, review domain.Review, evt *domain.WebhookEvent) (domain.Review, error) {
	// Skip bot-authored PRs for auto-reviews, but allow explicit mentions
	// (rereview/issue analysis) to proceed even if the PR was bot-authored.
	if review.IsBot() && review.TriggerComment == "" {
		if evt != nil {
			_ = u.store.Record(ctx, evt)
		}
		slog.Info("skipping bot-authored PR (auto-review)", "author", review.Author)
		return domain.Review{}, nil
	}

	review.JobKey = dedup.JobKey(review.Owner, review.Repo, review.PRNumber, review.HeadSHA)

	if evt != nil {
		_ = u.store.Record(ctx, evt)
	}

	ctx, job, ok := u.d.TryStart(ctx, review)
	if !ok {
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "duplicate review in flight")
		slog.Info("skipping duplicate review", "job", review.JobKey)
		return domain.Review{}, ErrDuplicate
	}

	// Post the in-progress comment if we don't already have one.
	if job.PendingCommentID == 0 {
		pendingBody := "Review in progress…\n\n<!-- elyoraa-in-progress -->"
		pendingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		id, err := u.gh.PostPendingComment(pendingCtx, job.InstallationID, job.Owner, job.Repo, job.PRNumber, pendingBody)
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
			editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = u.gh.EditComment(editCtx, job.InstallationID, job.Owner, job.Repo, job.PendingCommentID,
				"Review queued, will run later.\n\n<!-- elyoraa-in-progress -->")
			cancel()
		}
		u.d.Done(job.JobKey)
		_ = u.store.UpdateStatus(ctx, review.DeliveryID, "dropped", "queue full")
		return *job, ErrQueueFull
	}

	_ = u.store.UpdateStatus(ctx, review.DeliveryID, "queued", "")
	return *job, nil
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
	if review == nil || !u.isBotMentioned(review.GetBody()) {
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

	if pr.GetDraft() {
		return domain.Review{}, nil
	}

	r := domain.Review{
		DeliveryID:     deliveryID,
		Owner:          owner,
		Repo:           repoName,
		PRNumber:       pr.GetNumber(),
		HeadSHA:        pr.GetHead().GetSHA(),
		BaseSHA:        pr.GetBase().GetSHA(),
		InstallationID: instID,
		Author:         pr.GetUser().GetLogin(),
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		TriggerComment: review.GetBody(),
		EnqueuedAt:     time.Now(),
	}

	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "pull_request_review",
		Action: action, Owner: owner, Repo: repoName, PRNumber: pr.GetNumber(),
		Status: "received", Payload: body, ReceivedAt: time.Now(),
	})
	return u.enqueueReview(ctx, r, nil)
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
	if comment == nil || !u.isBotMentioned(comment.GetBody()) {
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

	if pr.GetDraft() {
		return domain.Review{}, nil
	}

	r := domain.Review{
		DeliveryID:     deliveryID,
		Owner:          owner,
		Repo:           repoName,
		PRNumber:       pr.GetNumber(),
		HeadSHA:        pr.GetHead().GetSHA(),
		BaseSHA:        pr.GetBase().GetSHA(),
		InstallationID: instID,
		Author:         pr.GetUser().GetLogin(),
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		TriggerComment: comment.GetBody(),
		EnqueuedAt:     time.Now(),
	}

	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "pull_request_review_comment",
		Action: action, Owner: owner, Repo: repoName, PRNumber: pr.GetNumber(),
		Status: "received", Payload: body, ReceivedAt: time.Now(),
	})
	return u.enqueueReview(ctx, r, nil)
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

	r := domain.Review{
		DeliveryID:     deliveryID,
		Owner:          owner,
		Repo:           repoName,
		PRNumber:       issue.GetNumber(),
		InstallationID: instID,
		Author:         issue.GetUser().GetLogin(),
		Title:          issue.GetTitle(),
		Body:           issue.GetBody(),
		Type:           "issue",
		EnqueuedAt:     time.Now(),
	}

	_ = u.store.Record(ctx, &domain.WebhookEvent{
		DeliveryID: deliveryID, EventType: "issues",
		Action: action, Owner: owner, Repo: repoName, PRNumber: issue.GetNumber(),
		Status: "received", Payload: body, ReceivedAt: time.Now(),
	})
	return u.enqueueReview(ctx, r, nil)
}
