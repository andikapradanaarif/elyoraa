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

	d.Done(*got)
	if d.Active() != 0 {
		t.Fatalf("expected 0 active after Done, got %d", d.Active())
	}
}

func TestDedupSplitSupersede(t *testing.T) {
	d := New()
	key := JobKey("o", "r", 1, "abc123")
	split := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d1", PendingCommentID: 9}

	ctxS, _, ok := d.TryStart(context.Background(), split)
	if !ok {
		t.Fatal("split job should register")
	}

	// A second split for the same key is a plain duplicate — dropped.
	dup := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), dup); ok {
		t.Fatal("duplicate split should be dropped")
	}

	// A review request supersedes the pending split proposal.
	review := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		DeliveryID: "d3"}
	_, got, ok := d.TryStart(context.Background(), review)
	if !ok {
		t.Fatal("review should supersede a pending split job")
	}
	if got.PendingCommentID != 9 {
		t.Fatalf("review should inherit the pending comment, got %d", got.PendingCommentID)
	}
	select {
	case <-ctxS.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("supersede should cancel the split job's context")
	}

	// The stale split instance must not bind over the new job's slot.
	if d.BindCancel(split, func() {}) {
		t.Fatal("superseded split instance should not bind")
	}
	if !d.BindCancel(*got, func() {}) {
		t.Fatal("superseding review should bind")
	}

	// The stale instance's Done must not clear the new job's slot.
	d.Done(split)
	if d.Active() != 1 {
		t.Fatalf("stale Done should not clear new job, active=%d", d.Active())
	}
	d.Done(*got)
	if d.Active() != 0 {
		t.Fatalf("expected 0 active, got %d", d.Active())
	}
}

func TestDedupSplitDoesNotSupersedeReview(t *testing.T) {
	d := New()
	key := JobKey("o", "r", 1, "abc123")
	review := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		DeliveryID: "d1"}
	if _, _, ok := d.TryStart(context.Background(), review); !ok {
		t.Fatal("review should register")
	}
	// A split job must not displace an in-flight review for the same SHA.
	split := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), split); ok {
		t.Fatal("split must not supersede a running review")
	}
}
