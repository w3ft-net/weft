package service

import (
	"context"
	"testing"
	"time"
)

// TestIngestLimiter_Disabled pins that bytesPerSec/burst <= 0 turns the
// check off entirely — an operator setting either to 0 in SSM disables
// rate limiting without needing a separate feature flag.
func TestIngestLimiter_Disabled(t *testing.T) {
	l := newIngestLimiter()
	ctx := context.Background()
	start := time.Now()
	if err := l.wait(ctx, 1, 1<<30, 0, 0); err != nil {
		t.Fatalf("wait with bytesPerSec=0: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("disabled limiter should return immediately, took %v", elapsed)
	}
}

// TestIngestLimiter_WithinBurst_NoWait pins that a token starts with a
// full burst available — the first rows of a new stream aren't delayed.
func TestIngestLimiter_WithinBurst_NoWait(t *testing.T) {
	l := newIngestLimiter()
	ctx := context.Background()
	start := time.Now()
	if err := l.wait(ctx, 1, 100, 1000, 1000); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("a row within burst should not be delayed, took %v", elapsed)
	}
}

// TestIngestLimiter_OverBudget_Throttles pins the core behavior: once a
// token's burst is spent, the next row is delayed (not rejected) until
// the bucket refills at bytesPerSec.
func TestIngestLimiter_OverBudget_Throttles(t *testing.T) {
	l := newIngestLimiter()
	ctx := context.Background()
	const bytesPerSec, burst = int64(1000), int64(1000)

	// Spend the entire burst.
	if err := l.wait(ctx, 1, burst, bytesPerSec, burst); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	// The bucket is now empty; a further 500-byte row needs 500ms to
	// refill at 1000 bytes/sec. Assert it actually waited, with a
	// generous floor to absorb scheduling jitter.
	start := time.Now()
	if err := l.wait(ctx, 1, 500, bytesPerSec, burst); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("expected the second row to wait ~500ms for refill, took %v", elapsed)
	}
}

// TestIngestLimiter_RowLargerThanBurst_LetsThrough pins that a single
// row bigger than the configured burst is allowed through immediately
// rather than blocked forever — ReserveN can never succeed for it.
func TestIngestLimiter_RowLargerThanBurst_LetsThrough(t *testing.T) {
	l := newIngestLimiter()
	ctx := context.Background()
	start := time.Now()
	if err := l.wait(ctx, 1, 10_000, 100, 100); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("a row exceeding burst should pass through unthrottled, took %v", elapsed)
	}
}

// TestIngestLimiter_ContextCanceled_ReturnsPromptly pins that a
// canceled context aborts an in-progress wait instead of blocking for
// the full delay — a stream closing mid-throttle shouldn't leak the
// wait.
func TestIngestLimiter_ContextCanceled_ReturnsPromptly(t *testing.T) {
	l := newIngestLimiter()
	ctx, cancel := context.WithCancel(context.Background())
	const bytesPerSec, burst = int64(10), int64(10)
	if err := l.wait(context.Background(), 1, burst, bytesPerSec, burst); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- l.wait(ctx, 1, burst, bytesPerSec, burst) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("wait did not return promptly after context cancellation")
	}
}

// TestIngestLimiter_SeparateTokens_Independent pins that one token's
// exhausted budget never throttles a different token — the map is
// keyed on tokenID, not shared.
func TestIngestLimiter_SeparateTokens_Independent(t *testing.T) {
	l := newIngestLimiter()
	ctx := context.Background()
	const bytesPerSec, burst = int64(100), int64(100)

	if err := l.wait(ctx, 1, burst, bytesPerSec, burst); err != nil {
		t.Fatalf("token 1 wait: %v", err)
	}

	start := time.Now()
	if err := l.wait(ctx, 2, burst, bytesPerSec, burst); err != nil {
		t.Fatalf("token 2 wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("token 2 should be unaffected by token 1's exhausted budget, took %v", elapsed)
	}
}
