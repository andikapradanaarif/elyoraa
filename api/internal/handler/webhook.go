package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
	"github.com/andikapradanaarif/elyoraa/internal/startup"
	"github.com/andikapradanaarif/elyoraa/internal/usecase"
)

// Handler exposes HTTP handlers for the service.
type Handler struct {
	cfg     *config.Config
	uc      *usecase.WebhookUsecase
	metrics *metrics.Registry
}

// New creates a new HTTP handler.
func New(cfg *config.Config, uc *usecase.WebhookUsecase, m *metrics.Registry) *Handler {
	return &Handler{cfg: cfg, uc: uc, metrics: m}
}

// Register adds the webhook, health, metrics, and job routes to the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/webhook/gh", h.ServeHTTP)
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc(h.cfg.MetricsPath, h.metrics.Handler)
	mux.HandleFunc("GET /jobs", h.ListJobs)
	mux.HandleFunc("GET /jobs/{key...}", h.GetJob)
	mux.HandleFunc("POST /jobs/retry/{key...}", h.RetryJob)
}

// ServeHTTP handles incoming GitHub webhooks.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	sig := r.Header.Get("X-Hub-Signature-256")
	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")

	slog.Info("webhook received", "event", eventType, "delivery", deliveryID)

	// GitHub sends a ping event when the webhook is first configured.
	// Respond with 200 so GitHub marks the webhook as active.
	if eventType == "ping" {
		slog.Info("webhook ping received", "delivery", deliveryID)
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := usecase.WithDeliveryID(r.Context(), deliveryID)
	ctx = usecase.WithEventType(ctx, eventType)

	review, err := h.uc.Handle(ctx, payload, sig)
	if err != nil {
		if !errors.Is(err, usecase.ErrQueueFull) && !errors.Is(err, usecase.ErrDuplicate) {
			slog.Warn("webhook handle", "event", eventType, "delivery", deliveryID, "error", err)
			http.Error(w, "webhook", http.StatusBadRequest)
			return
		}
		slog.Info("webhook dropped", "event", eventType, "delivery", deliveryID, "error", err)
	}

	if review.JobKey != "" {
		slog.Info("review enqueued", "event", eventType, "delivery", deliveryID, "job", review.JobKey, "type", review.Type, "owner", review.Owner, "repo", review.Repo, "pr", review.PRNumber)
	} else {
		slog.Info("webhook ignored", "event", eventType, "delivery", deliveryID)
	}

	w.WriteHeader(http.StatusOK)
}

// Health runs startup checks and reports readiness.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	results := startup.Check(h.cfg)
	w.Header().Set("Content-Type", "application/json")
	if startup.HasFailures(results) {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if err := json.NewEncoder(w).Encode(results); err != nil {
		slog.Error("health encode", "error", err)
	}
}
