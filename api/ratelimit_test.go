package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/w3ft-net/weft/config"
)

func newTestLimiter(cfg *config.Runtime) *limiter {
	ptr := new(atomic.Pointer[config.Runtime])
	ptr.Store(cfg)
	return newLimiter(ptr)
}

// TestRateLimit_ExhaustsBudget verifies that the 6th request from the
// same IP gets 429 when the budget is 5 burst. This is the canonical
// magic-link email-spam attack scenario.
func TestRateLimit_ExhaustsBudget(t *testing.T) {
	cfg := config.Defaults()
	cfg.LoginRPM = 5
	cfg.LoginBurst = 5
	l := newTestLimiter(cfg)

	h := l.RateLimit(LimitLogin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var lastCode int
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("POST", "/api/auth/login", nil)
		req.RemoteAddr = "198.51.100.7:4242"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		lastCode = w.Code
		if i < 5 && w.Code != 200 {
			t.Fatalf("req %d: expected 200, got %d", i, w.Code)
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 on 6th request, got %d", lastCode)
	}
}

// TestRateLimit_SeparateIPsSeparateBuckets — one IP exhausting its
// budget must not starve other IPs.
func TestRateLimit_SeparateIPsSeparateBuckets(t *testing.T) {
	cfg := config.Defaults()
	cfg.LoginRPM = 2
	cfg.LoginBurst = 2
	l := newTestLimiter(cfg)
	h := l.RateLimit(LimitLogin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust IP A.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/api/auth/login", nil)
		req.RemoteAddr = "198.51.100.7:1"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// IP B should still pass.
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	req.RemoteAddr = "203.0.113.9:2"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("IP B starved by IP A's budget: got %d", w.Code)
	}
}

// TestRateLimit_LiveConfigChange — changing the pointer's config must
// take effect on the very next request for an already-tracked IP.
// This is the "override in the moment" property.
func TestRateLimit_LiveConfigChange(t *testing.T) {
	cfg := config.Defaults()
	cfg.LoginRPM = 100
	cfg.LoginBurst = 100
	ptr := new(atomic.Pointer[config.Runtime])
	ptr.Store(cfg)
	l := newLimiter(ptr)

	h := l.RateLimit(LimitLogin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Warm up the bucket for this IP.
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	req.RemoteAddr = "198.51.100.7:1"
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Tighten the budget to zero → everything should be denied.
	tight := config.Defaults()
	tight.LoginRPM = 1
	tight.LoginBurst = 1
	ptr.Store(tight)

	// First request under new config consumes the burst.
	h.ServeHTTP(httptest.NewRecorder(), req)
	// Second should 429.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after tightening config, got %d", w.Code)
	}
}

// TestBodyCap_OversizeReturns413 — POST beyond the cap gets 413 with
// no handler invocation.
func TestBodyCap_OversizeReturns413(t *testing.T) {
	handlerCalled := false
	h := BodyCap(100, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		handlerCalled = true
	}))

	body := make([]byte, 200)
	req := httptest.NewRequest("POST", "/x", nopReader(body))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
	if handlerCalled {
		t.Error("handler should not have been invoked on oversize body")
	}
}

type fakeReader struct{ data []byte }

func (f *fakeReader) Read(p []byte) (int, error) { return 0, nil }
func (f *fakeReader) Close() error               { return nil }

func nopReader(b []byte) *fakeReader { return &fakeReader{data: b} }
