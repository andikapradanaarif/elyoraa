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

// TestCanSupersedeTable pins the origin-strength supersession rule: a job
// supersedes the in-flight job on its key iff its origin ranks
// stronger-or-equal and the delivery ID differs. Automated work (auto)
// never supersedes.
func TestCanSupersedeTable(t *testing.T) {
	mk := func(typ, origin, delivery string) *domain.Review {
		return &domain.Review{Type: typ, Origin: origin, DeliveryID: delivery}
	}
	cases := []struct {
		name string
		cur  *domain.Review
		n    *domain.Review
		want bool
	}{
		// Redelivery: same delivery ID is a duplicate, never a supersede.
		{"redelivery same id", mk("review", domain.OriginAccepted, "d1"), mk("review", domain.OriginAccepted, "d1"), false},
		{"empty cur id can be superseded", mk("split", domain.OriginMentionKeyword, ""), mk("review", domain.OriginMentionKeyword, "d1"), true},

		// Automated work never supersedes — including a same-key auto
		// split hitting a gate that already became a review.
		{"auto review vs auto review", mk("review", domain.OriginAuto, "d1"), mk("review", domain.OriginAuto, "d2"), false},
		{"auto split vs auto review", mk("review", domain.OriginAuto, "d1"), mk("split", domain.OriginAuto, "d2"), false},
		{"auto review vs auto split", mk("split", domain.OriginAuto, "d1"), mk("review", domain.OriginAuto, "d2"), false},
		{"auto split vs gate", mk("gate", domain.OriginAuto, "d1"), mk("split", domain.OriginAuto, "d2"), false},

		// Keyword intent beats automated work.
		{"keyword review vs auto gate", mk("gate", domain.OriginAuto, "d1"), mk("review", domain.OriginMentionKeyword, "d2"), true},
		{"keyword review vs auto split", mk("split", domain.OriginAuto, "d1"), mk("review", domain.OriginMentionKeyword, "d2"), true},
		{"keyword review vs dispatched review", mk("review", domain.OriginDispatched, "d1"), mk("review", domain.OriginMentionKeyword, "d2"), false},

		// Dispatched (router/agent or exact slash command).
		{"dispatched review vs gate", mk("gate", domain.OriginAuto, "d1"), mk("review", domain.OriginDispatched, "d2"), true},
		{"dispatched split vs auto split", mk("split", domain.OriginAuto, "d1"), mk("split", domain.OriginDispatched, "d2"), true},
		{"dispatched review vs keyword split", mk("split", domain.OriginMentionKeyword, "d1"), mk("review", domain.OriginDispatched, "d2"), true},
		{"dispatched vs dispatched", mk("split", domain.OriginDispatched, "d1"), mk("review", domain.OriginDispatched, "d2"), true},
		{"dispatched vs accepted", mk("split", domain.OriginAccepted, "d1"), mk("review", domain.OriginDispatched, "d2"), false},

		// Acceptance is the strongest intent.
		{"accepted split vs auto proposal", mk("split", domain.OriginAuto, "d1"), mk("split", domain.OriginAccepted, "d2"), true},
		{"accepted split vs auto review", mk("review", domain.OriginAuto, "d1"), mk("split", domain.OriginAccepted, "d2"), true},
		{"keyword review vs accepted split", mk("split", domain.OriginAccepted, "d1"), mk("review", domain.OriginMentionKeyword, "d2"), false},

		// Issue analyses and respond jobs sit outside the origin table.
		{"issue never superseded", mk("issue", domain.OriginMentionKeyword, "d1"), mk("issue", domain.OriginAccepted, "d2"), false},
		{"respond vs review keyspace", mk("respond", "", "d1"), mk("review", domain.OriginAccepted, "d2"), false},
	}
	for _, tc := range cases {
		if got := canSupersede(tc.cur, tc.n); got != tc.want {
			t.Errorf("%s: canSupersede=%v, want %v", tc.name, got, tc.want)
		}
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

	// A second automated split for the same key is a plain duplicate — dropped.
	dup := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), dup); ok {
		t.Fatal("duplicate split should be dropped")
	}

	// An explicit review request supersedes the pending split proposal.
	review := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		DeliveryID: "d3", Origin: domain.OriginMentionKeyword}
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

func TestDedupSplitCreateSupersedesProposal(t *testing.T) {
	d := New()
	key := JobKey("o", "r", 1, "abc123")
	propose := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d1", PendingCommentID: 9}

	ctxS, _, ok := d.TryStart(context.Background(), propose)
	if !ok {
		t.Fatal("split proposal should register")
	}

	// An accepted proposal supersedes its own pending proposal for the same
	// SHA — "create the issues" must not be dropped as a duplicate while
	// the proposal job is still running.
	create := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", SplitCreate: true, Origin: domain.OriginAccepted, DeliveryID: "d2"}
	_, got, ok := d.TryStart(context.Background(), create)
	if !ok {
		t.Fatal("split-create should supersede a pending proposal")
	}
	if got.PendingCommentID != 9 {
		t.Fatalf("split-create should inherit the pending comment, got %d", got.PendingCommentID)
	}
	select {
	case <-ctxS.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("supersede should cancel the proposal's context")
	}

	// But a proposal must never displace a running create job.
	propose2 := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d3"}
	if _, _, ok := d.TryStart(context.Background(), propose2); ok {
		t.Fatal("proposal must not supersede a running split-create job")
	}
	// Nor may another acceptance replace it mid-creation — re-running
	// issue creation risks duplicates.
	create2 := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", SplitCreate: true, Origin: domain.OriginAccepted, DeliveryID: "d4"}
	if _, _, ok := d.TryStart(context.Background(), create2); ok {
		t.Fatal("a second acceptance must not supersede a running split-create job")
	}
}

func TestDedupRespondDoesNotCancelReview(t *testing.T) {
	d := New()
	review := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123",
		JobKey: JobKey("o", "r", 1, "abc123"), DeliveryID: "d1"}

	ctxR, _, ok := d.TryStart(context.Background(), review)
	if !ok {
		t.Fatal("review should register")
	}

	// A respond job on the same PR gets its own slot — it must not cancel
	// the running review, and two mentions must both run.
	resp := domain.Review{Owner: "o", Repo: "r", PRNumber: 1,
		JobKey: RespondKey("o", "r", 1, 7001), Type: "respond", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), resp); !ok {
		t.Fatal("respond job should register alongside a running review")
	}
	resp2 := domain.Review{Owner: "o", Repo: "r", PRNumber: 1,
		JobKey: RespondKey("o", "r", 1, 7002), Type: "respond", DeliveryID: "d3"}
	if _, _, ok := d.TryStart(context.Background(), resp2); !ok {
		t.Fatal("a second mention should get its own slot")
	}
	if d.Active() != 3 {
		t.Fatalf("expected 3 active jobs, got %d", d.Active())
	}
	select {
	case <-ctxR.Done():
		t.Fatal("respond jobs must not cancel a running review")
	case <-time.After(50 * time.Millisecond):
	}

	// A normal job for a new SHA cancels same-PR jobs but not respond keys.
	review2 := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "def456",
		JobKey: JobKey("o", "r", 1, "def456"), DeliveryID: "d4"}
	if _, _, ok := d.TryStart(context.Background(), review2); !ok {
		t.Fatal("new-sha review should register")
	}
	if d.Active() != 3 {
		t.Fatalf("expected 3 active jobs (respond keys survive), got %d", d.Active())
	}
}

func TestDedupRespondSupersedeOnEdit(t *testing.T) {
	d := New()
	key := RespondKey("o", "r", 1, 7001)
	first := domain.Review{Owner: "o", Repo: "r", PRNumber: 1,
		JobKey: key, Type: "respond", DeliveryID: "d1"}

	ctx1, _, ok := d.TryStart(context.Background(), first)
	if !ok {
		t.Fatal("first respond job should register")
	}

	// An edited comment reuses the trigger ID → same key → the newer text
	// wins.
	edit := domain.Review{Owner: "o", Repo: "r", PRNumber: 1,
		JobKey: key, Type: "respond", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), edit); !ok {
		t.Fatal("edited comment should supersede the in-flight respond job")
	}
	select {
	case <-ctx1.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("edit should cancel the previous respond context")
	}
}

func TestDedupRespondRedeliveryDropped(t *testing.T) {
	d := New()
	key := RespondKey("o", "r", 1, 7001)
	first := domain.Review{Owner: "o", Repo: "r", PRNumber: 1,
		JobKey: key, Type: "respond", DeliveryID: "d1"}

	ctx1, _, ok := d.TryStart(context.Background(), first)
	if !ok {
		t.Fatal("first respond job should register")
	}

	// GitHub redelivery reuses the delivery GUID — a duplicate of an
	// in-flight job must be dropped, not restarted. Without the delivery-ID
	// check the redelivery would cancel the running job, then the stale
	// run's Done() could clear the replacement's slot.
	dup := domain.Review{Owner: "o", Repo: "r", PRNumber: 1,
		JobKey: key, Type: "respond", DeliveryID: "d1"}
	if _, _, ok := d.TryStart(context.Background(), dup); ok {
		t.Fatal("respond redelivery with the same delivery ID must be dropped")
	}
	select {
	case <-ctx1.Done():
		t.Fatal("redelivery must not cancel the in-flight job")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDedupGateSupersededByExplicitRequest(t *testing.T) {
	d := New()
	key := JobKey("o", "r", 1, "abc123")
	gate := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "gate", DeliveryID: "d1", PendingCommentID: 7}

	ctxG, _, ok := d.TryStart(context.Background(), gate)
	if !ok {
		t.Fatal("gate job should register")
	}

	// A duplicate gate for the same SHA is just a duplicate — dropped.
	gateDup := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "gate", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), gateDup); ok {
		t.Fatal("duplicate gate should be dropped")
	}

	// A mention-dispatched review for the same SHA supersedes the triage —
	// the human's explicit request must not be swallowed mid-triage.
	review := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		DeliveryID: "d3", Origin: domain.OriginDispatched}
	_, got, ok := d.TryStart(context.Background(), review)
	if !ok {
		t.Fatal("explicit review should supersede a running gate")
	}
	if got.PendingCommentID != 7 {
		t.Fatalf("review should inherit the pending comment, got %d", got.PendingCommentID)
	}
	select {
	case <-ctxG.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("supersede should cancel the gate's context")
	}
}

// TestDedupGateTransitionUpdatesSlot covers review.go's UpdateJob call: once
// a gate verdict transitions the job to a review, the slot's type changes and
// supersession rules must see the review — a mere automated split must not
// kill it, while explicit intent still can.
func TestDedupGateTransitionUpdatesSlot(t *testing.T) {
	d := New()
	key := JobKey("o", "r", 1, "abc123")
	gate := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "gate", DeliveryID: "d1"}

	if _, _, ok := d.TryStart(context.Background(), gate); !ok {
		t.Fatal("gate job should register")
	}

	// Gate verdict says "review" — Process syncs the dedup slot.
	d.UpdateJob(key, func(sj *domain.Review) { sj.Type = "review" })

	// An automated split (pull_request re-delivery, stale enqueue) must not
	// replace the review the gate committed to.
	autoSplit := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), autoSplit); ok {
		t.Fatal("automated split must not supersede a gate-turned-review")
	}

	// Explicit intent still can.
	dispatched := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", Origin: domain.OriginAccepted, SplitCreate: true, DeliveryID: "d3"}
	if _, _, ok := d.TryStart(context.Background(), dispatched); !ok {
		t.Fatal("accepted split should supersede a gate-turned-review")
	}
}

func TestDedupSplitDoesNotSupersedeReview(t *testing.T) {
	d := New()
	key := JobKey("o", "r", 1, "abc123")
	review := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		DeliveryID: "d1", Origin: domain.OriginMentionKeyword}
	if _, _, ok := d.TryStart(context.Background(), review); !ok {
		t.Fatal("review should register")
	}
	// An automated split job must not displace an in-flight review for the
	// same SHA.
	split := domain.Review{Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc123", JobKey: key,
		Type: "split", DeliveryID: "d2"}
	if _, _, ok := d.TryStart(context.Background(), split); ok {
		t.Fatal("split must not supersede a running review")
	}
}
