package logdb

import (
	"context"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
)

// LogEvaluateEvent emits one structured line per bucket/shard FSM Evaluate
// call inside evaluateStateMachines, so per-shard convergence timing under
// real N=2 load can be checked without attaching a profiler
// (docs/plans/coordinator-scaling-live-testing.md phase 3). kind is
// "bucket" or "shard"; id is that bucket/shard's own id. err is nil for a
// clean Evaluate.
func LogEvaluateEvent(kind, id string, dur time.Duration, err error) {
	if err != nil {
		weftsdk.Infof(context.Background(), "evaluate_event kind=%s id=%s duration_ms=%d err=%q", kind, id, dur.Milliseconds(), err)
		return
	}
	weftsdk.Infof(context.Background(), "evaluate_event kind=%s id=%s duration_ms=%d", kind, id, dur.Milliseconds())
}

// LogEvaluateTick emits one line per evaluateStateMachines tick with the
// tick's total duration and how many buckets/shards it covered, so a slow
// or stalled tick is visible without summing every per-shard
// LogEvaluateEvent line by hand — this is the line that would confirm or
// disprove the known sequential-evaluation gap (one slow shard blocking
// convergence fleet-wide, docs/known-issues.md) under real load.
func LogEvaluateTick(buckets, shards int, dur time.Duration) {
	weftsdk.Infof(context.Background(), "evaluate_tick buckets=%d shards=%d duration_ms=%d", buckets, shards, dur.Milliseconds())
}
