package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository"
	"github.com/andikapradanaarif/elyoraa/internal/repository/cache"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
)

// ctxCheckingStore flags any store call handed a nil context: TryStart
// returns one when it refuses a duplicate, and passing it to the PostgreSQL
// store used to panic inside the driver, swallowing the mention.
type ctxCheckingStore struct {
	t     *testing.T
	calls []string
}

func (s *ctxCheckingStore) Record(ctx context.Context, _ *domain.WebhookEvent) error {
	s.check("record", ctx)
	return nil
}

func (s *ctxCheckingStore) UpdateStatus(ctx context.Context, _, status, _ string) error {
	s.check("status:"+status, ctx)
	return nil
}

func (s *ctxCheckingStore) DeleteOlderThan(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (s *ctxCheckingStore) check(name string, ctx context.Context) {
	if ctx == nil {
		s.t.Errorf("%s got a nil context", name)
		return
	}
	s.calls = append(s.calls, name)
}

func TestEnqueueReview_DuplicateDropKeepsContext(t *testing.T) {
	cfg := &config.Config{}
	q := queue.New(1, nil, nil, 8)
	store := &ctxCheckingStore{t: t}
	u := NewWebhookUsecase(cfg, nil, dedup.New(), q, store, nil, nil)

	job := domain.Review{
		Owner: "o", Repo: "r", PRNumber: 7, HeadSHA: "abc",
		DeliveryID: "d-1", PendingCommentID: 123,
	}
	if _, err := u.enqueueReview(context.Background(), job, nil, domain.RepoConfig{}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	dup := job
	dup.DeliveryID = "d-2"
	if _, err := u.enqueueReview(context.Background(), dup, nil, domain.RepoConfig{}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate enqueue = %v, want ErrDuplicate", err)
	}

	found := false
	for _, c := range store.calls {
		if c == "status:dropped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("duplicate drop was not audited, calls: %v", store.calls)
	}
}

// WORKSPACE_BASE is the repo registry: a webhook for a repo with no bare
// clone is acknowledged and dropped silently — no job, no GitHub call (nil
// client proves it). elyoraa never clones on demand.
func TestEnqueueReview_UnregisteredRepoDropped(t *testing.T) {
	cfg := &config.Config{}
	q := queue.New(1, nil, nil, 8)
	c := cache.New(t.TempDir(), "", t.TempDir(), "")
	u := NewWebhookUsecase(cfg, nil, dedup.New(), q, repository.NewWebhookStore(nil), c, nil)

	job := domain.Review{
		Owner: "o", Repo: "r", PRNumber: 7, HeadSHA: "abc", DeliveryID: "d-0",
	}
	got, err := u.enqueueReview(context.Background(), job, nil, domain.RepoConfig{})
	if err != nil {
		t.Fatalf("unregistered repo must drop silently, got %v", err)
	}
	if got.JobKey != "" {
		t.Fatal("unregistered repo must not produce a job")
	}
}

// A mention past the author's rate limit must be dropped before any Crush
// work is enqueued — and before any GitHub call (nil client proves it).
func TestEnqueueReview_RateLimited(t *testing.T) {
	cfg := &config.Config{
		MentionRateLimit:  1,
		MentionRateWindow: time.Hour,
	}
	q := queue.New(1, nil, nil, 8)
	u := NewWebhookUsecase(cfg, nil, dedup.New(), q, repository.NewWebhookStore(nil), nil, nil)

	// Consume the single slot for this author+repo.
	if !u.limiter.allow("alice@o/r", time.Now()) {
		t.Fatal("setup: first slot should be free")
	}

	job := domain.Review{
		Owner: "o", Repo: "r", PRNumber: 7, HeadSHA: "abc",
		TriggerAuthor: "alice", TriggerComment: "@elyoraa review this",
		DeliveryID: "d-1",
	}
	_, err := u.enqueueReview(context.Background(), job, nil, domain.RepoConfig{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want ErrRateLimited", err)
	}
}

// Auto-reviews carry no TriggerAuthor and must bypass the mention limiter.
func TestEnqueueReview_AutoReviewNotRateLimited(t *testing.T) {
	cfg := &config.Config{
		MentionRateLimit:  1,
		MentionRateWindow: time.Hour,
	}
	q := queue.New(1, nil, nil, 8)
	u := NewWebhookUsecase(cfg, nil, dedup.New(), q, repository.NewWebhookStore(nil), nil, nil)
	u.limiter.allow("alice@o/r", time.Now())

	// PendingCommentID skips the GitHub call so the enqueue is exercised
	// end-to-end without a client.
	job := domain.Review{
		Owner: "o", Repo: "r", PRNumber: 7, HeadSHA: "abc",
		DeliveryID: "d-2", PendingCommentID: 123,
	}
	_, err := u.enqueueReview(context.Background(), job, nil, domain.RepoConfig{})
	if err != nil {
		t.Fatalf("auto-review must be enqueued, got %v", err)
	}
}

// The daily cap bounds runs per repo even for authorized triggers.
func TestEnqueueReview_DailyCap(t *testing.T) {
	cfg := &config.Config{DailyJobCapPerRepo: 1}
	q := queue.New(1, nil, nil, 8)
	u := NewWebhookUsecase(cfg, nil, dedup.New(), q, repository.NewWebhookStore(nil), nil, nil)

	// Seed one job that already started today in this repo.
	t0 := q.Tracker()
	t0.Enqueued(context.Background(), domain.Review{
		JobKey: "o/r/1/aaa", Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "aaa",
	})
	t0.Started(context.Background(), domain.Review{
		JobKey: "o/r/1/aaa", Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "aaa",
	})

	job := domain.Review{Owner: "o", Repo: "r", PRNumber: 2, HeadSHA: "bbb", DeliveryID: "d-3"}
	_, err := u.enqueueReview(context.Background(), job, nil, domain.RepoConfig{})
	if !errors.Is(err, ErrCapped) {
		t.Fatalf("got %v, want ErrCapped", err)
	}

	// A different repo has its own cap.
	other := domain.Review{
		Owner: "o", Repo: "other", PRNumber: 2, HeadSHA: "bbb",
		DeliveryID: "d-4", PendingCommentID: 123,
	}
	_, err = u.enqueueReview(context.Background(), other, nil, domain.RepoConfig{})
	if err != nil {
		t.Fatalf("different repo must be enqueued, got %v", err)
	}
}

// Jobs left in "processing" by a restart are marked failed so pending
// comments don't stay "in progress" forever.
func TestReconcileStuckJobs(t *testing.T) {
	cfg := &config.Config{CommentTimeout: time.Second}
	q := queue.New(1, nil, nil, 8)
	r := &ReviewUsecase{cfg: cfg, q: q}
	t0 := q.Tracker()

	stuck := domain.Review{
		JobKey: "o/r/9/ccc", Owner: "o", Repo: "r", PRNumber: 9, HeadSHA: "ccc",
		Type: "review",
	}
	t0.Enqueued(context.Background(), stuck)
	t0.Started(context.Background(), stuck)

	r.ReconcileStuckJobs(context.Background())

	rec, ok := t0.Get(context.Background(), "o/r/9/ccc")
	if !ok {
		t.Fatal("record missing")
	}
	if rec.Status != queue.JobStatusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if rec.Error == "" {
		t.Fatal("expected an interruption error on the record")
	}
}
