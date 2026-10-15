package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
)

func testHandler(adminToken string) *Handler {
	cfg := &config.Config{
		AdminToken:  adminToken,
		MetricsPath: "/metrics",
	}
	return New(cfg, nil, metrics.New())
}

func TestRequireAdmin_UnconfiguredTokenIs404(t *testing.T) {
	h := testHandler("")
	mux := http.NewServeMux()
	h.Register(mux)

	for _, path := range []string{"/metrics", "/jobs", "/jobs/o/r/1/abc", "/jobs/retry/o/r/1/abc"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/jobs/retry/o/r/1/abc" {
			req = httptest.NewRequest(http.MethodPost, path, nil)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404 (unconfigured admin token must hide the route)", path, rec.Code)
		}
	}
}

func TestRequireAdmin_TokenEnforcement(t *testing.T) {
	h := testHandler("s3cret-token")
	mux := http.NewServeMux()
	h.Register(mux)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"basic scheme", "Basic s3cret-token", http.StatusUnauthorized},
		{"bare token", "s3cret-token", http.StatusUnauthorized},
		{"correct", "Bearer s3cret-token", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 should set WWW-Authenticate")
			}
		})
	}
}

func TestWebhookPaused_DropsEvents(t *testing.T) {
	h := testHandler("")
	h.cfg.Paused = true

	req := httptest.NewRequest(http.MethodPost, "/webhook/gh", nil)
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("paused webhook should acknowledge with 200, got %d", rec.Code)
	}
}

func TestWebhookPing_StillWorksWhenPaused(t *testing.T) {
	h := testHandler("")
	h.cfg.Paused = true

	req := httptest.NewRequest(http.MethodPost, "/webhook/gh", nil)
	req.Header.Set("X-GitHub-Event", "ping")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ping should be answered while paused, got %d", rec.Code)
	}
}
