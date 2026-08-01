package repository

import (
	"context"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebhookStore persists webhook event lifecycle records.
type WebhookStore interface {
	Record(ctx context.Context, evt *domain.WebhookEvent) error
	UpdateStatus(ctx context.Context, deliveryID, status, errMsg string) error
}

// PostgresWebhookStore is a PostgreSQL-backed WebhookStore.
type PostgresWebhookStore struct {
	pool *pgxpool.Pool
}

type noOpWebhookStore struct{}

func (noOpWebhookStore) Record(_ context.Context, _ *domain.WebhookEvent) error { return nil }
func (noOpWebhookStore) UpdateStatus(_ context.Context, _, _, _ string) error   { return nil }

// NewWebhookStore returns a PostgreSQL store when a pool is available.
// When pool is nil, it returns a no-op store so the service can run without a database.
func NewWebhookStore(pool *pgxpool.Pool) WebhookStore {
	if pool == nil {
		return &noOpWebhookStore{}
	}
	return &PostgresWebhookStore{pool: pool}
}

// Record inserts or replaces a webhook event record.
func (s *PostgresWebhookStore) Record(ctx context.Context, evt *domain.WebhookEvent) error {
	if s.pool == nil {
		return nil
	}
	if evt.ReceivedAt.IsZero() {
		evt.ReceivedAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_events (
			delivery_id, event_type, action, owner, repo, pr_number, payload, status, received_at, error
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (delivery_id) DO UPDATE SET
			event_type = EXCLUDED.event_type,
			action = EXCLUDED.action,
			owner = EXCLUDED.owner,
			repo = EXCLUDED.repo,
			pr_number = EXCLUDED.pr_number,
			payload = EXCLUDED.payload,
			status = EXCLUDED.status,
			received_at = EXCLUDED.received_at,
			error = EXCLUDED.error,
			processed_at = NULL
	`, evt.DeliveryID, evt.EventType, evt.Action, evt.Owner, evt.Repo, evt.PRNumber,
		evt.Payload, evt.Status, evt.ReceivedAt, evt.Error)
	return err
}

// UpdateStatus updates the status and error of a webhook event.
// It also sets processed_at when the status is terminal.
func (s *PostgresWebhookStore) UpdateStatus(ctx context.Context, deliveryID, status, errMsg string) error {
	if s.pool == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE webhook_events
		SET status = $1,
			error = $2,
			processed_at = CASE
				WHEN $1 IN ('completed', 'failed', 'cancelled', 'dropped') THEN NOW()
				ELSE processed_at
			END
		WHERE delivery_id = $3
	`, status, errMsg, deliveryID)
	return err
}
