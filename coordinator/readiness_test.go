package coordinator

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func healthStatus(r *Readiness) int {
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	return rec.Code
}

// The load balancer's health check is the only readiness gate an ASG has, so
// /health must refuse (503) until the coordinator can serve, and stay 200
// after: a later dependency failure never makes the ASG replace a coordinator
// that is merely having a bad moment.
func TestReadiness_RefusesUntilReadyThenStaysUp(t *testing.T) {
	r := &Readiness{}
	if got := healthStatus(r); got != http.StatusServiceUnavailable {
		t.Fatalf("before MarkReady: status %d, want 503", got)
	}
	r.MarkReady()
	for i := 0; i < 3; i++ {
		if got := healthStatus(r); got != http.StatusOK {
			t.Fatalf("after MarkReady (check %d): status %d, want 200", i, got)
		}
	}
}
