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

// Register adds the webhook, health, and metrics routes to the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/webhook", h.ServeHTTP)
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc(h.cfg.MetricsPath, h.metrics.Handler)
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

	ctx := usecase.WithDeliveryID(r.Context(), deliveryID)
	ctx = usecase.WithEventType(ctx, eventType)

	_, err = h.uc.Handle(ctx, payload, sig)
	if err != nil {
		if !errors.Is(err, usecase.ErrQueueFull) && !errors.Is(err, usecase.ErrDuplicate) {
			slog.Warn("webhook handle", "error", err)
			http.Error(w, "webhook", http.StatusBadRequest)
			return
		}
		slog.Info("webhook dropped", "error", err)
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
