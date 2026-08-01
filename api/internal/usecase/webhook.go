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
		u.store.UpdateStatus(ctx, deliveryID, "failed", fmt.Sprintf("signature: %v", err))
		return domain.Review{}, fmt.Errorf("signature: %w", err)
	}

	eventType, _ := ctx.Value(eventTypeKey).(string)

	if eventType != "pull_request" {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  eventType,
			Status:     "ignored",
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	event, err := go_github.ParseWebHook(eventType, body)
	if err != nil {
		_ = u.store.Record(ctx, &domain.WebhookEvent{
			DeliveryID: deliveryID,
			EventType:  eventType,
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
			EventType:  eventType,
			Status:     "ignored",
			ReceivedAt: time.Now(),
		})
		return domain.Review{}, nil
	}

	action := prEvent.GetAction()
	if action != "opened" && action != "synchronize" && action != "reopened" {
		_ = u.store.Record(ctx, u.webhookEvent(deliveryID, prEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	pr := prEvent.GetPullRequest()
	if pr.GetDraft() {
		_ = u.store.Record(ctx, u.webhookEvent(deliveryID, prEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	repo := prEvent.GetRepo()
	if repo == nil || repo.GetFullName() == "" {
		_ = u.store.Record(ctx, u.webhookEvent(deliveryID, prEvent, body, "failed", "missing repository"))
		return domain.Review{}, fmt.Errorf("missing repository")
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	if len(ownerRepo) != 2 {
		_ = u.store.Record(ctx, u.webhookEvent(deliveryID, prEvent, body, "failed", "invalid repository"))
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

	if review.IsBot() {
		_ = u.store.Record(ctx, u.webhookEvent(deliveryID, prEvent, body, "ignored", ""))
		return domain.Review{}, nil
	}

	review.JobKey = dedup.JobKey(owner, repoName, review.PRNumber, review.HeadSHA)

	_ = u.store.Record(ctx, u.webhookEvent(deliveryID, prEvent, body, "received", ""))

	ctx, job, ok := u.d.TryStart(ctx, review)
	if !ok {
		_ = u.store.UpdateStatus(ctx, deliveryID, "dropped", "duplicate review in flight")
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
		_ = u.store.UpdateStatus(ctx, deliveryID, "dropped", "queue full")
		return *job, ErrQueueFull
	}

	_ = u.store.UpdateStatus(ctx, deliveryID, "queued", "")
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

func (u *WebhookUsecase) webhookEvent(deliveryID string, prEvent *go_github.PullRequestEvent, body []byte, status, errMsg string) *domain.WebhookEvent {
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
