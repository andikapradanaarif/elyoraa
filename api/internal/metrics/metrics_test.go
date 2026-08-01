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
