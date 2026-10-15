package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/usecase"
)

// ListJobs returns all tracked job outcomes, most recently updated first.
// Optional query param: ?status=failed|completed|processing|queued
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := h.uc.ListJobs(r.Context())
	status := r.URL.Query().Get("status")
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		if status == "" || j.Status == status {
			out = append(out, jobRecordJSON(j))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// GetJob returns one tracked job by its key (owner/repo/pr/sha).
func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	rec, ok := h.uc.GetJob(r.Context(), key)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, jobRecordJSON(rec))
}

// RetryJob re-enqueues a previously tracked job without requiring a new
// GitHub webhook delivery.
func (h *Handler) RetryJob(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	job, err := h.uc.RetryJob(r.Context(), key)
	if err != nil {
		switch {
		case errors.Is(err, usecase.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, usecase.ErrDuplicate):
			http.Error(w, "job already in flight", http.StatusConflict)
		case errors.Is(err, usecase.ErrQueueFull):
			http.Error(w, "queue full", http.StatusServiceUnavailable)
		default:
			http.Error(w, "retry failed", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"requeued": true, "job": job.JobKey})
}

func jobRecordJSON(j queue.JobRecord) map[string]any {
	m := map[string]any{
		"job_key":     j.JobKey,
		"delivery_id": j.DeliveryID,
		"type":        j.Type,
		"owner":       j.Owner,
		"repo":        j.Repo,
		"pr":          j.PRNumber,
		"status":      j.Status,
		"error":       j.Error,
		"attempts":    j.Attempts,
		"enqueued_at": j.EnqueuedAt,
		"updated_at":  j.UpdatedAt,
	}
	if j.Job.ParentKey != "" {
		m["parent_key"] = j.Job.ParentKey
	}
	if j.Job.TriggerAuthor != "" {
		m["trigger_author"] = j.Job.TriggerAuthor
	}
	if j.FinishedAt != nil {
		m["finished_at"] = *j.FinishedAt
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
