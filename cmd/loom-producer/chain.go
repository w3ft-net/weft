package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"time"

	weft "github.com/w3ft-net/weft-go"
)

// runChains simulates Loom's checkout flow: a user-facing request that
// genuinely hops api -> edge (payment/gateway) -> worker (async
// fulfillment), tagged end-to-end with one shared trace ID — the
// concrete, demoable counterpart to the ambient per-line solo trace
// IDs runStream already mints. Event names (checkout/
// checkout_processed/fulfillment) are distinct from ambient traffic
// (request/job_completed/etc.) so the onboarding tour's discovery
// lesson can grep for real chains without fishing through background
// noise.
func runChains(ctx context.Context, env string, api, edge, worker *os.File) {
	rng := newRNG("chain@" + env)
	for {
		if !sleep(ctx, time.Duration(5+rng.IntN(11))*time.Second) {
			return
		}
		if !runOneChain(ctx, rng, api, edge, worker) {
			return
		}
	}
}

// runOneChain emits one checkout -> checkout_processed -> fulfillment
// chain, sharing one trace ID across all three hops. Returns false
// only when ctx was cancelled mid-chain (the caller should stop); a
// write error on one hop is logged and treated as transient (the
// caller's next chain attempt, after its own interval, gets a fresh
// trace ID and tries again).
func runOneChain(ctx context.Context, rng *rand.Rand, api, edge, worker *os.File) bool {
	traceID := weft.NewTraceID()
	if err := writeChainLine(api, "INFO", traceID, checkoutContent(rng)); err != nil {
		log.Printf("loom-producer: chain write %s: %v", api.Name(), err)
		return true
	}

	if !sleep(ctx, time.Duration(50+rng.IntN(151))*time.Millisecond) {
		return false
	}
	if err := writeChainLine(edge, "INFO", traceID, checkoutProcessedContent(rng)); err != nil {
		log.Printf("loom-producer: chain write %s: %v", edge.Name(), err)
		return true
	}

	if !sleep(ctx, time.Duration(200+rng.IntN(1801))*time.Millisecond) {
		return false
	}
	level, content := fulfillmentContent(rng)
	if err := writeChainLine(worker, level, traceID, content); err != nil {
		log.Printf("loom-producer: chain write %s: %v", worker.Name(), err)
	}
	return true
}

// sleep waits out d, or returns false early if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func writeChainLine(f *os.File, level, traceID, content string) error {
	_, err := fmt.Fprintln(f, formatLine(level, traceID, content))
	return err
}

func checkoutContent(rng *rand.Rand) string {
	user := pickStr(rng, userIDs)
	dur := 20 + rng.IntN(181)
	return fmt.Sprintf("checkout method=POST path=/api/checkout status=200 dur=%dms user_id=%s", dur, user)
}

func checkoutProcessedContent(rng *rand.Rand) string {
	dur := 40 + rng.IntN(361)
	return fmt.Sprintf("checkout_processed payment_provider=stripe status=200 dur=%dms", dur)
}

// fulfillmentContent returns the worker hop's level and content. ~5%
// of chains end in an ERROR, giving the trace lesson a realistic
// "found the failure" payoff.
func fulfillmentContent(rng *rand.Rand) (level, content string) {
	order := pickStr(rng, orders)
	if rng.IntN(20) == 0 {
		return "ERROR", fmt.Sprintf("fulfillment order_id=%s reason=inventory_shortage", order)
	}
	dur := 200 + rng.IntN(1801)
	return "INFO", fmt.Sprintf("fulfillment order_id=%s duration_ms=%d", order, dur)
}
