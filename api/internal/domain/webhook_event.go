package domain

import (
	"encoding/json"
	"time"
)

// WebhookEvent records a single GitHub webhook delivery.
type WebhookEvent struct {
	DeliveryID  string          `json:"delivery_id"`
	EventType   string          `json:"event_type"`
	Action      string          `json:"action"`
	Owner       string          `json:"owner"`
	Repo        string          `json:"repo"`
	PRNumber    int             `json:"pr_number"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	ReceivedAt  time.Time       `json:"received_at"`
	ProcessedAt *time.Time      `json:"processed_at,omitempty"`
	Error       string          `json:"error"`
}
