package dedup

import (
	"context"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

func TestDedup(t *testing.T) {
	d := New()
	j1 := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: JobKey("o", "r", 1, "abc123")}

	ctx1, _, ok := d.TryStart(context.Background(), j1)
	if !ok {
		t.Fatalf("first TryStart should succeed")
	}

	_, _, ok = d.TryStart(context.Background(), j1)
	if ok {
		t.Fatalf("duplicate TryStart should fail")
	}

	j2 := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "def456", JobKey: JobKey("o", "r", 1, "def456"), PendingCommentID: 42}
	_, got, ok := d.TryStart(context.Background(), j2)
	if !ok {
		t.Fatalf("replacement TryStart should succeed")
	}
	if got.PendingCommentID != 42 {
		t.Fatalf("new job should keep its own pending comment")
	}
	if d.Active() != 1 {
		t.Fatalf("expected 1 active, got %d", d.Active())
	}

	// The previous job context should be cancelled.
	select {
	case <-ctx1.Done():
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("replacement should cancel previous job context")
	}

	d.Done(j2.JobKey)
	if d.Active() != 0 {
		t.Fatalf("expected 0 active after Done, got %d", d.Active())
	}
}
