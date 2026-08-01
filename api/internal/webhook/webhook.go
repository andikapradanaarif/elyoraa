package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	gh "github.com/andikapradanaarif/elyoraa/internal/github"
	"github.com/andikapradanaarif/elyoraa/internal/job"
	"github.com/andikapradanaarif/elyoraa/internal/queue"
	"github.com/andikapradanaarif/elyoraa/internal/startup"
	"github.com/google/go-github/v66/github"
)

// Handler handles GitHub webhooks.
type Handler struct {
	cfg *config.Config
	gh  *gh.Client
	q   *queue.Queue
	d   *dedup.Dedup
}

// New creates a webhook handler.
func New(cfg *config.Config, client *gh.Client, q *queue.Queue, d *dedup.Dedup) *Handler {
	return &Handler{cfg: cfg, gh: client, q: q, d: d}
}

// Register adds the webhook and health/metrics routes to the given mux.
func (h *Handler) Register(mux *http.ServeMux, metrics http.HandlerFunc) {
	mux.HandleFunc("/webhook", h.ServeHTTP)
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc(h.cfg.MetricsPath, metrics)
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
	if err := h.gh.VerifyWebhookSignature(payload, sig, h.cfg.GitHubWebhookSecret); err != nil {
		slog.Warn("webhook signature failed", "error", err)
		http.Error(w, "signature", http.StatusBadRequest)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "pull_request" {
		w.WriteHeader(http.StatusOK)
		return
	}

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		slog.Warn("parse webhook", "error", err)
		http.Error(w, "parse", http.StatusBadRequest)
		return
	}

	prEvent, ok := event.(*github.PullRequestEvent)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	action := prEvent.GetAction()
	if action != "opened" && action != "synchronize" && action != "reopened" {
		w.WriteHeader(http.StatusOK)
		return
	}

	pr := prEvent.GetPullRequest()
	if pr.GetDraft() {
		w.WriteHeader(http.StatusOK)
		return
	}

	author := pr.GetUser().GetLogin()
	repo := prEvent.GetRepo()
	if repo == nil || repo.GetFullName() == "" {
		slog.Warn("webhook missing repository")
		http.Error(w, "repository", http.StatusBadRequest)
		return
	}

	ownerRepo := strings.SplitN(repo.GetFullName(), "/", 2)
	if len(ownerRepo) != 2 {
		http.Error(w, "repository", http.StatusBadRequest)
		return
	}
	owner, repoName := ownerRepo[0], ownerRepo[1]

	reqJob := job.Review{
		Owner:          owner,
		Repo:           repoName,
		PRNumber:       pr.GetNumber(),
		HeadSHA:        pr.GetHead().GetSHA(),
		BaseSHA:        pr.GetBase().GetSHA(),
		InstallationID: prEvent.GetInstallation().GetID(),
		Author:         author,
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		EnqueuedAt:     time.Now(),
	}

	if isBot(&reqJob) {
		w.WriteHeader(http.StatusOK)
		return
	}

	reqJob.JobKey = dedup.JobKey(owner, repoName, reqJob.PRNumber, reqJob.HeadSHA)

	ctx, job, ok := h.d.TryStart(context.Background(), reqJob)
	if !ok {
		slog.Info("skipping duplicate review", "job", job.JobKey)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Post the in-progress comment if we don't already have one.
	if job.PendingCommentID == 0 {
		pendingBody := "Review in progress…\n\n<!-- elyoraa-in-progress -->"
		pendingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		id, err := h.gh.PostPendingComment(pendingCtx, job.InstallationID, job.Owner, job.Repo, job.PRNumber, pendingBody)
		cancel()
		if err != nil {
			slog.Warn("failed to post pending comment", "error", err)
		} else {
			job.PendingCommentID = id
		}
	}

	if !h.q.Submit(ctx, *job) {
		// Queue is full. Update the pending comment and release the dedup slot.
		slog.Warn("queue full, dropping job", "job", job.JobKey)
		if job.PendingCommentID != 0 {
			editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = h.gh.EditComment(editCtx, job.InstallationID, job.Owner, job.Repo, job.PendingCommentID,
				"Review queued, will run later.\n\n<!-- elyoraa-in-progress -->")
			cancel()
		}
		h.d.Done(job.JobKey)
	}

	w.WriteHeader(http.StatusOK)
}

func isBot(j *job.Review) bool {
	if j.Author == "" {
		return false
	}
	lower := strings.ToLower(j.Author)
	return strings.HasSuffix(lower, "[bot]") ||
		strings.HasPrefix(lower, "github-actions") ||
		lower == "dependabot" ||
		lower == "dependabot[bot]"
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
