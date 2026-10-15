package usecase

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
	"github.com/andikapradanaarif/elyoraa/internal/repository"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
)

// recordingStore captures webhook audit records for assertions.
type recordingStore struct {
	events   []*domain.WebhookEvent
	statuses []string
}

func (s *recordingStore) Record(_ context.Context, e *domain.WebhookEvent) error {
	s.events = append(s.events, e)
	return nil
}

func (s *recordingStore) UpdateStatus(_ context.Context, _, status, _ string) error {
	s.statuses = append(s.statuses, status)
	return nil
}

func (s *recordingStore) DeleteOlderThan(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func p2Config() *config.Config {
	return &config.Config{
		PRReviewBudgetMinutes: 15,
		PRLinesPerMinute:      100,
		SizeGateMode:          "deterministic",
		MentionRouter:         "keywords",
		PRSkipLabel:           "elyoraa:skip",
		BotHandle:             "elyoraa",
		MentionRateLimit:      100,
		MentionRateWindow:     time.Hour,
	}
}

func newP2Usecase(cfg *config.Config, store repository.WebhookStore, m *metrics.Registry) *WebhookUsecase {
	q := queue.New(1, nil, nil, 8)
	return NewWebhookUsecase(cfg, nil, dedup.New(), q, store, nil, m)
}

func autoJob() domain.Review {
	return domain.Review{
		Type: "review", Owner: "o", Repo: "r", PRNumber: 7,
		Author: "alice", HeadSHA: "abc", Origin: domain.OriginAuto,
	}
}

// A repo opt-out label must suppress pure automation.
func TestEnqueueReview_SkipLabelDrops(t *testing.T) {
	store := &recordingStore{}
	u := newP2Usecase(p2Config(), store, nil)

	j := autoJob()
	j.PRLabels = []string{"elyoraa:skip", "bug"}

	got, err := u.enqueueReview(context.Background(), j, nil, domain.RepoConfig{})
	if err != nil {
		t.Fatalf("skip drop should not error: %v", err)
	}
	if got.JobKey != "" && got.PendingCommentID != 0 {
		t.Fatal("job should be dropped before key assignment")
	}
	if len(store.statuses) == 0 || store.statuses[len(store.statuses)-1] != "dropped" {
		t.Fatalf("expected dropped status, got %v", store.statuses)
	}
}

// Explicit human intent overrides the label — a mention still runs. The
// mention is rate-limited here (limiter pre-consumed), which proves it
// passed the skip check: the label path returns nil error, not this one.
func TestEnqueueReview_SkipLabelOverriddenByMention(t *testing.T) {
	store := &recordingStore{}
	u := newP2Usecase(p2Config(), store, nil)

	u.limiter = newRateLimiter(1, time.Hour)
	if !u.limiter.allow("alice@o/r", time.Now()) {
		t.Fatal("limiter setup")
	} // slot consumed — the next mention hits ErrRateLimited

	j := autoJob()
	j.PRLabels = []string{"elyoraa:skip"}
	j.TriggerAuthor = "alice"
	j.TriggerComment = "@elyoraa rereview"
	j.Origin = domain.OriginMentionKeyword

	_, err := u.enqueueReview(context.Background(), j, nil, domain.RepoConfig{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("mention should bypass the label and reach the limiter; got %v", err)
	}
}

// Per-repo skip_label overrides the global one.
func TestEnqueueReview_RepoSkipLabelOverride(t *testing.T) {
	store := &recordingStore{}
	u := newP2Usecase(p2Config(), store, nil)

	j := autoJob()
	j.PRLabels = []string{"repo:skip"} // global label absent, repo label present

	_, err := u.enqueueReview(context.Background(), j, nil, domain.RepoConfig{SkipLabel: "repo:skip"})
	if err != nil {
		t.Fatalf("expected silent drop, got %v", err)
	}
	if len(store.statuses) == 0 || store.statuses[len(store.statuses)-1] != "dropped" {
		t.Fatalf("expected dropped status, got %v", store.statuses)
	}
}

// enabled: false drops any job before it costs anything.
func TestEnqueueReview_RepoDisabled(t *testing.T) {
	store := &recordingStore{}
	u := newP2Usecase(p2Config(), store, nil)

	disabled := false
	_, err := u.enqueueReview(context.Background(), autoJob(), nil, domain.RepoConfig{Enabled: &disabled})
	if err != nil {
		t.Fatalf("expected silent drop, got %v", err)
	}
	if len(store.statuses) == 0 || store.statuses[len(store.statuses)-1] != "dropped" {
		t.Fatalf("expected dropped status, got %v", store.statuses)
	}
}

// Reaction on a bot-authored comment is counted; on anyone else's, ignored.
func TestHandleReaction(t *testing.T) {
	cfg := p2Config()
	store := &recordingStore{}
	m := metrics.New()
	u := newP2Usecase(cfg, store, m)

	botComment := `{
		"action": "created",
		"reaction": {"content": "+1", "user": {"login": "alice"}},
		"repository": {"full_name": "o/r"},
		"comment": {"body": "review", "user": {"login": "elyoraa[bot]"}}
	}`
	if _, err := u.handleReaction(context.Background(), "d1", []byte(botComment)); err != nil {
		t.Fatalf("handleReaction: %v", err)
	}
	if len(store.events) != 1 || store.events[0].Status != "received" {
		t.Fatalf("bot reaction should be recorded as received: %+v", store.events)
	}

	humanComment := `{
		"action": "created",
		"reaction": {"content": "hooray", "user": {"login": "bob"}},
		"repository": {"full_name": "o/r"},
		"comment": {"body": "lgtm", "user": {"login": "carol"}}
	}`
	if _, err := u.handleReaction(context.Background(), "d2", []byte(humanComment)); err != nil {
		t.Fatalf("handleReaction: %v", err)
	}
	if store.events[1].Status != "ignored" {
		t.Fatalf("human reaction should be ignored: %s", store.events[1].Status)
	}

	// A marker in the body also identifies the comment as ours (covers
	// replies where login matching is unreliable).
	markerComment := `{
		"action": "created",
		"reaction": {"content": "rocket", "user": {"login": "dan"}},
		"repository": {"full_name": "o/r"},
		"comment": {"body": "<!-- elyoraa:respond --> done", "user": {"login": "someone"}}
	}`
	if _, err := u.handleReaction(context.Background(), "d3", []byte(markerComment)); err != nil {
		t.Fatalf("handleReaction: %v", err)
	}
	if store.events[2].Status != "received" {
		t.Fatalf("marker reaction should be received: %s", store.events[2].Status)
	}

	w := httptest.NewRecorder()
	m.Handler(w, nil)
	out := w.Body.String()
	for _, want := range []string{
		`elyoraa_reactions_total{reaction="+1",action="created"} 1`,
		`elyoraa_reactions_total{reaction="rocket",action="created"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, `reaction="hooray"`) {
		t.Fatal("ignored reaction must not be counted")
	}
}
