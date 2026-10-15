package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsHandler(t *testing.T) {
	m := New()
	m.IncQueueDepth()
	m.IncReviewTotal()
	m.IncReviewErrors()
	m.RecordDuration(2 * time.Second)

	rr := httptest.NewRecorder()
	m.Handler(rr, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	for _, name := range []string{
		"elyoraa_queue_depth",
		"elyoraa_reviews_total",
		"elyoraa_review_errors_total",
		"elyoraa_review_duration_seconds_sum",
		"elyoraa_review_duration_seconds_count",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("missing metric %s", name)
		}
	}
}

func TestLabeledCounters(t *testing.T) {
	m := New()
	m.IncJobOutcome("review", "completed")
	m.IncJobOutcome("review", "failed")
	m.IncJobOutcome("respond", "completed")
	m.IncRespondAction("request_review")
	m.IncGateVerdict("split")

	rr := httptest.NewRecorder()
	m.Handler(rr, nil)
	body := rr.Body.String()

	for _, line := range []string{
		`elyoraa_jobs_total{job_type="review",outcome="completed"} 1`,
		`elyoraa_jobs_total{job_type="review",outcome="failed"} 1`,
		`elyoraa_jobs_total{job_type="respond",outcome="completed"} 1`,
		`elyoraa_respond_actions_total{action="request_review"} 1`,
		`elyoraa_gate_verdicts_total{verdict="split"} 1`,
	} {
		if !strings.Contains(body, line) {
			t.Errorf("missing metric line %q in:\n%s", line, body)
		}
	}
	// Label values must be quoted — an unquoted label set is invalid
	// Prometheus exposition syntax and silently unscrapeable.
	if strings.Contains(body, `job_type=review`) {
		t.Errorf("unquoted label value in:\n%s", body)
	}
}
